package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"codex-window-refresh/internal/v2/store"
)

const generationStreamLimit = 1 << 20
const generationTextLimit = 16 << 10

type generationRequest struct {
	EntryProtocol  string              `json:"entry_protocol"`
	ExitProtocol   string              `json:"exit_protocol"`
	Model          string              `json:"model"`
	Stream         bool                `json:"stream"`
	Body           []byte              `json:"body"`
	Headers        map[string][]string `json:"headers"`
	Query          map[string][]string `json:"query"`
	ForcedProvider string              `json:"forced_provider"`
	AuthID         string              `json:"auth_id"`
}

type generationOpened struct {
	StatusCode int    `json:"status_code"`
	StreamID   string `json:"stream_id"`
}

type generationRead struct {
	Payload []byte `json:"payload"`
	Error   string `json:"error"`
	Done    bool   `json:"done"`
}

type generationHandle struct {
	StreamID string `json:"stream_id"`
}

func (c *CPA) GenerationAvailable() bool {
	if c.Host == nil {
		return false
	}
	if capability, ok := c.Host.(interface{ SupportsPromptGeneration() bool }); ok {
		return capability.SupportsPromptGeneration()
	}
	return true
}

func (c *CPA) generationAccount(ctx context.Context, account store.Account) error {
	if account.Status == "disabled" {
		return errors.New("account_disabled")
	}
	if !store.ValidID(account.ID) || account.AuthIndex == "" || account.Binding == "" || account.Status != "available" {
		return errors.New("account_identity_unavailable")
	}
	accounts, err := c.Accounts(ctx)
	if err != nil {
		return errors.New("account_identity_unavailable")
	}
	found := false
	for _, current := range accounts {
		if current.ID != account.ID {
			continue
		}
		found = true
		if current.Status == "disabled" {
			return errors.New("account_disabled")
		}
		if current.Status != "available" || current.AuthIndex == "" {
			return errors.New("account_identity_unavailable")
		}
		if current.AuthIndex != account.AuthIndex {
			return errors.New("account_binding_changed")
		}
	}
	if !found {
		return errors.New("account_not_found")
	}
	cred, err := c.readBindingFor(ctx, account, true)
	if err != nil {
		switch err.Error() {
		case "account_binding_changed", "account_disabled", "account_identity_unavailable":
			return errors.New(err.Error())
		}
		return errors.New("account_identity_unavailable")
	}
	if account.Binding != bindingID(account, cred.Upstream) {
		return errors.New("account_binding_changed")
	}
	return nil
}

func (c *CPA) GenerateCandidates(ctx context.Context, account store.Account, model string, count int) (values []string, resultErr error) {
	if model == "" || len(model) > 200 || !utf8.ValidString(model) || strings.IndexFunc(model, func(value rune) bool { return unicode.IsSpace(value) || unicode.IsControl(value) }) >= 0 || count < 1 || count > 20 {
		return nil, errors.New("generation_request_invalid")
	}
	if c.Host == nil {
		return nil, errors.New("account_identity_unavailable")
	}
	if err := c.generationAccount(ctx, account); err != nil {
		return nil, err
	}
	if !c.GenerationAvailable() {
		return nil, errors.New("generation_not_supported")
	}
	if ctx.Err() != nil {
		return nil, errors.New("generation_request_invalid")
	}
	prompt := fmt.Sprintf("Generate exactly %d distinct, short, harmless standalone sentences suitable as minimal API connectivity checks. Return only a JSON array of strings, without explanations or formatting. Each string must be nonempty, on one line, and at most 512 UTF-8 bytes. Do not include secrets, personal information, instructions, or requests for external actions.", count)
	body, err := json.Marshal(map[string]any{
		"model": model, "stream": true, "store": false,
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": prompt}}}},
	})
	if err != nil {
		return nil, errors.New("generation_request_invalid")
	}
	defer clear(body)
	request := generationRequest{
		EntryProtocol: "openai-response", ExitProtocol: "codex", Model: model,
		Stream: true, Body: body, ForcedProvider: "codex", AuthID: account.ID,
		Headers: map[string][]string{"Content-Type": {"application/json"}}, Query: map[string][]string{},
	}
	unknown := func() ([]string, error) { return nil, errors.New("generation_delivery_unknown") }
	var opened generationOpened
	err = c.Host.Call(ctx, "host.model.execute_stream", request, &opened)
	if opened.StreamID == "" {
		return unknown()
	}
	var closeOnce sync.Once
	var closeErr error
	closeModel := func() {
		closeOnce.Do(func() {
			closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			closeErr = c.Host.Call(closeCtx, "host.model.stream_close", generationHandle{opened.StreamID}, &struct{}{})
		})
	}
	stopClose, closeJoined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(closeJoined)
		select {
		case <-ctx.Done():
			closeModel()
		case <-stopClose:
		}
	}()
	defer func() {
		close(stopClose)
		<-closeJoined
		closeModel()
		if closeErr != nil || ctx.Err() != nil {
			values, resultErr = unknown()
		}
	}()
	if err != nil || opened.StatusCode < 200 || opened.StatusCode >= 300 {
		return unknown()
	}
	stream := candidateStream{}
	for reads := 0; reads < 1024; reads++ {
		var chunk generationRead
		if c.Host.Call(ctx, "host.model.stream_read", generationHandle{opened.StreamID}, &chunk) != nil {
			return unknown()
		}
		if !stream.accept(chunk.Payload) || chunk.Error != "" || ctx.Err() != nil {
			return unknown()
		}
		if chunk.Done {
			if !stream.finish() {
				return unknown()
			}
			return candidateValues(stream.text(), count)
		}
	}
	return unknown()
}

type candidateStream struct {
	pending   []byte
	total     int
	deltas    string
	completed string
	terminal  bool
	failed    bool
}

func (stream *candidateStream) accept(payload []byte) bool {
	stream.total += len(payload)
	if stream.total > generationStreamLimit {
		return false
	}
	if generationLineBoundary(stream.pending, payload) {
		stream.pending = append(stream.pending, '\n')
	}
	stream.pending = append(stream.pending, payload...)
	stream.pending = bytes.ReplaceAll(stream.pending, []byte("\r\n"), []byte("\n"))
	for {
		index := bytes.Index(stream.pending, []byte("\n\n"))
		if index < 0 {
			break
		}
		stream.processFrame(stream.pending[:index])
		stream.pending = stream.pending[index+2:]
	}
	trimmed := bytes.TrimSpace(stream.pending)
	if json.Valid(trimmed) {
		stream.process(trimmed)
		stream.pending = nil
	} else if data, ok := generationEventData(stream.pending); ok && (json.Valid(data) || string(data) == "[DONE]") {
		stream.process(data)
		stream.pending = nil
	}
	return !stream.failed
}

func (stream *candidateStream) finish() bool {
	stream.processFrame(stream.pending)
	return stream.terminal && !stream.failed
}

func (stream *candidateStream) text() string {
	if stream.completed != "" {
		return stream.completed
	}
	return stream.deltas
}

func (stream *candidateStream) processFrame(frame []byte) {
	if data, ok := generationEventData(frame); ok {
		stream.process(data)
	} else if !generationMetadataOnly(frame) && len(bytes.TrimSpace(frame)) > 0 {
		stream.process(bytes.TrimSpace(frame))
	}
}

func generationHasError(raw json.RawMessage) bool {
	return len(raw) != 0 && string(bytes.TrimSpace(raw)) != "null"
}

func (stream *candidateStream) process(raw []byte) {
	if string(raw) == "[DONE]" {
		return
	}
	var event struct {
		Type     string          `json:"type"`
		Delta    string          `json:"delta"`
		Error    json.RawMessage `json:"error"`
		Response struct {
			Status            string          `json:"status"`
			Error             json.RawMessage `json:"error"`
			IncompleteDetails json.RawMessage `json:"incomplete_details"`
			Output            []struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"output"`
		} `json:"response"`
	}
	if !utf8.Valid(raw) || json.Unmarshal(raw, &event) != nil || event.Type == "" || generationHasError(event.Error) || generationHasError(event.Response.Error) {
		stream.failed = true
		return
	}
	switch event.Type {
	case "error", "response.failed", "response.incomplete", "response.refusal.delta", "response.refusal.done":
		stream.failed = true
	case "response.output_text.delta":
		if stream.terminal || len(stream.deltas)+len(event.Delta) > generationTextLimit {
			stream.failed = true
			return
		}
		stream.deltas += event.Delta
	case "response.completed":
		if stream.terminal || (event.Response.Status != "" && event.Response.Status != "completed") || generationHasError(event.Response.IncompleteDetails) {
			stream.failed = true
			return
		}
		stream.terminal = true
		for _, output := range event.Response.Output {
			for _, content := range output.Content {
				if content.Type == "refusal" {
					stream.failed = true
				}
				if content.Type != "output_text" {
					continue
				}
				if len(stream.completed)+len(content.Text) > generationTextLimit {
					stream.failed = true
					return
				}
				stream.completed += content.Text
			}
		}
		if stream.completed != "" && stream.deltas != "" && stream.completed != stream.deltas {
			stream.failed = true
		}
	}
}

func candidateValues(text string, count int) ([]string, error) {
	invalid := func() ([]string, error) { return nil, errors.New("generation_candidates_invalid") }
	if len(text) > generationTextLimit || !utf8.ValidString(text) || count < 1 || count > 20 {
		return invalid()
	}
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, "```") {
		first, rest, found := strings.Cut(text, "\n")
		if !found || (strings.TrimSpace(first) != "```json" && strings.TrimSpace(first) != "```") || !strings.HasSuffix(rest, "```") {
			return invalid()
		}
		text = strings.TrimSpace(strings.TrimSuffix(rest, "```"))
	}
	var values []string
	if json.Unmarshal([]byte(text), &values) != nil || len(values) != count {
		return invalid()
	}
	seen := map[string]bool{}
	for index, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || len(value) > 512 || !utf8.ValidString(value) || seen[value] || strings.IndexFunc(value, func(value rune) bool { return unicode.IsControl(value) || value == '\u2028' || value == '\u2029' }) >= 0 {
			return invalid()
		}
		values[index] = value
		seen[value] = true
	}
	return values, nil
}

func generationEventData(frame []byte) ([]byte, bool) {
	var data []string
	for _, line := range strings.Split(string(frame), "\n") {
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if len(data) == 0 {
		return nil, false
	}
	return []byte(strings.Join(data, "\n")), true
}

func generationMetadataOnly(frame []byte) bool {
	for _, line := range strings.Split(string(frame), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line != "" && !strings.HasPrefix(line, ":") && !strings.HasPrefix(line, "event:") && !strings.HasPrefix(line, "id:") && !strings.HasPrefix(line, "retry:") {
			return false
		}
	}
	return true
}

func generationLineBoundary(pending, payload []byte) bool {
	if len(pending) == 0 || bytes.HasSuffix(pending, []byte("\n")) || len(payload) == 0 {
		return false
	}
	startsField := false
	for _, prefix := range []string{"data:", "event:", "id:", "retry:", ":"} {
		startsField = startsField || bytes.HasPrefix(payload, []byte(prefix))
	}
	if !startsField {
		return false
	}
	if generationMetadataOnly(pending) {
		return true
	}
	if !bytes.HasPrefix(payload, []byte("data:")) {
		return false
	}
	data, ok := generationEventData(pending)
	if !ok {
		return false
	}
	inString, escaped := false, false
	for _, value := range data {
		if escaped {
			escaped = false
		} else if inString && value == '\\' {
			escaped = true
		} else if value == '"' {
			inString = !inString
		}
	}
	return !inString
}
