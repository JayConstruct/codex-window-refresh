package executor

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type wakeHost struct {
	*generationHost
	supported bool
}

func (host *wakeHost) SupportsWake() bool { return host.supported }

func TestWakeKnownUnsentNativeBudgetDoesNotBecomeDeliveryUnknown(t *testing.T) {
	cpa, host, account := wakeFixture()
	host.opened = generationOpened{}
	host.executeErr = errors.New("model_budget_busy")
	if err := cpa.ExecuteWake(context.Background(), account, "fixture-model", "hello"); err == nil || err.Error() != "wake_model_busy" || host.executes != 1 || host.closes != 0 || !host.request.Serial {
		t.Fatal("native pre-send budget refusal was not distinguished", err)
	}
}

type wakeWithoutGenerationHost struct{ *wakeHost }

func (*wakeWithoutGenerationHost) SupportsPromptGeneration() bool { return false }

type wakeChangingHost struct {
	*wakeHost
	instance       string
	changeMethod   string
	changeInstance bool
}

func (host *wakeChangingHost) Instance() string { return host.instance }

func (host *wakeChangingHost) Call(ctx context.Context, method string, request, response any) error {
	err := host.generationHost.Call(ctx, method, request, response)
	if method == host.changeMethod {
		if host.changeInstance {
			host.instance = "replacement-instance"
		} else {
			host.supported = false
		}
	}
	return err
}

func TestWakeRechecksCapabilityAndPinsBridgeInstanceAfterIdentityReads(t *testing.T) {
	for _, method := range []string{"host.auth.list", "host.auth.get_runtime", "host.auth.get"} {
		for _, changeInstance := range []bool{false, true} {
			name := method + "/capability"
			if changeInstance {
				name = method + "/instance"
			}
			t.Run(name, func(t *testing.T) {
				cpa, host, account := wakeFixture()
				changing := &wakeChangingHost{wakeHost: &wakeHost{host, true}, instance: "original-instance", changeMethod: method, changeInstance: changeInstance}
				cpa.Host = changing
				want := "generation_not_supported"
				if changeInstance {
					want = "account_binding_changed"
				}
				err := cpa.ExecuteWake(context.Background(), account, "fixture-model", "Hello.")
				if err == nil || err.Error() != want || host.executes != 0 || host.reads != 0 || host.closes != 0 {
					t.Fatalf("err=%v execute=%d read=%d close=%d", err, host.executes, host.reads, host.closes)
				}
			})
		}
	}
	cpa, host, account := wakeFixture()
	cpa.Host = &wakeChangingHost{wakeHost: &wakeHost{host, true}, instance: ""}
	if err := cpa.ExecuteWake(context.Background(), account, "fixture-model", "Hello."); err == nil || err.Error() != "account_identity_unavailable" || host.lists != 0 || host.executes != 0 {
		t.Fatalf("empty cached instance err=%v list=%d execute=%d", err, host.lists, host.executes)
	}
	cpa, host, account = wakeFixture()
	cpa.Host = &wakeChangingHost{wakeHost: &wakeHost{host, true}, instance: "stable-instance"}
	if err := cpa.ExecuteWake(context.Background(), account, "fixture-model", "Hello."); err != nil || host.executes != 1 || host.closes != 1 {
		t.Fatalf("stable instance err=%v execute=%d close=%d", err, host.executes, host.closes)
	}
}

func wakeFixture() (*CPA, *generationHost, store.Account) {
	cpa, host, account := generationFixture()
	cpa.Host = &wakeHost{host, true}
	host.chunks = []generationRead{{Payload: generationCompleted("你好，测试成功。"), Done: true}}
	return cpa, host, account
}

func TestWakeCapabilityIsCachedIndependentAndFailClosed(t *testing.T) {
	cpa, host, _ := generationFixture()
	if cpa.WakeAvailable() || (&CPA{}).WakeAvailable() {
		t.Fatal("legacy capability allowed waking")
	}
	cpa.Host = &wakeHost{host, true}
	if !cpa.WakeAvailable() {
		t.Fatal("negotiated wake support missing")
	}
	cpa.Host = &wakeHost{host, false}
	if cpa.WakeAvailable() || host.lists != 0 || host.runtimes != 0 || host.credentials != 0 || host.executes != 0 {
		t.Fatal("capability lookup made callbacks or ignored explicit rejection")
	}
	cpa.Host = &wakeWithoutGenerationHost{&wakeHost{host, true}}
	if !cpa.WakeAvailable() || cpa.GenerationAvailable() {
		t.Fatal("wake capability coupled to candidate generation")
	}
}

func TestWakePinsCodexWithOnlyExactPromptAndDiscardsOutput(t *testing.T) {
	cpa, host, account := wakeFixture()
	prompt := "用一句话回复 <你好> & 欢迎。\n保持简短。"
	if err := cpa.ExecuteWake(context.Background(), account, "gpt-fixture", prompt); err != nil {
		t.Fatal(err)
	}
	request := host.request
	if request.AuthID != account.ID || request.ForcedProvider != "codex" || request.EntryProtocol != "openai-response" || request.ExitProtocol != "codex" || request.Model != "gpt-fixture" || !request.Stream || host.executes != 1 || host.closes != 1 || host.reads != 1 || !host.closeContextOK {
		t.Fatalf("unsafe request=%+v model=%d read=%d close=%d", request, host.executes, host.reads, host.closes)
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(host.requestBody, &body) != nil || len(body) != 4 || string(body["stream"]) != "true" || string(body["store"]) != "false" || string(body["model"]) != `"gpt-fixture"` {
		t.Fatalf("unexpected body=%s", host.requestBody)
	}
	var input []struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(body["input"], &input) != nil || len(input) != 1 || input[0].Role != "user" || len(input[0].Content) != 1 || input[0].Content[0].Type != "input_text" || input[0].Content[0].Text != prompt {
		t.Fatalf("prompt altered: %s", body["input"])
	}
	if strings.Contains(string(host.requestBody), "JSON array") || strings.Contains(string(host.requestBody), "fixture-secret") || strings.Contains(string(host.requestBody), "你好，测试成功") {
		t.Fatal("candidate constraints, credentials or output entered request")
	}
	stream := candidateStream{discardText: true}
	if !stream.accept([]byte(`{"type":"response.output_text.delta","delta":"private output"}`)) || !stream.accept(generationCompleted("different private output")) || !stream.finish() || stream.text() != "" || stream.deltas != "" || stream.completed != "" {
		t.Fatal("wake parser retained output or required JSON candidates")
	}
}

func TestWakePreflightRejectsInvalidInputAndIdentityWithoutSending(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*CPA, *generationHost, *store.Account, *string, *string)
		code   string
	}{
		{"empty model", func(_ *CPA, _ *generationHost, _ *store.Account, model, _ *string) { *model = "" }, "wake_request_invalid"},
		{"long model", func(_ *CPA, _ *generationHost, _ *store.Account, model, _ *string) { *model = strings.Repeat("m", 201) }, "wake_request_invalid"},
		{"model whitespace", func(_ *CPA, _ *generationHost, _ *store.Account, model, _ *string) { *model = "model\u00a0name" }, "wake_request_invalid"},
		{"model control", func(_ *CPA, _ *generationHost, _ *store.Account, model, _ *string) { *model = "model\x00name" }, "wake_request_invalid"},
		{"invalid model utf8", func(_ *CPA, _ *generationHost, _ *store.Account, model, _ *string) { *model = string([]byte{255}) }, "wake_request_invalid"},
		{"blank prompt", func(_ *CPA, _ *generationHost, _ *store.Account, _, prompt *string) { *prompt = " \t\r\n\u3000" }, "wake_request_invalid"},
		{"long prompt", func(_ *CPA, _ *generationHost, _ *store.Account, _, prompt *string) {
			*prompt = strings.Repeat("a", 8193)
		}, "wake_request_invalid"},
		{"prompt control", func(_ *CPA, _ *generationHost, _ *store.Account, _, prompt *string) { *prompt = "hello\x00there" }, "wake_request_invalid"},
		{"invalid prompt utf8", func(_ *CPA, _ *generationHost, _ *store.Account, _, prompt *string) { *prompt = string([]byte{255}) }, "wake_request_invalid"},
		{"unsupported bridge", func(cpa *CPA, host *generationHost, _ *store.Account, _, _ *string) { cpa.Host = host }, "generation_not_supported"},
		{"negative capability", func(cpa *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			cpa.Host = &wakeHost{host, false}
		}, "generation_not_supported"},
		{"missing host", func(cpa *CPA, _ *generationHost, _ *store.Account, _, _ *string) { cpa.Host = nil }, "generation_not_supported"},
		{"non-codex snapshot", func(_ *CPA, _ *generationHost, account *store.Account, _, _ *string) {
			account.Provider = "antigravity"
		}, "generation_not_supported"},
		{"disabled snapshot", func(_ *CPA, _ *generationHost, account *store.Account, _, _ *string) { account.Status = "disabled" }, "account_disabled"},
		{"missing binding", func(_ *CPA, _ *generationHost, account *store.Account, _, _ *string) { account.Binding = "" }, "account_identity_unavailable"},
		{"missing index", func(_ *CPA, _ *generationHost, account *store.Account, _, _ *string) { account.AuthIndex = "" }, "account_identity_unavailable"},
		{"missing account", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) { host.inventory = `{"files":[]}` }, "account_not_found"},
		{"replaced upstream", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) { host.upstream = "new-upstream" }, "account_binding_changed"},
		{"replaced index", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.inventory = `{"files":[{"id":"a","auth_index":"new","provider":"codex"}]}`
		}, "account_binding_changed"},
		{"replaced provider", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"antigravity"}]}`
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"antigravity","email":"a@example.test"}}`
		}, "account_binding_changed"},
		{"runtime disabled", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","status":"disabled"}}`
		}, "account_disabled"},
		{"runtime unavailable", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","status":"unavailable"}}`
		}, "account_identity_unavailable"},
		{"runtime only", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","runtime_only":true}}`
		}, "account_binding_changed"},
		{"duplicate id", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"codex"},{"id":"a","auth_index":"j","provider":"other"}]}`
		}, "account_identity_unavailable"},
		{"duplicate index", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"codex"},{"id":"b","auth_index":"i","provider":"other"}]}`
		}, "account_identity_unavailable"},
		{"oversize inventory", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.inventory = `{"files":[` + strings.Repeat(`{"id":"a","provider":"codex"},`, 10000) + `{"id":"a","provider":"codex"}]}`
		}, "account_identity_unavailable"},
		{"private callback error", func(_ *CPA, host *generationHost, _ *store.Account, _, _ *string) {
			host.authErr = errors.New("private bearer secret")
		}, "account_identity_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpa, host, account := wakeFixture()
			model, prompt := "gpt-fixture", "Hello."
			test.modify(cpa, host, &account, &model, &prompt)
			err := cpa.ExecuteWake(context.Background(), account, model, prompt)
			if err == nil || err.Error() != test.code || host.executes != 0 || host.closes != 0 {
				t.Fatalf("err=%v execute=%d close=%d", err, host.executes, host.closes)
			}
		})
	}
	cpa, host, account := wakeFixture()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cpa.ExecuteWake(ctx, account, "gpt-fixture", "Hello."); err == nil || err.Error() != "wake_request_invalid" || host.executes != 0 || host.lists != 0 {
		t.Fatalf("pre-canceled err=%v execute=%d list=%d", err, host.executes, host.lists)
	}
}

func TestWakeUnknownNeverReexecutesAndAlwaysClosesOwnedStream(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*generationHost)
	}{
		{"execute lost acknowledgement", func(host *generationHost) { host.executeErr = errors.New("private execute failure") }},
		{"execute rejection after send", func(host *generationHost) { host.executeErr = errors.New("generation_request_invalid") }},
		{"read error", func(host *generationHost) { host.readErr = errors.New("private read failure") }},
		{"close error", func(host *generationHost) { host.closeErr = errors.New("private close failure") }},
		{"non-success status", func(host *generationHost) { host.opened.StatusCode = 429 }},
		{"chunk error", func(host *generationHost) { host.chunks[0].Error = "private upstream error" }},
		{"missing completion", func(host *generationHost) {
			host.chunks = []generationRead{{Payload: []byte(`{"type":"response.output_text.delta","delta":"Hello"}`), Done: true}}
		}},
		{"failed completion", func(host *generationHost) {
			host.chunks[0].Payload = []byte(`{"type":"response.completed","response":{"status":"failed"}}`)
		}},
		{"error event", func(host *generationHost) {
			host.chunks[0].Payload = []byte(`{"type":"error","message":"private failure"}`)
		}},
		{"error after completion", func(host *generationHost) {
			host.chunks[0].Done = false
			host.chunks = append(host.chunks, generationRead{Payload: []byte(`{"type":"error"}`), Done: true})
		}},
		{"refusal", func(host *generationHost) {
			host.chunks[0].Payload = []byte(`{"type":"response.completed","response":{"status":"completed","output":[{"content":[{"type":"refusal","text":"No."}]}]}}`)
		}},
		{"truncated frame", func(host *generationHost) {
			host.chunks[0].Done = false
			host.chunks = append(host.chunks, generationRead{Payload: []byte(`data: {"type":`), Done: true})
		}},
		{"oversize stream", func(host *generationHost) {
			host.chunks = []generationRead{{Payload: []byte(strings.Repeat(":", generationStreamLimit+1)), Done: true}}
		}},
		{"read limit", func(host *generationHost) { host.chunks = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpa, host, account := wakeFixture()
			test.modify(host)
			err := cpa.ExecuteWake(context.Background(), account, "gpt-fixture", "Hello.")
			if err == nil || err.Error() != "wake_delivery_unknown" || host.executes != 1 || host.closes != 1 || host.reads > 1024 || !host.closeContextOK {
				t.Fatalf("err=%v execute=%d close=%d read=%d", err, host.executes, host.closes, host.reads)
			}
		})
	}
	cpa, host, account := wakeFixture()
	host.opened.StreamID = ""
	if err := cpa.ExecuteWake(context.Background(), account, "gpt-fixture", "Hello."); err == nil || err.Error() != "wake_delivery_unknown" || host.executes != 1 || host.closes != 0 {
		t.Fatalf("missing handle err=%v execute=%d close=%d", err, host.executes, host.closes)
	}
}

func TestWakeCancellationClosesBlockedReadAndJoins(t *testing.T) {
	cpa, host, account := wakeFixture()
	host.blockRead = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() { finished <- cpa.ExecuteWake(ctx, account, "gpt-fixture", "Hello.") }()
	select {
	case <-host.readStarted:
	case <-time.After(time.Second):
		t.Fatal("wake read did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil || err.Error() != "wake_delivery_unknown" || host.executes != 1 || host.closes != 1 || !host.closeContextOK {
			t.Fatalf("err=%v execute=%d close=%d", err, host.executes, host.closes)
		}
	case <-time.After(time.Second):
		t.Fatal("wake cancellation left callbacks running")
	}
}

func TestWakeAcceptsNormalTextAndBoundedCompletionFormats(t *testing.T) {
	completed := string(generationCompleted("Normal text, not a JSON candidate array."))
	tests := []struct {
		name     string
		payloads []string
	}{
		{"JSON", []string{completed}},
		{"fragmented SSE", []string{"event: response.completed\ndata: " + completed[:30], completed[30:] + "\n\n"}},
		{"native lines", []string{"event: response.completed", "data: " + completed}},
		{"normal delta", []string{`{"type":"response.output_text.delta","delta":"Hello."}`, `{"type":"response.completed","response":{"status":"completed"}}`}},
		{"large text discarded", []string{string(generationCompleted(strings.Repeat("x", generationTextLimit+1)))}},
		{"empty completed output", []string{`{"type":"response.completed","response":{"status":"completed"}}`}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpa, host, account := wakeFixture()
			host.chunks = nil
			for index, payload := range test.payloads {
				host.chunks = append(host.chunks, generationRead{Payload: []byte(payload), Done: index == len(test.payloads)-1})
			}
			if err := cpa.ExecuteWake(context.Background(), account, strings.Repeat("m", 200), strings.Repeat("中", 2730)+"aa"); err != nil || host.executes != 1 || host.closes != 1 {
				t.Fatalf("err=%v execute=%d close=%d", err, host.executes, host.closes)
			}
		})
	}
}
