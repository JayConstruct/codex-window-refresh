package cpa

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"codex-window-refresh/internal/core"
)

func (h *Host) SendExact(ctx context.Context, req core.ExactRequest) (core.SendResult, error) {
	return h.sendExact(ctx, req, nil)
}

func (h *Host) sendExact(ctx context.Context, req core.ExactRequest, text *generationText) (core.SendResult, error) {
	if _, err := h.validate(ctx, req.AccountID, req.Generation); err != nil {
		return core.SendResult{Disposition: core.SendDefinitelyNotSent, ErrorCode: err.Error()}, nil
	}
	if req.OperationID == "" || req.Model == "" || req.Prompt == "" {
		return core.SendResult{Disposition: core.SendDefinitelyNotSent, ErrorCode: "invalid_exact_request"}, nil
	}
	body, err := json.Marshal(map[string]any{"model": req.Model, "stream": true, "store": false, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": req.Prompt}}}}})
	if err != nil {
		return core.SendResult{Disposition: core.SendDefinitelyNotSent, ErrorCode: "invalid_exact_request"}, nil
	}
	request := modelRequest{EntryProtocol: "openai-response", ExitProtocol: "codex", Model: req.Model, Stream: true, Body: body, ForcedProvider: "codex", AuthID: string(req.AccountID), Headers: map[string][]string{"Content-Type": {"application/json"}}, Query: map[string][]string{}}
	var opened streamResponse
	if err := h.callback.Call(ctx, "host.model.execute_stream", request, &opened); err != nil {
		// A transport may decode the successful stream handle and then report a
		// concurrent cancellation. Release that handle even though delivery is unknown.
		if opened.StreamID != "" {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_ = h.callback.Call(closeCtx, "host.model.stream_close", streamRequest{opened.StreamID}, &struct{}{})
			cancel()
		}
		return core.SendResult{Disposition: core.SendUnknown, ErrorCode: "model_callback_failed"}, nil
	}
	if opened.StreamID == "" {
		return core.SendResult{Disposition: core.SendUnknown, ErrorCode: "model_stream_missing"}, nil
	}
	closeModel := func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = h.callback.Call(closeCtx, "host.model.stream_close", streamRequest{opened.StreamID}, &struct{}{})
	}
	// A synchronous native RPC cannot propagate Go context cancellation into an
	// already blocked host stream read. Explicit close cancels the host-owned
	// upstream stream. Always join this watcher before returning: no callback may
	// survive the engine shutdown and native library unload.
	stopClose, closeJoined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(closeJoined)
		select {
		case <-ctx.Done():
			closeModel()
		case <-stopClose:
		}
	}()
	defer func() { close(stopClose); <-closeJoined; closeModel() }()
	read := core.QuotaRead{AccountID: req.AccountID, Generation: req.Generation, ReadStartedSeq: req.ReadStartedSeq}
	h.mu.RLock()
	baseline := h.baselines[req.AccountID]
	h.mu.RUnlock()
	var observations []core.QuotaObservation
	remember := func(o core.QuotaObservation) {
		operationObservation(&o, req.OperationID)
		observations = retainSendObservations(observations, o)
	}
	result := func(disposition core.SendDisposition, code string) core.SendResult {
		out := core.SendResult{Disposition: disposition, Observations: observations, ErrorCode: code}
		if len(observations) > 0 {
			latest := observations[len(observations)-1]
			out.Observation = &latest
		}
		return out
	}
	if o, err := ParseQuota(quotaHeaders(opened.Headers), read, time.Now().UTC(), core.ObservationExecution, baseline); err == nil {
		remember(o)
	}
	completed := false
	streamFailed := false
	pending := []byte{}
	process := func(data []byte) {
		if text != nil {
			text.accept(data)
		}
		var event struct {
			Type     string `json:"type"`
			Response struct {
				Status string `json:"status"`
			} `json:"response"`
		}
		if json.Unmarshal(data, &event) != nil {
			return
		}
		switch event.Type {
		case "response.completed":
			if event.Response.Status == "" || event.Response.Status == "completed" {
				completed = true
			} else {
				streamFailed = true
			}
		case "error", "response.failed", "response.incomplete":
			streamFailed = true
		case "codex.rate_limits":
			if o, err := ParseQuota(data, read, time.Now().UTC(), core.ObservationExecution, baseline); err == nil {
				remember(o)
			}
		}
	}
	for {
		var chunk streamRead
		if err := h.callback.Call(ctx, "host.model.stream_read", streamRequest{opened.StreamID}, &chunk); err != nil {
			return result(core.SendUnknown, "model_stream_read_failed"), nil
		}
		if len(chunk.Payload) > 0 {
			// The native Codex executor returns scanner lines as separate
			// callback payloads, without their terminating newline. Keep SSE
			// metadata separate from a following native data line while still
			// retaining genuinely fragmented JSON and multiline data events.
			if nativeSSELineBoundary(pending, chunk.Payload) {
				pending = append(pending, '\n')
			}
			pending = append(pending, chunk.Payload...)
			if len(pending) > 1<<20 {
				return result(core.SendUnknown, "model_stream_frame_too_large"), nil
			}
			pending = bytes.ReplaceAll(pending, []byte("\r\n"), []byte("\n"))
			for {
				index := bytes.Index(pending, []byte("\n\n"))
				if index < 0 {
					break
				}
				event := pending[:index]
				pending = pending[index+2:]
				parseSSEEvent(event, process)
			}
			// Native Codex quota frames can be complete JSON without SSE framing.
			if json.Valid(bytes.TrimSpace(pending)) {
				process(bytes.TrimSpace(pending))
				pending = nil
			} else if data, ok := sseEventData(pending); ok && json.Valid(data) {
				// A complete native data line needs no blank SSE separator.
				// Never consume an incomplete JSON payload: subsequent callback
				// chunks may finish it or add another data line to the event.
				process(data)
				pending = nil
			}
		}
		if chunk.Error != "" {
			streamFailed = true
		}
		if chunk.Done || chunk.Error != "" {
			break
		}
		if ctx.Err() != nil {
			return result(core.SendUnknown, "model_stream_canceled"), nil
		}
	}
	if len(bytes.TrimSpace(pending)) > 0 {
		parseSSEEvent(pending, process)
	}
	disposition := core.SendUnknown
	code := "model_stream_missing_completion"
	if completed && !streamFailed && opened.StatusCode >= 200 && opened.StatusCode < 300 {
		disposition = core.SendCompleted
		code = ""
	} else if streamFailed {
		code = "model_stream_failed"
	}
	return result(disposition, code), nil
}

// Keep original messages which still own a latest field or positive activation
// proof, bounding storage to five regardless of stream length. Keeping shape and receive
// time avoids fabricating same-message joint-reset or recovery evidence.
func retainSendObservations(observations []core.QuotaObservation, next core.QuotaObservation) []core.QuotaObservation {
	observations = append(observations, next)
	var seen uint8
	kept := make([]core.QuotaObservation, 0, 5)
	for index := len(observations) - 1; index >= 0; index-- {
		o := observations[index]
		var fields uint8
		if o.Permission != nil {
			fields |= 1
		}
		if o.FiveHour != nil {
			fields |= 2
			if o.FiveHour.State == core.WindowActive {
				fields |= 8
			}
		}
		if o.Weekly != nil {
			fields |= 4
			if o.Weekly.State == core.WindowActive {
				fields |= 16
			}
		}
		if fields & ^seen != 0 {
			kept = append(kept, o)
		}
		seen |= fields
	}
	for left, right := 0, len(kept)-1; left < right; left, right = left+1, right-1 {
		kept[left], kept[right] = kept[right], kept[left]
	}
	return kept
}

func parseSSEEvent(event []byte, process func([]byte)) {
	if data, ok := sseEventData(event); ok {
		process(data)
	} else if json.Valid(bytes.TrimSpace(event)) {
		process(bytes.TrimSpace(event))
	}
}

func sseEventData(event []byte) ([]byte, bool) {
	data := []string{}
	for _, line := range strings.Split(string(event), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) > 0 {
		return []byte(strings.Join(data, "\n")), true
	}
	return nil, false
}

func sseMetadataOnly(event []byte) bool {
	if len(bytes.TrimSpace(event)) == 0 {
		return false
	}
	for _, line := range strings.Split(string(event), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" && !strings.HasPrefix(line, ":") && !strings.HasPrefix(line, "event:") && !strings.HasPrefix(line, "id:") && !strings.HasPrefix(line, "retry:") {
			return false
		}
	}
	return true
}

func nativeSSELineBoundary(pending, payload []byte) bool {
	if len(pending) == 0 || bytes.HasSuffix(pending, []byte("\n")) || !startsSSEField(payload) {
		return false
	}
	if sseMetadataOnly(pending) {
		return true
	}
	// A native multiline data event also loses the newlines between data
	// fields. Only insert one outside a JSON string, where literal newlines
	// are valid JSON whitespace. A fragmented JSON string may itself resume
	// with "data:" and must retain those exact bytes.
	if !bytes.HasPrefix(payload, []byte("data:")) {
		return false
	}
	data, ok := sseEventData(pending)
	if !ok {
		return false
	}
	inString, escaped := false, false
	for _, ch := range data {
		if escaped {
			escaped = false
		} else if inString && ch == '\\' {
			escaped = true
		} else if ch == '"' {
			inString = !inString
		}
	}
	return !inString
}

func startsSSEField(payload []byte) bool {
	for _, prefix := range [][]byte{[]byte("data:"), []byte("event:"), []byte("id:"), []byte("retry:"), []byte(":")} {
		if bytes.HasPrefix(payload, prefix) {
			return true
		}
	}
	return false
}
func operationObservation(o *core.QuotaObservation, id core.OperationID) *core.QuotaObservation {
	if o == nil {
		return nil
	}
	if o.FiveHour != nil {
		o.FiveHour.OperationID = id
	}
	if o.Weekly != nil {
		o.Weekly.OperationID = id
	}
	return o
}
