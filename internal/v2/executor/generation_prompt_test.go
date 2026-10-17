package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestGenerationCustomPromptKeepsFixedOutputContract(t *testing.T) {
	for _, customPrompt := range []string{"请生成中文问候。\n不要提问。", strings.Repeat("<&>", 2730), strings.Repeat("\n", 8191) + "x"} {
		cpa, host, account := generationFixture()
		if _, err := cpa.GenerateCandidatesWithPrompt(context.Background(), account, "fixture-model", 2, customPrompt); err != nil {
			t.Fatal(err)
		}
		var body struct {
			Instructions string `json:"instructions"`
			Input        []struct {
				Role    string `json:"role"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"input"`
		}
		if err := json.Unmarshal(host.requestBody, &body); err != nil || !strings.Contains(body.Instructions, "exactly 2") || !strings.Contains(body.Instructions, "512 UTF-8 bytes") || !strings.Contains(body.Instructions, "JSON array") || len(body.Input) != 1 || body.Input[0].Role != "user" || len(body.Input[0].Content) != 1 || body.Input[0].Content[0].Text != customPrompt {
			t.Fatal("custom prompt or output constraints lost", err)
		}
		if host.executes != 1 || host.closes != 1 || host.request.AuthID != account.ID || base64.StdEncoding.EncodedLen(len(host.requestBody)) > 65536 {
			t.Fatal("prompt bypassed pinning or bridge size bound")
		}
	}
}

func TestGenerationCustomPromptInvalidBeforeHostCalls(t *testing.T) {
	for _, prompt := range []string{string([]byte{0xff}), "nul\x00", "control\x1b", strings.Repeat("x", 8193)} {
		cpa, host, account := generationFixture()
		if _, err := cpa.GenerateCandidatesWithPrompt(context.Background(), account, "fixture-model", 2, prompt); err == nil || err.Error() != "generation_request_invalid" || host.executes != 0 {
			t.Fatal("invalid prompt reached model", err)
		}
	}
}
