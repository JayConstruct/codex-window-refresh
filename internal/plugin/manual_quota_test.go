package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"codex-window-refresh/internal/core"
)

type manualQueryEngine struct {
	*fakeEngine
	queued   bool
	requests int
	ready    bool
}

func (e *manualQueryEngine) RequestQuotaQuery() bool {
	if e.queued {
		return false
	}
	e.queued = true
	e.requests++
	return true
}
func (e *manualQueryEngine) Diagnostics() (core.HostCapabilities, string, int) {
	return core.HostCapabilities{QuotaAdapterReady: e.ready}, "", 1
}
func TestQuotaQueryAPIQueuesWithoutSchedulerNotificationOrNetwork(t *testing.T) {
	s, base, _, cb := setupService(t)
	e := &manualQueryEngine{fakeEngine: base, ready: true}
	s.engine = e
	for _, expected := range []string{"queued", "already_queued"} {
		r := managementCall(t, s, "POST", ManagementPrefix+"/quota-query")
		var body map[string]string
		if json.Unmarshal(r.Body, &body) != nil || r.StatusCode != 202 || body["status"] != expected {
			t.Fatalf("bad query response: %+v", r)
		}
	}
	if e.requests != 1 || base.notified != 0 || cb.calls != 0 {
		t.Fatal("query endpoint notified model scheduler, duplicated work or performed host calls")
	}
	s.cfg.QuotaQuery.Manual = false
	if r := managementCall(t, s, "POST", ManagementPrefix+"/quota-query"); r.StatusCode != 409 {
		t.Fatal("disabled manual query accepted")
	}
	s.cfg.QuotaQuery.Manual = true
	e.ready = false
	if r := managementCall(t, s, "POST", ManagementPrefix+"/quota-query"); r.StatusCode != 503 {
		t.Fatal("missing bridge accepted")
	}
	e.ready = true
	payload, _ := json.Marshal(managementRequest{Method: "POST", Path: ManagementPrefix + "/quota-query", Body: []byte(`{"model":"must-not-send","prompt":"test"}`)})
	var r managementResponse
	if json.Unmarshal(rpcResult(t, s.Handle(context.Background(), "management.handle", payload)), &r) != nil || r.StatusCode != 400 {
		t.Fatal("model payload accepted by quota-only API")
	}
	s.stopping = true
	if r := managementCall(t, s, "POST", ManagementPrefix+"/quota-query"); r.StatusCode != 409 {
		t.Fatal("stopped scheduler accepted work")
	}
}
