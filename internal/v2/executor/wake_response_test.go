package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestWakeResponseCapturesSuccessfulTextAndUpstreamFailureWithoutResend(t *testing.T) {
	for _, failure := range []bool{false, true} {
		cpa, host, account := wakeFixture()
		prompt := "private prompt content"
		if failure {
			host.opened.StatusCode = 429
			host.chunks = []generationRead{{Payload: []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"Try later Bearer private-access-token api_key=private-key private prompt content https://user:secret@proxy.example"}}}`), Done: true}}
		} else {
			host.chunks = []generationRead{{Payload: generationCompleted("Hello " + prompt + " sk-private-token user@example.com"), Done: true}}
		}
		response, err := cpa.ExecuteWakeWithResponse(context.Background(), account, "actual-model", prompt)
		if host.executes != 1 || host.closes != 1 || host.reads != 1 {
			t.Fatal("recording changed execution count", host.executes, host.closes, host.reads)
		}
		if failure {
			if err == nil || err.Error() != "wake_delivery_unknown" || response.HTTPStatus != 429 || response.UpstreamErrorCode != "rate_limit_exceeded" || response.Status != "failed" || response.Completed {
				t.Fatal(response, err)
			}
		} else if err != nil || !response.Completed || !strings.Contains(response.ResponseText, "Hello") {
			t.Fatal(response, err)
		}
		raw, _ := json.Marshal(response)
		for _, secret := range []string{prompt, "sk-private-token", "user@example.com", "private-access-token", "private-key", "user:secret"} {
			if strings.Contains(string(raw), secret) {
				t.Fatal("diagnostic leaked secret", secret)
			}
		}
	}
}

func TestWakeResponseKeepsPartialTextOnStreamFailureAndCloseFailure(t *testing.T) {
	cpa, host, account := wakeFixture()
	host.chunks = []generationRead{{Payload: []byte(`{"type":"response.output_text.delta","delta":"Partial safe response"}`)}, {Error: "private stream failure", Done: true}}
	host.closeErr = errors.New("private close failure")
	response, err := cpa.ExecuteWakeWithResponse(context.Background(), account, "actual-model", "hello")
	if err == nil || response.ResponseText != "Partial safe response" || !response.CloseFailed || response.Stage != "close" || strings.Contains(response.ErrorMessage, "private") || host.executes != 1 {
		t.Fatal(response, err)
	}
}
