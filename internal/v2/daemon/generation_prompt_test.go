package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/policy"
	"codex-window-refresh/internal/v2/store"
)

type customPromptSource struct {
	generationSource
	prompts chan string
}

func (source *customPromptSource) GenerateCandidatesWithPrompt(ctx context.Context, account store.Account, model string, count int, prompt string) ([]string, error) {
	source.calls.Add(1)
	source.prompts <- prompt
	return []string{"第一句", "第二句"}, nil
}

func TestGenerationPromptAPIUsesSnapshotNotLaterSettingsAndDoesNotLeak(t *testing.T) {
	source := &customPromptSource{prompts: make(chan string, 2)}
	_, storage := generationService(t, &source.generationSource)
	service, err := New(storage, source, Options{QueryConcurrency: 1, QueryTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	response := request(service, "GET", "/api/v2/overview", "", "")
	if !strings.Contains(response.Body.String(), `"editable_generation_prompt":true`) {
		t.Fatal(response.Body.String())
	}
	prompt := "仅生成自然的中文晚安问候。\n不要提问。"
	body, _ := json.Marshal(store.GenerationRequest{AccountID: "generation-account", Model: "test-model", Count: 2, Prompt: prompt})
	response = request(service, "POST", "/api/v2/prompt-generations", string(body), "custom-prompt")
	var accepted store.Generation
	if response.Code != 202 || json.Unmarshal(response.Body.Bytes(), &accepted) != nil || strings.Contains(response.Body.String(), "prompt") {
		t.Fatal(response.Code, response.Body.String())
	}
	settings := policy.DefaultModel()
	settings.GenerationPrompt = "later changed prompt"
	if _, err = storage.SaveModelSettings(context.Background(), 0, settings); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); service.generate(ctx) }()
	defer func() { cancel(); <-done }()
	awaitGeneration(t, storage, accepted.ID, "completed")
	select {
	case actual := <-source.prompts:
		if actual != prompt {
			t.Fatal("worker did not use submitted prompt snapshot")
		}
	case <-time.After(time.Second):
		t.Fatal("worker did not receive custom prompt")
	}
	response = request(service, "POST", "/api/v2/prompt-generations", string(body), "custom-prompt")
	if response.Code != 202 || source.calls.Load() != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
	changed := strings.Replace(string(body), "不要提问", "只用英语", 1)
	response = request(service, "POST", "/api/v2/prompt-generations", changed, "custom-prompt")
	if response.Code != 409 || source.calls.Load() != 1 {
		t.Fatal(response.Code, response.Body.String())
	}
}

func TestGenerationPromptAPIValidationAndUnsupportedSource(t *testing.T) {
	source := &generationSource{generate: func(context.Context, store.Account, string, int) ([]string, error) {
		return nil, errors.New("must not execute")
	}}
	service, storage := generationService(t, source)
	for _, prompt := range []string{"nul\x00", "control\x1b", strings.Repeat("x", 8193)} {
		body, _ := json.Marshal(store.GenerationRequest{AccountID: "generation-account", Model: "test-model", Count: 2, Prompt: prompt})
		response := request(service, "POST", "/api/v2/prompt-generations", string(body), "invalid")
		if response.Code != 400 {
			t.Fatal(response.Code, response.Body.String())
		}
	}
	raw := strings.Replace(generationBody, `"count":2`, "\"count\":2,\"prompt\":\""+string([]byte{0xff})+"\"", 1)
	if response := request(service, "POST", "/api/v2/prompt-generations", raw, "invalid-utf8"); response.Code != 400 {
		t.Fatal(response.Code, response.Body.String())
	}
	body := strings.Replace(generationBody, `"count":2`, `"count":2,"prompt":"valid custom prompt"`, 1)
	response := request(service, "POST", "/api/v2/prompt-generations", body, "unsupported")
	if response.Code != 503 || !strings.Contains(response.Body.String(), "generation_prompt_unavailable") {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, exists, err := storage.LookupGeneration(context.Background(), "unsupported", store.GenerationRequest{AccountID: "generation-account", Model: "test-model", Count: 2, Prompt: "valid custom prompt"}); err != nil || exists || source.calls.Load() != 0 {
		t.Fatal("unsupported prompt was accepted", exists, err)
	}
}
