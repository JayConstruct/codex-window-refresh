package cpa

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-window-refresh/internal/core"
)

func TestGenerationCapturesSplitTextAndCompletedOutput(t *testing.T) {
	values := []string{"Hello.", "Hi.", "Ready.", "Yes.", "OK."}
	payload, _ := json.Marshal(values)
	for _, terminalOnly := range []bool{false, true} {
		terminal := map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{map[string]any{"content": []any{map[string]any{"type": "output_text", "text": string(payload)}}}}}}
		terminalRaw, _ := json.Marshal(terminal)
		chunks := []streamRead{}
		if !terminalOnly {
			for _, part := range []string{string(payload[:10]), string(payload[10:])} {
				raw, _ := json.Marshal(map[string]string{"type": "response.output_text.delta", "delta": part})
				chunks = append(chunks, streamRead{Payload: append(append([]byte("data: "), raw...), []byte("\n\n")...)})
			}
		}
		chunks = append(chunks, streamRead{Payload: append(append([]byte("data: "), terminalRaw...), []byte("\n\n")...), Done: true})
		callback := &fakeCallbacks{upstream: "real", chunks: chunks}
		host := NewHost(callback, nil)
		accounts, err := host.Accounts(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		result, candidates, err := host.GeneratePrompts(context.Background(), core.ExactRequest{AccountID: accounts[0].ID, Generation: accounts[0].Generation, OperationID: "generation", Model: "model", Prompt: core.GenerationPrompt(5)}, 5)
		if err != nil || result.Disposition != core.SendCompleted || len(candidates) != 5 || candidates[0] != "Hello." || !callback.closed {
			t.Fatalf("generation parsing failed: %v %+v %v", result, candidates, err)
		}
	}
}

func TestGeneratedCandidatesAreBoundedUniqueAndNeverExposeRawErrors(t *testing.T) {
	for _, text := range []string{`{}`, `["duplicate","duplicate","a","b","c"]`, `["a","b","c","d","line\nbreak"]`, `["a","b","c","d","` + strings.Repeat("x", 257) + `"]`, strings.Repeat("x", 17000)} {
		if _, err := NormalizePromptCandidates(text, 5); err == nil || err.Error() != "prompt_candidates_invalid" {
			t.Fatal("invalid generated output accepted or leaked")
		}
	}
	values, err := NormalizePromptCandidates("```json\n[\" a \",\"b\",\"c\",\"d\",\"e\"]\n```", 5)
	if err != nil || values[0] != "a" {
		t.Fatal("safe generated values not normalized")
	}
}
