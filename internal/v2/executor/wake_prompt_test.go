package executor

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
)

func TestWakeCodingInstructionsKeepSavedUserPromptsAndNoClientImpersonation(t *testing.T) {
	for _, prompt := range []string{
		"请用一句话解释 Go 中的 defer。",
		"  Keep my exact text.\r\n\t<code> & 中文 🙂  ",
		"Use a friendly greeting instead of programming advice.",
	} {
		t.Run(prompt, func(t *testing.T) {
			cpa, host, account := wakeFixture()
			if err := cpa.ExecuteWake(context.Background(), account, "fixture-model", prompt); err != nil {
				t.Fatal(err)
			}
			var body struct {
				Instructions string `json:"instructions"`
				ToolChoice   string `json:"tool_choice"`
				Input        []struct {
					Role    string `json:"role"`
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
				} `json:"input"`
			}
			if err := json.Unmarshal(host.requestBody, &body); err != nil {
				t.Fatal(err)
			}
			if body.Instructions != wakeInstructions || body.ToolChoice != "none" || len(body.Input) != 1 || body.Input[0].Role != "user" || len(body.Input[0].Content) != 1 || body.Input[0].Content[0].Type != "input_text" || body.Input[0].Content[0].Text != prompt {
				t.Fatal("coding instructions changed the saved prompt or allowed tools")
			}
			if !reflect.DeepEqual(host.request.Headers, map[string][]string{"Content-Type": {"application/json"}}) || len(host.request.Query) != 0 || host.executes != 1 || host.closes != 1 || !host.request.Serial {
				t.Fatal("wake added client identity, session parameters, retries, or changed stream ownership")
			}
		})
	}
}

func TestWakeCodingInstructionsDoNotChangeCandidateGeneration(t *testing.T) {
	cpa, host, account := generationFixture()
	if _, err := cpa.GenerateCandidates(context.Background(), account, "fixture-model", 2); err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(host.requestBody, &body) != nil {
		t.Fatal("invalid candidate request")
	}
	if _, exists := body["instructions"]; exists {
		t.Fatal("wake instructions leaked into candidate generation")
	}
	if _, exists := body["tool_choice"]; exists {
		t.Fatal("wake tool settings leaked into candidate generation")
	}
}
