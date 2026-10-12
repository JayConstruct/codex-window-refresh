package plugin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	scheduler "codex-window-refresh/internal/runtime"
)

func quotaCallback(t *testing.T, s *Service, method string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	result := rpcResult(t, s.Handle(context.Background(), method, payload))
	if string(result) != "{}" {
		t.Fatalf("response observer altered downstream data: %s", result)
	}
}
func TestServiceWiresAllResponseQuotaHooksAndNeverModifiesResponses(t *testing.T) {
	s, e, _, cb := setupService(t)
	headers := http.Header{"X-Codex-Primary-Used-Percent": {"25"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}
	for _, method := range []string{"response.intercept_after", "response.intercept_stream_chunk", "websocket.response_event"} {
		rpcResult(t, s.Handle(context.Background(), "request.intercept_after", []byte(`{"RequestID":"r","Metadata":{"selected_auth_id":"a"}}`)))
		req := map[string]any{"RequestID": "r", "ResponseHeaders": headers, "ChunkIndex": -1, "Body": []byte("exact original response bytes"), "Metadata": map[string]string{"selected_auth_id": "a"}, "Provider": "codex", "AuthID": "a", "EventType": "codex.rate_limits", "Payload": []byte(`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":25,"window_minutes":300,"reset_after_seconds":3600}}}`)}
		quotaCallback(t, s, method, req)
	}
	e.mu.Lock()
	if len(e.observations) != 3 {
		t.Fatalf("hooks did not enqueue quota: %+v", e.observations)
	}
	for _, q := range e.observations {
		if q.AccountID != "a" || q.Generation != "g" || q.FiveHour.Remaining.Value != 75 || q.Class != core.ObservationExecution {
			t.Fatalf("quota misbound: %+v", q)
		}
	}
	e.mu.Unlock()
	for _, method := range []string{"response.intercept_after", "response.intercept_stream_chunk", "websocket.response_event"} {
		if got := string(rpcResult(t, s.Handle(context.Background(), method, []byte(`broken optional quota`)))); got != "{}" {
			t.Fatal("malformed optional response did not fail open")
		}
	}
	if cb.calls != 0 {
		t.Fatal("foreground response hooks performed host/network calls")
	}
}

type responseIntegrationHost struct{ fakeHost }

func (responseIntegrationHost) Accounts(context.Context) ([]core.HostAccount, error) {
	return []core.HostAccount{{ID: "a", Generation: "g", Enabled: true}}, nil
}
func TestResponseCallbackPublishesRealRuntimeSnapshotWithoutQuotaBridge(t *testing.T) {
	cfg := config.Defaults()
	cfg.Execution.Model = "fixture-model"
	cfg.Enabled = false
	host := responseIntegrationHost{}
	store := &fakeStore{}
	e, err := scheduler.New(cfg, host, store, realClock{}, secureRandom{})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := New(nil, Options{})
	s.engine = e
	s.host = host
	rpcResult(t, s.Handle(context.Background(), "request.intercept_after", []byte(`{"RequestID":"r","Metadata":{"caller_scope":"client","selected_auth_id":"a"}}`)))
	rpcResult(t, s.Handle(context.Background(), "request.complete", []byte(`{"RequestID":"r"}`)))
	quotaCallback(t, s, "response.intercept_after", map[string]any{"RequestID": "r", "ResponseHeaders": http.Header{"X-Codex-Primary-Used-Percent": {"31"}, "X-Codex-Primary-Window-Minutes": {"300"}, "X-Codex-Primary-Reset-After-Seconds": {"3600"}}})
	if err = e.Step(context.Background()); err != nil {
		t.Fatal(err)
	}
	response := managementCall(t, s, "GET", ManagementPrefix+"/accounts")
	var accounts map[core.AccountID]core.AccountState
	if json.Unmarshal(response.Body, &accounts) != nil || accounts["a"].FiveHour.Remaining == nil || accounts["a"].FiveHour.Remaining.Value != 69 || accounts["a"].FiveHour.Source != "cpa_response_headers" || time.Since(accounts["a"].FiveHour.ConfirmedAt) > time.Second {
		t.Fatalf("quota did not reach management snapshot: %s", response.Body)
	}
}
