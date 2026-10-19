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

const wakeInstructions = "You are a concise coding assistant. Answer the user's request directly. No tools, file access, or command execution are available; do not claim otherwise."

func (c *CPA) WakeAvailable() bool {
	if c.Host == nil {
		return false
	}
	capability, ok := c.Host.(interface{ SupportsWake() bool })
	return ok && capability.SupportsWake()
}

func (c *CPA) ExecuteWake(ctx context.Context, account store.Account, model, prompt string) (resultErr error) {
	_, err := c.ExecuteWakeWithResponse(ctx, account, model, prompt)
	return err
}

func (c *CPA) ExecuteWakeWithResponse(ctx context.Context, account store.Account, model, prompt string) (response store.WakeResponse, resultErr error) {
	response.Stage = "preflight"
	resultErr = c.executeWake(ctx, account, model, prompt, &response)
	if resultErr != nil {
		response.ErrorCode = resultErr.Error()
	}
	if errors.Is(resultErr, context.DeadlineExceeded) {
		response.ErrorCode = "context_deadline_exceeded"
	}
	if errors.Is(resultErr, context.Canceled) {
		response.ErrorCode = "context_canceled"
	}
	if ctx.Err() != nil {
		response.ErrorCode = "context_canceled"
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			response.ErrorCode = "context_deadline_exceeded"
		}
	}
	return store.NormalizeWakeResponse(response), resultErr
}

func (c *CPA) executeWake(ctx context.Context, account store.Account, model, prompt string, response *store.WakeResponse) (resultErr error) {
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
	if err := c.wakeAccount(ctx, account); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return errors.New("wake_request_invalid")
	}
	payload := map[string]any{
		"model": model, "stream": true, "store": false,
		"instructions": wakeInstructions, "tool_choice": "none",
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
	response.Stage = "open"
	err := c.Host.Call(ctx, "host.model.execute_stream", request, &opened)
	response.HTTPStatus = opened.StatusCode
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
			response.CloseFailed = closeErr != nil
			if closeErr != nil {
				response.Stage = "close"
			} else {
				response.Stage = "interrupted"
			}
			resultErr = unknown()
		}
	}()
	recorder := wakeRecorder{response: response, prompt: prompt}
	defer recorder.finish()
	if err != nil {
		return unknown()
	}
	response.Stage = "read"
	stream := candidateStream{discardText: true, observe: recorder.capture}
	for reads := 0; reads < 1024; reads++ {
		var chunk generationRead
		if c.Host.Call(ctx, "host.model.stream_read", generationHandle{opened.StreamID}, &chunk) != nil {
			response.ErrorMessage = "CPA 模型响应流读取失败，未保存原始错误。"
			return unknown()
		}
		response.Bytes += len(chunk.Payload)
		if !stream.accept(chunk.Payload) || chunk.Error != "" || ctx.Err() != nil {
			if chunk.Error != "" && response.ErrorMessage == "" {
				response.ErrorMessage = "CPA 上报模型响应流异常，未保存原始错误。"
			}
			return unknown()
		}
		if chunk.Done {
			if !stream.finish() {
				return unknown()
			}
			if opened.StatusCode < 200 || opened.StatusCode >= 300 {
				return unknown()
			}
			response.Stage = "completed"
			response.Completed = true
			return nil
		}
	}
	return unknown()
}
