package cpa

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

type callbackFunc func(context.Context, string, any, any) error

func (f callbackFunc) Call(ctx context.Context, method string, request, response any) error {
	return f(ctx, method, request, response)
}
func respond(dest any, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dest)
}

type fakeCallbacks struct {
	upstream, token string
	chunks          []streamRead
	index           int
	opened, closed  bool
	captured        modelRequest
	failure         bool
}

func (f *fakeCallbacks) Call(_ context.Context, method string, request, response any) error {
	switch method {
	case "host.auth.list":
		return respond(response, authList{Files: []authEntry{{ID: "host-auth", AuthIndex: "auth-index", Provider: "codex", Status: "active"}}})
	case "host.auth.get_runtime":
		return respond(response, authRuntime{Auth: authEntry{ID: "host-auth", AuthIndex: "auth-index", Provider: "codex", Status: "active"}})
	case "host.auth.get":
		return respond(response, map[string]any{"auth_index": "auth-index", "json": map[string]any{"type": "codex", "account_id": f.upstream, "access_token": f.token, "updated_at": time.Now().Format(time.RFC3339Nano)}})
	case "host.model.execute_stream":
		f.opened = true
		f.captured = request.(modelRequest)
		return respond(response, streamResponse{StatusCode: 200, StreamID: "stream-id"})
	case "host.model.stream_read":
		if f.failure {
			return errors.New("upstream error may contain secret")
		}
		if f.index >= len(f.chunks) {
			return respond(response, streamRead{Done: true})
		}
		chunk := f.chunks[f.index]
		f.index++
		return respond(response, chunk)
	case "host.model.stream_close":
		f.closed = true
		return respond(response, struct{}{})
	default:
		return errors.New("unexpected callback")
	}
}
func TestGenerationUsesUpstreamIdentityAndExactPin(t *testing.T) {
	f := &fakeCallbacks{upstream: "real-upstream", token: "initial-token", chunks: []streamRead{{Payload: []byte("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"), Done: true}}}
	h := NewHost(f, nil)
	accounts, err := h.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gen := accounts[0].Generation
	f.token = "oauth-refreshed"
	accounts, err = h.Accounts(context.Background())
	if err != nil || accounts[0].Generation != gen {
		t.Fatalf("OAuth refresh changed generation: %v", err)
	}
	result, err := h.SendExact(context.Background(), core.ExactRequest{AccountID: "host-auth", Generation: gen, OperationID: "op", Model: "supported", Prompt: "Reply OK."})
	if err != nil || result.Disposition != core.SendCompleted || !f.closed || f.captured.AuthID != "host-auth" || f.captured.ForcedProvider != "codex" || f.captured.EntryProtocol != "openai-response" || f.captured.ExitProtocol != "codex" {
		t.Fatalf("wrong pinned flow: %+v err=%v request=%+v closed=%v", result, err, f.captured, f.closed)
	}
	f.upstream = "different-upstream"
	f.opened = false
	result, err = h.SendExact(context.Background(), core.ExactRequest{AccountID: "host-auth", Generation: gen, OperationID: "op2", Model: "supported", Prompt: "hi"})
	if err != nil || result.Disposition != core.SendDefinitelyNotSent || f.opened {
		t.Fatalf("changed credential sent: %+v %v", result, err)
	}
}
func TestStreamsRequireTerminalCompletionAndAlwaysClose(t *testing.T) {
	cases := []struct {
		name     string
		chunks   []streamRead
		failure  bool
		expected core.SendDisposition
	}{
		{"empty", nil, false, core.SendUnknown},
		{"delta only", []streamRead{{Payload: []byte("data: {\"type\":\"response.output_text.delta\"}\n\n"), Done: true}}, false, core.SendUnknown},
		{"split completion", []streamRead{{Payload: []byte("data: {\"type\":\"response.comp")}, {Payload: []byte("leted\"}\n\n"), Done: true}}, false, core.SendCompleted},
		{"terminal error", []streamRead{{Payload: []byte("data: {\"type\":\"response.completed\"}\n\n"), Error: "secret-error", Done: true}}, false, core.SendUnknown},
		{"callback error", nil, true, core.SendUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeCallbacks{upstream: "real", chunks: tc.chunks, failure: tc.failure}
			h := NewHost(f, nil)
			a, err := h.Accounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			r, err := h.SendExact(context.Background(), core.ExactRequest{AccountID: a[0].ID, Generation: a[0].Generation, OperationID: "op", Model: "m", Prompt: "hi"})
			if err != nil || r.Disposition != tc.expected || !f.closed || strings.Contains(r.ErrorCode, "secret") {
				t.Fatalf("result %+v error %v closed %v", r, err, f.closed)
			}
		})
	}
}
func TestQuotaWindowLengthAndUnknownPlaceholder(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	read := core.QuotaRead{AccountID: "a", Generation: "g", ReadStartedSeq: 7}
	raw := []byte(`{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1800018000},"secondary_window":{"used_percent":10,"limit_window_seconds":604800,"reset_at":1800604800}}}`)
	o, err := ParseQuota(raw, read, at, core.ObservationQuery, core.AccountState{FiveHourBaseline: core.WindowBaseline{CycleID: "old", StableResetAt: at.Add(-time.Minute)}})
	if err != nil || o.FiveHour.State != core.WindowUnknown || o.Weekly.State != core.WindowActive || *o.Permission != core.PermissionAllowed || o.FiveHour.Remaining.Value != 100 || o.Weekly.Remaining.Value != 90 {
		t.Fatalf("quota %+v err %v", o, err)
	}
	// Primary/secondary position is not sufficient: the exact interval identifies kind.
	raw = []byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":5,"limit_window_seconds":604800,"reset_at":1800604800},"secondary_window":{"used_percent":100,"limit_window_seconds":18000,"reset_at":1799999999}}}`)
	o, err = ParseQuota(raw, read, at, core.ObservationQuery, core.AccountState{})
	if err != nil || o.FiveHour.State != core.WindowExpired || o.Weekly.State != core.WindowActive {
		t.Fatalf("swapped interval windows %+v %v", o, err)
	}
	for _, raw := range []string{`{}`, `{"rate_limit":{"primary_window":{"used_percent":101,"limit_window_seconds":18000,"reset_at":1800018000}}}`, `{"rate_limit":{"primary_window":{"used_percent":1,"limit_window_seconds":18000}}}`} {
		if _, err := ParseQuota([]byte(raw), read, at, core.ObservationQuery, core.AccountState{}); err == nil {
			t.Fatalf("invalid quota accepted: %s", raw)
		}
	}
}
func TestManagementBridgeUsesIndexAndHostTokenSubstitution(t *testing.T) {
	var calls int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Path != "/v0/management/api-call" || r.Header.Get("Authorization") != "Bearer test-management-key" {
			t.Error("wrong management endpoint/auth")
		}
		var body struct {
			AuthIndex, Method, URL string
			Header                 map[string]string
		}
		var raw map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Fatal(err)
		}
		_ = json.Unmarshal(raw["auth_index"], &body.AuthIndex)
		_ = json.Unmarshal(raw["method"], &body.Method)
		_ = json.Unmarshal(raw["url"], &body.URL)
		_ = json.Unmarshal(raw["header"], &body.Header)
		if body.AuthIndex != "index" || body.Method != "GET" || body.URL != "https://chatgpt.com/backend-api/wham/usage" || body.Header["Authorization"] != "Bearer $TOKEN$" || body.Header["Chatgpt-Account-Id"] != "upstream" {
			t.Errorf("wrong bridge request %+v", body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"status_code": 200, "body": `{"rate_limit":{"allowed":true}}`})
	})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, r)
		return recorder.Result(), nil
	})}
	reader := &BridgeReader{ManagementURL: "http://management.invalid", ManagementKeyEnv: "TEST_KEY", Getenv: func(string) string { return "test-management-key" }, Client: client}
	raw, err := reader.Read(context.Background(), AuthBinding{AuthIndex: "index", UpstreamAccountID: "upstream"})
	if err != nil || !json.Valid(raw) || calls != 1 {
		t.Fatalf("bridge error %v", err)
	}
	reader.Getenv = func(string) string { return "" }
	if _, err := reader.Read(context.Background(), AuthBinding{AuthIndex: "index", UpstreamAccountID: "upstream"}); !errors.Is(err, ErrMissingBridge) || calls != 1 {
		t.Fatal("missing key dispatched")
	}
}
func TestTrustedActivityAndPassThrough(t *testing.T) {
	accounts := map[core.AccountID]core.HostAccount{"a": {ID: "a", Generation: "g"}}
	original := []byte(`{"RequestID":"request","Metadata":{"caller_scope":"host-hash","selected_auth_id":"a"},"Headers":{"X-Test":["one","two"]},"Body":"cHJvbXB0"}`)
	events, err := ParseActivity("request.intercept_before", original, accounts)
	if err != nil || len(events) != 1 || events[0].Phase != core.ActivityStarted || events[0].Identity != "host-hash" {
		t.Fatalf("activity %+v %v", events, err)
	}
	events, err = ParseActivity("request.intercept_after", original, accounts)
	if err != nil || events[0].AccountID != "a" || events[0].Generation != "g" {
		t.Fatalf("selection %+v %v", events, err)
	}
	response, err := PassThrough("request.intercept_before", original)
	raw, _ := json.Marshal(response)
	if err != nil || !strings.Contains(string(raw), `"Body":"cHJvbXB0"`) || !strings.Contains(string(raw), `"X-Test":["one","two"]`) {
		t.Fatalf("request mutated: %s %v", raw, err)
	}
	internal := []byte(`{"RequestID":"plugin","Metadata":{"caller_scope":"host-hash","source":"plugin_host_model_callback"}}`)
	events, err = ParseActivity("request.intercept_before", internal, accounts)
	if err != nil || len(events) != 0 {
		t.Fatal("plugin traffic counted")
	}
	events, err = ParseActivity("request.complete", []byte(`{"RequestID":"request","CompletedAt":"2026-10-11T00:00:00Z"}`), accounts)
	if err != nil || len(events) != 1 || events[0].Phase != core.ActivityCompleted || events[0].Identity != "" {
		t.Fatalf("completion without partial metadata lost: %+v %v", events, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestZeroUsedFullWindowPreservesOnlyAlreadyConfirmedSameActiveWindow(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	read := core.QuotaRead{AccountID: "a", Generation: "g", ReadStartedSeq: 9}
	reset := at.Add(5 * time.Hour)
	baseline := core.AccountState{ID: "a", Generation: "g", FiveHour: core.WindowEvidence{State: core.WindowActive, ResetAt: reset}, FiveHourBaseline: core.WindowBaseline{CycleID: "confirmed-cycle", StableResetAt: reset}}
	raw := []byte(`{"rate_limit":{"allowed":true,"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_at":1800018000}}}`)
	o, err := ParseQuota(raw, read, at, core.ObservationQuery, baseline)
	if err != nil || o.FiveHour.State != core.WindowActive || o.FiveHour.CycleID != "confirmed-cycle" {
		t.Fatalf("confirmed active cycle was lost: %+v %v", o, err)
	}
	for _, change := range []func(*core.AccountState){func(a *core.AccountState) { a.Generation = "different" }, func(a *core.AccountState) { a.FiveHourBaseline.StableResetAt = at.Add(-time.Minute) }, func(a *core.AccountState) { a.FiveHour.State = core.WindowUnknown }} {
		b := baseline
		change(&b)
		o, err = ParseQuota(raw, read, at, core.ObservationQuery, b)
		if err != nil || o.FiveHour.State != core.WindowUnknown {
			t.Fatalf("zero placeholder accepted as new active: %+v %v", o, err)
		}
	}
	o, err = ParseQuota(raw, read, at, core.ObservationExecution, baseline)
	if err != nil || o.FiveHour.State != core.WindowUnknown {
		t.Fatal("zero execution evidence invented activation")
	}
}

func TestCancelClosesBlockedNativeStyleReadAndJoinsWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	readStarted := make(chan struct{})
	closed := make(chan struct{})
	var closeOnce sync.Once
	f := &fakeCallbacks{upstream: "real"}
	cb := callbackFunc(func(ctx context.Context, method string, request, response any) error {
		switch method {
		case "host.model.stream_read":
			close(readStarted)
			<-closed
			return errors.New("read canceled")
		case "host.model.stream_close":
			closeOnce.Do(func() { close(closed) })
			return respond(response, struct{}{})
		default:
			return f.Call(ctx, method, request, response)
		}
	})
	h := NewHost(cb, nil)
	accounts, err := h.Accounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan core.SendResult, 1)
	go func() {
		r, _ := h.SendExact(ctx, core.ExactRequest{AccountID: "host-auth", Generation: accounts[0].Generation, OperationID: "op", Model: "model", Prompt: "hi"})
		result <- r
	}()
	select {
	case <-readStarted:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	cancel()
	select {
	case r := <-result:
		if r.Disposition != core.SendUnknown {
			t.Fatalf("canceled delivery incorrectly classified: %+v", r)
		}
	case <-time.After(time.Second):
		t.Fatal("explicit cancellation failed to unblock native-style read")
	}
}

func TestCanceledOpenResponseStillReleasesDecodedStream(t *testing.T) {
	f := &fakeCallbacks{upstream: "real"}
	cb := callbackFunc(func(ctx context.Context, method string, request, response any) error {
		if err := f.Call(ctx, method, request, response); err != nil {
			return err
		}
		if method == "host.model.execute_stream" {
			return context.Canceled
		}
		return nil
	})
	h := NewHost(cb, nil)
	accounts, err := h.Accounts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := h.SendExact(context.Background(), core.ExactRequest{AccountID: "host-auth", Generation: accounts[0].Generation, OperationID: "op", Model: "model", Prompt: "hi"})
	if err != nil || result.Disposition != core.SendUnknown || !f.closed {
		t.Fatalf("open handle leaked: %+v %v closed %v", result, err, f.closed)
	}
}
