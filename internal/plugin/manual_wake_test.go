package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
)

type manualWakeEngine struct {
	*fakeEngine
	target      core.AccountID
	requests    int
	unavailable bool
	ready       bool
}

func (engine *manualWakeEngine) Snapshot() core.CoreState {
	state := engine.fakeEngine.Snapshot()
	account := state.Accounts["a"]
	account.Enabled = !engine.unavailable
	state.Accounts["a"] = account
	return state
}

func (engine *manualWakeEngine) QueueManualWake(id core.AccountID) string {
	if engine.target != "" {
		return "already_queued"
	}
	engine.target = id
	engine.requests++
	return "queued"
}

func (engine *manualWakeEngine) Diagnostics() (core.HostCapabilities, string, int) {
	return core.HostCapabilities{StableAccountIdentity: engine.ready, ExactAccountExecution: engine.ready, QuotaAdapterReady: engine.ready}, "", 1
}

func TestManualWakeAPIRequiresExactAccountAndQueuesWithoutNetwork(t *testing.T) {
	service, base, _, callback := setupService(t)
	engine := &manualWakeEngine{fakeEngine: base, ready: true, unavailable: true}
	service.engine = engine
	service.cfg.Enabled = false
	call := func(body string) managementResponse {
		payload, _ := json.Marshal(managementRequest{Method: "POST", Path: ManagementPrefix + "/manual-wake", Body: []byte(body)})
		var result managementResponse
		if err := json.Unmarshal(rpcResult(t, service.Handle(context.Background(), "management.handle", payload)), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, body := range []string{"", `{}`, `null`, `[]`, `{"account_id":null}`, `{"account_id":""}`, `{"account_id":123}`, `{"account_id":"a","model":"override"}`, `{"account_id":"a","account_id":"b"}`, `{"account_id":"a"} {}`} {
		if result := call(body); result.StatusCode != 400 {
			t.Fatalf("invalid selection accepted: %s => %d", body, result.StatusCode)
		}
	}
	if call(`{"account_id":"missing"}`).StatusCode != 404 {
		t.Fatal("unknown account accepted")
	}
	for attempt := 0; attempt < 2; attempt++ {
		if call(`{"account_id":"a"}`).StatusCode != 202 {
			t.Fatal("manual wake required automatic wake")
		}
	}
	if engine.target != "a" || engine.requests != 1 || callback.calls != 0 || base.notified != 0 {
		t.Fatal("API performed host network work or changed target")
	}
	engine.ready = false
	if call(`{"account_id":"a"}`).StatusCode != 503 {
		t.Fatal("unready host accepted")
	}
	engine.ready = true
	service.cfg.Execution.Model = ""
	if call(`{"account_id":"a"}`).StatusCode != 409 {
		t.Fatal("missing model accepted")
	}
	service.cfg.Execution.Model = "model"
	paused := true
	service.cfg.AccountOverrides["a"] = config.AccountOverride{Paused: &paused}
	if call(`{"account_id":"a"}`).StatusCode != 409 {
		t.Fatal("paused account accepted")
	}
	delete(service.cfg.AccountOverrides, "a")
	disabled := false
	service.wrapper.HostEnabled = &disabled
	if call(`{"account_id":"a"}`).StatusCode != 409 {
		t.Fatal("disabled CPA plugin accepted")
	}
}
