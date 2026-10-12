package cpa

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"codex-window-refresh/internal/core"
)

func responsePayload(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func responseAccounts() map[core.AccountID]core.HostAccount {
	return map[core.AccountID]core.HostAccount{"a": {ID: "a", Generation: "g", Enabled: true}, "b": {ID: "b", Generation: "gb", Enabled: true}}
}
func responseHeaders() http.Header {
	return http.Header{"X-Codex-Allowed": {"true"}, "X-Codex-Primary-Used-Percent": {"25"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}, "X-Codex-Secondary-Used-Percent": {"40"}, "X-Codex-Secondary-Window-Minutes": {"10080"}, "X-Codex-Secondary-Reset-After-Seconds": {"86400"}}
}
func trackResponse(t *testing.T, o *ResponseQuotaObserver, accounts map[core.AccountID]core.HostAccount, id, account string, at time.Time) {
	t.Helper()
	o.Track("request.intercept_after", responsePayload(t, map[string]any{"RequestID": id, "Metadata": map[string]string{"selected_auth_id": account}}), accounts, at)
}
func TestForegroundHeadersCapturedOnceWithoutReadingOrdinaryChunks(t *testing.T) {
	for _, method := range []string{"response.intercept_after", "response.intercept_stream_chunk"} {
		t.Run(method, func(t *testing.T) {
			at := time.Unix(1800000000, 0).UTC()
			accounts := responseAccounts()
			o := &ResponseQuotaObserver{}
			trackResponse(t, o, accounts, "r", "a", at)
			// request.complete can arrive before downstream header interception.
			o.Track("request.complete", []byte(`{"RequestID":"r"}`), accounts, at)
			payload := responsePayload(t, map[string]any{"RequestID": "r", "ChunkIndex": -1, "ResponseHeaders": responseHeaders(), "Body": []byte(`{"private":"must stay untouched"}`)})
			calls := 0
			lookup := func() map[core.AccountID]core.HostAccount { calls++; return accounts }
			observed := o.Observe(method, payload, lookup, at)
			if len(observed) != 1 {
				t.Fatalf("quota missing: %+v", observed)
			}
			q := observed[0]
			if q.AccountID != "a" || q.Generation != "g" || q.Class != core.ObservationExecution || q.Source != "cpa_response_headers" || q.FiveHour.Remaining.Value != 75 || q.Weekly.Remaining.Value != 60 || !q.FiveHour.ResetAt.Equal(at.Add(time.Hour)) {
				t.Fatalf("wrong quota: %+v", q)
			}
			if len(o.Observe(method, payload, lookup, at.Add(time.Second))) != 0 {
				t.Fatal("duplicate headers advanced freshness")
			}
			if method == "response.intercept_stream_chunk" {
				before := calls
				for _, index := range []int{0, 1, 20} {
					chunk := responsePayload(t, map[string]any{"RequestID": "r", "ChunkIndex": index, "ResponseHeaders": responseHeaders(), "Body": []byte("data: text\n\n")})
					if len(o.Observe(method, chunk, lookup, at.Add(time.Minute))) != 0 {
						t.Fatal("old stream headers reused")
					}
				}
				if calls != before {
					t.Fatal("ordinary chunks consulted account pool")
				}
			}
		})
	}
}
func TestWebSocketQuotaUsesExplicitAccountAndEventType(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	accounts := responseAccounts()
	o := &ResponseQuotaObserver{}
	trackResponse(t, o, accounts, "r", "a", at)
	frame := []byte(`{"type":"codex.rate_limits","rate_limits":{"allowed":true,"primary":{"used_percent":35,"window_minutes":300,"reset_after_seconds":3500},"secondary":{"used_percent":45,"window_minutes":10080,"reset_after_seconds":86000}}}`)
	lookup := func() map[core.AccountID]core.HostAccount { return accounts }
	for _, tc := range []struct {
		name, provider, auth, event string
		want                        bool
	}{
		{"matching", "codex", "a", "codex.rate_limits", true},
		{"wrong account", "codex", "b", "codex.rate_limits", false},
		{"wrong provider", "claude", "a", "codex.rate_limits", false},
		{"missing account", "codex", "", "codex.rate_limits", false},
		{"wrong discriminator", "codex", "a", "response.delta", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := responsePayload(t, map[string]any{"RequestID": "r", "Provider": tc.provider, "AuthID": tc.auth, "EventType": tc.event, "Payload": frame})
			q := o.Observe("websocket.response_event", p, lookup, at)
			if (len(q) == 1) != tc.want {
				t.Fatalf("wrong observations: %+v", q)
			}
			if tc.want && (q[0].FiveHour.Remaining.Value != 65 || q[0].Source != "cpa_websocket_quota") {
				t.Fatal("incorrect websocket quota")
			}
		})
	}
	// Later provider-native events refresh; a window-only event keeps permission absent.
	p := responsePayload(t, map[string]any{"RequestID": "r", "Provider": "codex", "AuthID": "a", "Payload": []byte(`{"type":"codex.rate_limits","rateLimit":{"primary":{"usedPercent":36,"windowMinutes":300,"resetAfterSeconds":3400}}}`)})
	q := o.Observe("websocket.response_event", p, lookup, at.Add(time.Second))
	if len(q) != 1 || q[0].FiveHour.Remaining.Value != 64 || q[0].Weekly != nil || q[0].Permission != nil {
		t.Fatal("partial websocket quota corrupted absent fields")
	}
	// A WebSocket error can carry usable quota headers independently of its body text.
	p = responsePayload(t, map[string]any{"RequestID": "r", "Provider": "codex", "AuthID": "a", "EventType": "error", "Payload": responsePayload(t, map[string]any{"type": "error", "headers": responseHeaders(), "message": "private error"})})
	if len(o.Observe("websocket.response_event", p, lookup, at.Add(2*time.Second))) != 1 {
		t.Fatal("websocket error quota missing")
	}
	p = responsePayload(t, map[string]any{"RequestID": "r", "Provider": "codex", "AuthID": "a", "EventType": "error", "Payload": []byte(`{"type":"error","headers":{"x-codex-primary-used-percent":"30","x-codex-primary-window-minutes":"300","x-codex-primary-reset-after-seconds":"3000"}}`)})
	if q := o.Observe("websocket.response_event", p, lookup, at.Add(3*time.Second)); len(q) != 1 || q[0].FiveHour.Remaining.Value != 70 {
		t.Fatal("lower-case websocket headers missing")
	}
}
func TestResponseQuotaRejectsUnknownReplacedAndRetriedBindings(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	accounts := responseAccounts()
	o := &ResponseQuotaObserver{}
	lookup := func() map[core.AccountID]core.HostAccount { return accounts }
	p := responsePayload(t, map[string]any{"RequestID": "r", "ResponseHeaders": responseHeaders(), "Metadata": map[string]string{"selected_auth_id": "a"}})
	if len(o.Observe("response.intercept_after", p, lookup, at)) != 0 {
		t.Fatal("unknown request got attributed")
	}
	trackResponse(t, o, accounts, "r", "a", at)
	accounts["a"] = core.HostAccount{ID: "a", Generation: "replacement", Enabled: true}
	if len(o.Observe("response.intercept_after", p, lookup, at)) != 0 {
		t.Fatal("old request rebound to replacement credential")
	}
	trackResponse(t, o, accounts, "r", "b", at)
	if len(o.Observe("response.intercept_after", p, lookup, at)) != 0 {
		t.Fatal("response attributed across retry")
	}
	p = responsePayload(t, map[string]any{"RequestID": "r", "ResponseHeaders": responseHeaders(), "Metadata": map[string]string{"selected_auth_id": "b"}})
	q := o.Observe("response.intercept_after", p, lookup, at)
	if len(q) != 1 || q[0].AccountID != "b" || q[0].Generation != "gb" {
		t.Fatal("new retry binding not used")
	}
	trackResponse(t, o, accounts, "r", "unknown", at)
	if len(o.Observe("response.intercept_after", p, lookup, at)) != 0 {
		t.Fatal("unsupported retry retained prior account")
	}
}
func TestResponseQuotaOptionalInputFailsOpenAndIgnoresClientBodies(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	accounts := responseAccounts()
	o := &ResponseQuotaObserver{}
	lookup := func() map[core.AccountID]core.HostAccount { t.Fatal("irrelevant data consulted accounts"); return nil }
	trackResponse(t, o, accounts, "r", "a", at)
	for _, payload := range [][]byte{[]byte(`broken`), []byte(`{}`), responsePayload(t, map[string]any{"RequestID": "r", "Body": responsePayload(t, map[string]any{"selected_auth_id": "a", "rate_limits": map[string]any{"allowed": true}})}), responsePayload(t, map[string]any{"RequestID": "r", "RequestHeaders": responseHeaders()}), responsePayload(t, map[string]any{"RequestID": "r", "ResponseHeaders": responseHeaders(), "Metadata": map[string]string{"source": "plugin_host_model_callback"}})} {
		if len(o.Observe("response.intercept_after", payload, lookup, at)) != 0 {
			t.Fatal("optional or client input produced quota")
		}
	}
	h := responseHeaders()
	h.Set("X-Codex-Primary-Reset-After-Seconds", "999999")
	if len(o.Observe("response.intercept_after", responsePayload(t, map[string]any{"RequestID": "r", "ResponseHeaders": h}), lookup, at)) != 0 {
		t.Fatal("malformed reset accepted")
	}
	o.Track("request.intercept_after", responsePayload(t, map[string]any{"RequestID": "r", "Metadata": map[string]string{"selected_auth_id": "a", "source": "plugin_host_model_callback"}}), accounts, at)
	if len(o.Observe("response.intercept_after", responsePayload(t, map[string]any{"RequestID": "r", "ResponseHeaders": responseHeaders()}), func() map[core.AccountID]core.HostAccount { return accounts }, at)) != 0 {
		t.Fatal("plugin-generated request got observed twice")
	}
}
func TestResponseBindingsExpireAreBoundedAndConcurrencySafe(t *testing.T) {
	at := time.Unix(1800000000, 0).UTC()
	accounts := responseAccounts()
	o := &ResponseQuotaObserver{}
	lookup := func() map[core.AccountID]core.HostAccount { return accounts }
	trackResponse(t, o, accounts, "r", "a", at)
	p := responsePayload(t, map[string]any{"RequestID": "r", "ResponseHeaders": responseHeaders()})
	if len(o.Observe("response.intercept_after", p, lookup, at.Add(responseBindingTTL))) != 0 || len(o.bindings) != 0 {
		t.Fatal("expired binding survived")
	}
	for i := 0; i < maxResponseBindings+10; i++ {
		trackResponse(t, o, accounts, fmt.Sprint(i), "a", at.Add(time.Duration(i)*time.Millisecond))
	}
	if len(o.bindings) != maxResponseBindings {
		t.Fatal("bindings grew without limit")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprint(i)
			raw, _ := json.Marshal(map[string]any{"RequestID": id, "Metadata": map[string]string{"selected_auth_id": "a"}})
			o.Track("request.intercept_after", raw, accounts, at)
			raw, _ = json.Marshal(map[string]any{"RequestID": id, "ResponseHeaders": responseHeaders()})
			o.Observe("response.intercept_after", raw, lookup, at)
		}(i)
	}
	wg.Wait()
}
