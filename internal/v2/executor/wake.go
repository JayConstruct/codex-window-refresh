package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

func (c *CPA) WakeAvailable() bool {
	if c.Host == nil {
		return false
	}
	capability, ok := c.Host.(interface{ SupportsWake() bool })
	return ok && capability.SupportsWake()
}

func (c *CPA) ExecuteWake(ctx context.Context, account store.Account, model, prompt string) (resultErr error) {
	if model == "" || len(model) > 200 || !utf8.ValidString(model) || strings.IndexFunc(model, func(value rune) bool { return unicode.IsSpace(value) || unicode.IsControl(value) }) >= 0 || strings.TrimSpace(prompt) == "" || !policy.ValidGenerationPrompt(prompt) {
		return errors.New("wake_request_invalid")
	}
	if generationSnapshotProvider(account) != "codex" || !c.WakeAvailable() {
		return errors.New("generation_not_supported")
	}
	instanceHost, pinInstance := c.Host.(interface{ Instance() string })
	instance := ""
	if pinInstance {
		instance = instanceHost.Instance()
		if instance == "" {
			return errors.New("account_identity_unavailable")
		}
	}
	if ctx.Err() != nil {
		return errors.New("wake_request_invalid")
	}
	if err := c.generationAccount(ctx, account); err != nil {
		switch err.Error() {
		case "account_not_found", "account_binding_changed", "account_disabled", "account_identity_unavailable":
			return err
		}
		return errors.New("account_identity_unavailable")
	}
	if ctx.Err() != nil {
		return errors.New("wake_request_invalid")
	}
	payload := map[string]any{
		"model": model, "stream": true, "store": false,
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]string{"type": "input_text", "text": prompt}}}},
	}
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if encoder.Encode(payload) != nil {
		return errors.New("wake_request_invalid")
	}
	body := buffer.Bytes()
	defer clear(body)
	request := generationRequest{
		Serial:        true,
		EntryProtocol: "openai-response", ExitProtocol: "codex", Model: model,
		Stream: true, Body: body, ForcedProvider: "codex", AuthID: account.ID,
		Headers: map[string][]string{"Content-Type": {"application/json"}}, Query: map[string][]string{},
	}
	unknown := func() error { return errors.New("wake_delivery_unknown") }
	if !c.WakeAvailable() {
		return errors.New("generation_not_supported")
	}
	if pinInstance && instanceHost.Instance() != instance {
		return errors.New("account_binding_changed")
	}
	var opened generationOpened
	err := c.Host.Call(ctx, "host.model.execute_stream", request, &opened)
	if opened.StreamID == "" && err != nil && err.Error() == "model_budget_busy" {
		return errors.New("wake_model_busy")
	}
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
			resultErr = unknown()
		}
	}()
	if err != nil || opened.StatusCode < 200 || opened.StatusCode >= 300 {
		return unknown()
	}
	stream := candidateStream{discardText: true}
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
			return nil
		}
	}
	return unknown()
}
