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

type selectedQueryEngine struct {
	*manualQueryEngine
	target core.AccountID
}

func (e *selectedQueryEngine) QueueQuotaQuery(id core.AccountID) string {
	if e.queued {
		if e.target == id {
			return "already_queued"
		}
		return "busy"
	}
	e.target, e.queued = id, true
	e.requests++
	return "queued"
}

func TestAccountQuotaQueryAPISelectsExactAccountAndRejectsOtherPayloads(t *testing.T) {
	s, base, _, cb := setupService(t)
	e := &selectedQueryEngine{manualQueryEngine: &manualQueryEngine{fakeEngine: base, ready: true}}
	s.engine = e
	call := func(body string) managementResponse {
		t.Helper()
		payload, _ := json.Marshal(managementRequest{Method: "POST", Path: ManagementPrefix + "/quota-query", Body: []byte(body)})
		var result managementResponse
		if err := json.Unmarshal(rpcResult(t, s.Handle(context.Background(), "management.handle", payload)), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, body := range []string{`{"account_id":"a"}`, `{"account_id":"a"}`} {
		if r := call(body); r.StatusCode != 202 {
			t.Fatalf("selected query rejected: %+v", r)
		}
	}
	if e.target != "a" || e.requests != 1 || cb.calls != 0 || base.notified != 0 {
		t.Fatal("account selection was lost, duplicated or performed host work")
	}
	if r := call(`{}`); r.StatusCode != 409 {
		t.Fatal("batch query widened active account query")
	}
	if r := call(`{"account_id":"missing"}`); r.StatusCode != 404 {
		t.Fatal("unknown account accepted")
	}
	for _, body := range []string{`null`, `[]`, `{"account_id":""}`, `{"account_id":null}`, `{"account_id":1}`, `{"account_id":"a","model":"test"}`, `{"prompt":"test"}`, `{} {}`} {
		if r := call(body); r.StatusCode != 400 {
			t.Fatalf("invalid quota parameters accepted: %s => %+v", body, r)
		}
	}
	s.cfg.QuotaQuery.Manual = false
	if r := call(`{"account_id":"a"}`); r.StatusCode != 409 {
		t.Fatal("account query bypassed manual permission")
	}
	s.cfg.QuotaQuery.Manual = true
	e.ready = false
	if r := call(`{"account_id":"a"}`); r.StatusCode != 503 {
		t.Fatal("account query bypassed bridge readiness")
	}
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
