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
	var observation *core.QuotaObservation
	if o, err := ParseQuota(quotaHeaders(opened.Headers), read, time.Now().UTC(), core.ObservationExecution, baseline); err == nil {
		observation = &o
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
				observation = &o
			}
		}
	}
	for {
		var chunk streamRead
		if err := h.callback.Call(ctx, "host.model.stream_read", streamRequest{opened.StreamID}, &chunk); err != nil {
			return core.SendResult{Disposition: core.SendUnknown, Observation: operationObservation(observation, req.OperationID), ErrorCode: "model_stream_read_failed"}, nil
		}
		if len(chunk.Payload) > 0 {
			pending = append(pending, chunk.Payload...)
			if len(pending) > 1<<20 {
				return core.SendResult{Disposition: core.SendUnknown, ErrorCode: "model_stream_frame_too_large"}, nil
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
			}
		}
		if chunk.Error != "" {
			streamFailed = true
		}
		if chunk.Done || chunk.Error != "" {
			break
		}
		if ctx.Err() != nil {
			return core.SendResult{Disposition: core.SendUnknown, ErrorCode: "model_stream_canceled"}, nil
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
	return core.SendResult{Disposition: disposition, Observation: operationObservation(observation, req.OperationID), ErrorCode: code}, nil
}
func parseSSEEvent(event []byte, process func([]byte)) {
	data := []string{}
	for _, line := range strings.Split(string(event), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) > 0 {
		process([]byte(strings.Join(data, "\n")))
	} else if json.Valid(bytes.TrimSpace(event)) {
		process(bytes.TrimSpace(event))
	}
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
