package executor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/v2/store"
)

type generationHost struct {
	inventory      string
	runtime        string
	upstream       string
	authErr        error
	executeErr     error
	readErr        error
	closeErr       error
	opened         generationOpened
	chunks         []generationRead
	executes       int
	reads          int
	closes         int
	request        generationRequest
	requestBody    []byte
	blockRead      bool
	readStarted    chan struct{}
	closed         chan struct{}
	closeOnce      sync.Once
	closeContextOK bool
}

func generationFixture() (*CPA, *generationHost, store.Account) {
	host := &generationHost{
		inventory: `{"files":[{"id":"a","auth_index":"i","provider":"codex"}]}`,
		runtime:   `{"auth":{"id":"a","auth_index":"i","provider":"codex"}}`,
		upstream:  "upstream", opened: generationOpened{StatusCode: 200, StreamID: "stream-fixture"},
		closed: make(chan struct{}), readStarted: make(chan struct{}),
	}
	account := store.Account{ID: "a", AuthIndex: "i", Status: "available"}
	account.Binding = bindingID(account, "upstream")
	host.chunks = []generationRead{{Payload: generationCompleted(`["First.","Second."]`), Done: true}}
	return &CPA{Host: host}, host, account
}

func generationCompleted(text string) []byte {
	raw, _ := json.Marshal(map[string]any{
		"type": "response.completed", "response": map[string]any{
			"status": "completed", "output": []any{map[string]any{"content": []any{map[string]string{"type": "output_text", "text": text}}}},
		},
	})
	return raw
}

func generationRespond(response, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, response)
}

func (host *generationHost) Call(ctx context.Context, method string, request, response any) error {
	switch method {
	case "host.auth.list":
		if host.authErr != nil {
			return host.authErr
		}
		return json.Unmarshal([]byte(host.inventory), response)
	case "host.auth.get_runtime":
		return json.Unmarshal([]byte(host.runtime), response)
	case "host.auth.get":
		return generationRespond(response, map[string]any{"auth_index": "i", "json": map[string]string{"type": "codex", "account_id": host.upstream, "access_token": "fixture-secret"}})
	case "host.model.execute_stream":
		host.executes++
		raw, _ := json.Marshal(request)
		_ = json.Unmarshal(raw, &host.request)
		host.requestBody = append([]byte(nil), host.request.Body...)
		if err := generationRespond(response, host.opened); err != nil {
			return err
		}
		return host.executeErr
	case "host.model.stream_read":
		host.reads++
		if host.blockRead {
			close(host.readStarted)
			<-host.closed
			return errors.New("private-upstream-cancel")
		}
		if host.readErr != nil {
			return host.readErr
		}
		if len(host.chunks) == 0 {
			return generationRespond(response, generationRead{})
		}
		chunk := host.chunks[0]
		host.chunks = host.chunks[1:]
		return generationRespond(response, chunk)
	case "host.model.stream_close":
		host.closes++
		deadline, ok := ctx.Deadline()
		host.closeContextOK = ctx.Err() == nil && ok && time.Until(deadline) <= 5*time.Second && time.Until(deadline) > 0
		host.closeOnce.Do(func() { close(host.closed) })
		return host.closeErr
	default:
		return errors.New("unexpected-generation-callback")
	}
}

func TestGenerationPinsAccountAndRequest(t *testing.T) {
	cpa, host, account := generationFixture()
	values, err := cpa.GenerateCandidates(context.Background(), account, "gpt-fixture", 2)
	if err != nil || !reflect.DeepEqual(values, []string{"First.", "Second."}) {
		t.Fatalf("values=%v err=%v", values, err)
	}
	request := host.request
	if host.executes != 1 || host.reads != 1 || host.closes != 1 || !host.closeContextOK || request.AuthID != account.ID || request.ForcedProvider != "codex" || request.EntryProtocol != "openai-response" || request.ExitProtocol != "codex" || !request.Stream || request.Model != "gpt-fixture" {
		t.Fatalf("unsafe request=%+v execute=%d read=%d close=%d", request, host.executes, host.reads, host.closes)
	}
	var body struct {
		Model  string `json:"model"`
		Store  *bool  `json:"store"`
		Stream bool   `json:"stream"`
		Input  []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"input"`
	}
	if json.Unmarshal(host.requestBody, &body) != nil || body.Model != request.Model || body.Store == nil || *body.Store || !body.Stream || len(body.Input) != 1 || body.Input[0].Role != "user" || len(body.Input[0].Content) != 1 || body.Input[0].Content[0].Type != "input_text" || !strings.Contains(body.Input[0].Content[0].Text, "exactly 2") || strings.Contains(string(host.requestBody), "fixture-secret") {
		t.Fatalf("invalid body=%s", host.requestBody)
	}
}

func TestGenerationPreflightNeverExecutesInvalidRequests(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*generationHost, *store.Account, *string, *int)
		code   string
	}{
		{"empty model", func(_ *generationHost, _ *store.Account, model *string, _ *int) { *model = "" }, "generation_request_invalid"},
		{"model too long", func(_ *generationHost, _ *store.Account, model *string, _ *int) { *model = strings.Repeat("m", 201) }, "generation_request_invalid"},
		{"model whitespace", func(_ *generationHost, _ *store.Account, model *string, _ *int) { *model = "gpt\u00a0fixture" }, "generation_request_invalid"},
		{"model control", func(_ *generationHost, _ *store.Account, model *string, _ *int) { *model = "gpt\x00fixture" }, "generation_request_invalid"},
		{"model invalid utf8", func(_ *generationHost, _ *store.Account, model *string, _ *int) { *model = string([]byte{255}) }, "generation_request_invalid"},
		{"zero count", func(_ *generationHost, _ *store.Account, _ *string, count *int) { *count = 0 }, "generation_request_invalid"},
		{"large count", func(_ *generationHost, _ *store.Account, _ *string, count *int) { *count = 21 }, "generation_request_invalid"},
		{"disabled snapshot", func(_ *generationHost, account *store.Account, _ *string, _ *int) { account.Status = "disabled" }, "account_disabled"},
		{"missing binding", func(_ *generationHost, account *store.Account, _ *string, _ *int) { account.Binding = "" }, "account_identity_unavailable"},
		{"missing index", func(_ *generationHost, account *store.Account, _ *string, _ *int) { account.AuthIndex = "" }, "account_identity_unavailable"},
		{"missing account", func(host *generationHost, _ *store.Account, _ *string, _ *int) { host.inventory = `{"files":[]}` }, "account_not_found"},
		{"duplicate id", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"codex"},{"id":"a","auth_index":"j","provider":"codex"}]}`
		}, "account_identity_unavailable"},
		{"duplicate index", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"codex"},{"id":"b","auth_index":"i","provider":"other"}]}`
		}, "account_identity_unavailable"},
		{"disabled inventory", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"codex","disabled":true}]}`
		}, "account_disabled"},
		{"unavailable inventory", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"codex","unavailable":true}]}`
		}, "account_identity_unavailable"},
		{"runtime only inventory", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.inventory = `{"files":[{"id":"a","auth_index":"i","provider":"codex","runtime_only":true}]}`
		}, "account_identity_unavailable"},
		{"replaced inventory index", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.inventory = `{"files":[{"id":"a","auth_index":"new","provider":"codex"}]}`
		}, "account_binding_changed"},
		{"disabled runtime", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","disabled":true}}`
		}, "account_disabled"},
		{"unavailable runtime", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","unavailable":true}}`
		}, "account_identity_unavailable"},
		{"runtime only runtime", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","runtime_only":true}}`
		}, "account_binding_changed"},
		{"replaced upstream", func(host *generationHost, _ *store.Account, _ *string, _ *int) { host.upstream = "replacement" }, "account_binding_changed"},
		{"private callback error", func(host *generationHost, _ *store.Account, _ *string, _ *int) {
			host.authErr = errors.New("Authorization: private-secret")
		}, "account_identity_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpa, host, account := generationFixture()
			model, count := "gpt-fixture", 2
			test.modify(host, &account, &model, &count)
			values, err := cpa.GenerateCandidates(context.Background(), account, model, count)
			if err == nil || err.Error() != test.code || values != nil || host.executes != 0 || host.closes != 0 {
				t.Fatalf("values=%v err=%v executions=%d closes=%d", values, err, host.executes, host.closes)
			}
		})
	}
}

func TestGenerationUnknownNeverReplaysAndAlwaysCloses(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*generationHost)
	}{
		{"execute lost acknowledgement", func(host *generationHost) { host.executeErr = errors.New("private transport error") }},
		{"read failed", func(host *generationHost) { host.readErr = errors.New("private read error") }},
		{"close failed", func(host *generationHost) { host.closeErr = errors.New("private close error") }},
		{"non success status", func(host *generationHost) { host.opened.StatusCode = 429 }},
		{"chunk error", func(host *generationHost) { host.chunks[0].Error = "private stream error" }},
		{"missing completion", func(host *generationHost) {
			host.chunks = []generationRead{{Payload: []byte(`{"type":"response.output_text.delta","delta":"[\"First.\",\"Second.\"]"}`), Done: true}}
		}},
		{"failed completion", func(host *generationHost) {
			host.chunks = []generationRead{{Payload: []byte(`{"type":"response.completed","response":{"status":"failed"}}`), Done: true}}
		}},
		{"completion error", func(host *generationHost) {
			host.chunks = []generationRead{{Payload: []byte(`{"type":"response.completed","response":{"status":"completed","error":{"message":"private error"}}}`), Done: true}}
		}},
		{"error after completion", func(host *generationHost) {
			host.chunks[0].Done = false
			host.chunks = append(host.chunks, generationRead{Payload: []byte(`{"type":"error","message":"private error"}`), Done: true})
		}},
		{"truncated trailing frame", func(host *generationHost) {
			host.chunks[0].Done = false
			host.chunks = append(host.chunks, generationRead{Payload: []byte(`data: {"type":`), Done: true})
		}},
		{"text too large", func(host *generationHost) {
			host.chunks = []generationRead{{Payload: generationCompleted(strings.Repeat("x", generationTextLimit+1)), Done: true}}
		}},
		{"stream too large", func(host *generationHost) {
			host.chunks = []generationRead{{Payload: []byte(strings.Repeat(":", generationStreamLimit+1)), Done: true}}
		}},
		{"empty read limit", func(host *generationHost) { host.chunks = nil }},
		{"different completed text", func(host *generationHost) {
			host.chunks = append([]generationRead{{Payload: []byte(`{"type":"response.output_text.delta","delta":"different"}`)}}, host.chunks...)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpa, host, account := generationFixture()
			test.modify(host)
			values, err := cpa.GenerateCandidates(context.Background(), account, "gpt-fixture", 2)
			if values != nil || err == nil || err.Error() != "generation_delivery_unknown" || host.executes != 1 || host.closes != 1 || !host.closeContextOK || host.reads > 1024 {
				t.Fatalf("values=%v err=%v executions=%d reads=%d closes=%d", values, err, host.executes, host.reads, host.closes)
			}
		})
	}
	cpa, host, account := generationFixture()
	host.opened.StreamID = ""
	if _, err := cpa.GenerateCandidates(context.Background(), account, "gpt-fixture", 2); err == nil || err.Error() != "generation_delivery_unknown" || host.executes != 1 || host.closes != 0 {
		t.Fatalf("empty stream err=%v executions=%d closes=%d", err, host.executes, host.closes)
	}
}

func TestGenerationCancellationClosesBlockedReadAndJoins(t *testing.T) {
	cpa, host, account := generationFixture()
	host.blockRead = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := cpa.GenerateCandidates(ctx, account, "gpt-fixture", 2)
		finished <- err
	}()
	select {
	case <-host.readStarted:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if err == nil || err.Error() != "generation_delivery_unknown" || host.closes != 1 || host.executes != 1 || !host.closeContextOK {
			t.Fatalf("err=%v closes=%d executions=%d", err, host.closes, host.executes)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not close and join blocked stream")
	}
}

func TestGenerationFragmentedFormats(t *testing.T) {
	completed := string(generationCompleted(`["First.","Second."]`))
	tests := []struct {
		name     string
		payloads []string
	}{
		{"JSON frame", []string{completed}},
		{"fragmented JSON", []string{completed[:20], completed[20:70], completed[70:]}},
		{"SSE frame", []string{"event: response.completed\ndata: " + completed + "\n\n"}},
		{"native scanner lines", []string{": keepalive", "id: 42", "retry: 1000", "event: response.completed", "data: " + completed}},
		{"fragmented SSE JSON", []string{"event: response.completed\ndata: " + completed[:31], completed[31:80], completed[80:] + "\n\n"}},
		{"fragmented CRLF", []string{"event: response.completed\r", "\ndata: " + completed + "\r", "\n\r", "\n"}},
		{"multiline SSE", []string{"event: response.completed\ndata: {\n", `data: "type":"response.completed",` + "\n", `data: "response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"[\"First.\",\"Second.\"]"}]}]}}` + "\n\n"}},
		{"native multiline SSE", []string{"event: response.completed", "data: {", `data: "type":"response.completed",`, `data: "response":{"status":"completed","output":[{"content":[{"type":"output_text","text":"[\"First.\",\"Second.\"]"}]}]}}`}},
		{"deltas with completion", []string{`{"type":"response.output_text.delta","delta":"[\"First.\","}`, `{"type":"response.output_text.delta","delta":"\"Second.\"]"}`, `{"type":"response.completed","response":{"status":"completed"}}`}},
		{"DONE after completion", []string{"data: " + completed + "\n\n", "data: [DONE]\n\n"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cpa, host, account := generationFixture()
			host.chunks = nil
			for index, payload := range test.payloads {
				host.chunks = append(host.chunks, generationRead{Payload: []byte(payload), Done: index == len(test.payloads)-1})
			}
			values, err := cpa.GenerateCandidates(context.Background(), account, "gpt-fixture", 2)
			if err != nil || !reflect.DeepEqual(values, []string{"First.", "Second."}) || host.closes != 1 {
				t.Fatalf("values=%v err=%v closes=%d", values, err, host.closes)
			}
		})
	}
}

func TestGenerationCandidateBounds(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		count int
		valid bool
	}{
		{"one", `["One."]`, 1, true},
		{"trim", `[" First. ","Second."]`, 2, true},
		{"json fence", "```json\n[\"First.\",\"Second.\"]\n```", 2, true},
		{"bare fence", "```\r\n[\"First.\",\"Second.\"]\r\n```", 2, true},
		{"duplicate", `["First.","First."]`, 2, false},
		{"duplicate after trim", `["First."," First. "]`, 2, false},
		{"empty", `[" "]`, 1, false},
		{"line break", `["First.\nSecond."]`, 1, false},
		{"unicode line break", `["First.\u2028Second."]`, 1, false},
		{"null character", `["First.\u0000Second."]`, 1, false},
		{"too few", `["First."]`, 2, false},
		{"too many", `["First.","Second."]`, 1, false},
		{"not strings", `[42]`, 1, false},
		{"object", `{"values":["One."]}`, 1, false},
		{"extra JSON", `["One."] ["Two."]`, 1, false},
		{"null", `null`, 1, false},
		{"long candidate", `["` + strings.Repeat("a", 513) + `"]`, 1, false},
		{"byte max", `["` + strings.Repeat("a", 512) + `"]`, 1, true},
		{"unicode bytes", `["` + strings.Repeat("中", 171) + `"]`, 1, false},
		{"invalid utf8", "[\"" + string([]byte{255}) + "\"]", 1, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			values, err := candidateValues(test.text, test.count)
			if test.valid && (err != nil || len(values) != test.count) {
				t.Fatalf("values=%v err=%v", values, err)
			}
			if !test.valid && (err == nil || err.Error() != "generation_candidates_invalid" || values != nil) {
				t.Fatalf("values=%v err=%v", values, err)
			}
		})
	}
	cpa, host, account := generationFixture()
	host.chunks[0].Payload = generationCompleted(`["duplicate","duplicate"]`)
	if _, err := cpa.GenerateCandidates(context.Background(), account, "gpt-fixture", 2); err == nil || err.Error() != "generation_candidates_invalid" || host.closes != 1 {
		t.Fatalf("completed malformed response err=%v closes=%d", err, host.closes)
	}
}

type generationUnsupportedHost struct{ *generationHost }

func (*generationUnsupportedHost) SupportsPromptGeneration() bool { return false }

func TestGenerationUnsupportedBridgeNeverExecutes(t *testing.T) {
	cpa, host, account := generationFixture()
	if !cpa.GenerationAvailable() || host.executes != 0 || host.reads != 0 {
		t.Fatal("fake generation capability should be available without callbacks")
	}
	cpa.Host = &generationUnsupportedHost{host}
	if cpa.GenerationAvailable() || host.executes != 0 {
		t.Fatal("unsupported cached capability allowed generation")
	}
	if _, err := cpa.GenerateCandidates(context.Background(), account, "gpt-fixture", 2); err == nil || err.Error() != "generation_not_supported" || host.executes != 0 {
		t.Fatalf("err=%v executes=%d", err, host.executes)
	}
	if (&CPA{}).GenerationAvailable() {
		t.Fatal("missing host advertises generation")
	}
}

func TestGenerationAcceptsTwentyDistinctCandidates(t *testing.T) {
	cpa, host, account := generationFixture()
	var expected []string
	for index := 0; index < 20; index++ {
		expected = append(expected, strings.Repeat("a", index+1))
	}
	raw, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	host.chunks[0].Payload = generationCompleted(string(raw))
	values, err := cpa.GenerateCandidates(context.Background(), account, strings.Repeat("m", 200), 20)
	if err != nil || !reflect.DeepEqual(values, expected) || host.executes != 1 || host.closes != 1 {
		t.Fatalf("values=%v err=%v executes=%d closes=%d", values, err, host.executes, host.closes)
	}
}

func TestReadOnlyQuotaAndIdentityStillAllowDisabledOrUnavailableAccounts(t *testing.T) {
	for _, status := range []string{"disabled", "unavailable"} {
		t.Run(status, func(t *testing.T) {
			cpa, host, account := generationFixture()
			account.Status = status
			host.runtime = `{"auth":{"id":"a","auth_index":"i","provider":"codex","` + status + `":true}}`
			queries := 0
			cpa.transport = roundTrip(func(request *http.Request) (*http.Response, error) {
				queries++
				if request.Method != http.MethodGet || request.URL.String() != "https://chatgpt.com/backend-api/wham/usage" {
					t.Fatal("unexpected read-only quota request")
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"rate_limit":{"allowed":true}}`))}, nil
			})
			identity, err := cpa.Identity(context.Background(), account)
			if err != nil || identity.Binding != account.Binding || identity.Status != status {
				t.Fatalf("identity=%+v err=%v", identity, err)
			}
			if _, err := cpa.Query(context.Background(), account); err != nil || queries != 1 || host.executes != 0 {
				t.Fatalf("quota err=%v HTTP=%d model=%d", err, queries, host.executes)
			}
			if _, err := cpa.GenerateCandidates(context.Background(), account, "gpt-fixture", 2); err == nil || host.executes != 0 {
				t.Fatalf("generation err=%v model=%d", err, host.executes)
			}
		})
	}
}
