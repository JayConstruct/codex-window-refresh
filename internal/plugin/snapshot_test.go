package plugin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	scheduler "codex-window-refresh/internal/runtime"
)

type atomicSnapshotEngine struct {
	*fakeEngine
	view  scheduler.ManagementSnapshot
	reads int
}

func (e *atomicSnapshotEngine) ManagementSnapshot() scheduler.ManagementSnapshot {
	e.reads++
	return e.view
}
func (*atomicSnapshotEngine) Snapshot() core.CoreState { panic("split state read") }
func (*atomicSnapshotEngine) Diagnostics() (core.HostCapabilities, string, int) {
	panic("split capability read")
}
func (*atomicSnapshotEngine) SchedulerDiagnostics() any { panic("split scheduler read") }

func TestUnifiedSnapshotUsesOnePublishedViewAndExcludesPrivateState(t *testing.T) {
	s, base, _, _ := setupService(t)
	cfg := config.Defaults()
	business := true
	cfg.Enabled, cfg.ManualWakeDisabled, cfg.BusinessEnabled = false, true, &business
	engine := &atomicSnapshotEngine{fakeEngine: base, view: scheduler.ManagementSnapshot{
		InstanceID: "engine-123", SnapshotSeq: 91, Config: cfg,
		Capabilities: core.HostCapabilities{QuotaAdapterReady: true}, LastError: "account_read_failed",
		State:     core.CoreState{Revision: 17, Accounts: map[core.AccountID]core.AccountState{"snapshot-account": {ID: "snapshot-account"}}, Activity: map[core.UserIdentity]core.UserActivity{"private-identity": {Identity: "private-identity"}}},
		Scheduler: scheduler.SchedulingSummary{Jobs: []scheduler.JobStatus{{JobID: "retained-1", Kind: "quota_query", Phase: "completed"}}},
	}}
	s.engine = engine
	r := managementCall(t, s, "GET", ManagementPrefix+"/snapshot")
	var got struct {
		InstanceID string `json:"instance_id"`
		Seq        uint64 `json:"snapshot_seq"`
		Summary    struct {
			Enabled, BusinessEnabled, HostEnabled, Running bool
			Revision                                       uint64
			Accounts                                       int
			Scheduler                                      scheduler.SchedulingSummary
		}
		Accounts     map[core.AccountID]core.AccountState
		Capabilities core.HostCapabilities
		Config       config.Config
	}
	// Explicit tags preserve the lower-case API names for compound fields.
	var raw map[string]json.RawMessage
	if r.StatusCode != 200 || json.Unmarshal(r.Body, &raw) != nil || json.Unmarshal(r.Body, &got) != nil {
		t.Fatalf("snapshot failed: %s", r.Body)
	}
	var summary map[string]json.RawMessage
	_ = json.Unmarshal(raw["summary"], &summary)
	var businessApplied, hostEnabled bool
	_ = json.Unmarshal(summary["business_enabled"], &businessApplied)
	_ = json.Unmarshal(summary["host_enabled"], &hostEnabled)
	if engine.reads != 1 || got.InstanceID != "engine-123" || got.Seq != 91 || got.Summary.Revision != 17 || got.Summary.Accounts != len(got.Accounts) || got.Summary.Enabled || !businessApplied || hostEnabled || !got.Summary.Running || !got.Capabilities.QuotaAdapterReady || got.Config.Enabled || len(got.Summary.Scheduler.Jobs) != 1 {
		t.Fatalf("view was mixed or incomplete: %s", r.Body)
	}
	if strings.Contains(string(r.Body), "private-identity") || strings.Contains(string(r.Body), "BusinessEnabled") {
		t.Fatal("private state leaked into snapshot")
	}
	if r.Headers.Get("Cache-Control") != "no-store" {
		t.Fatal("live state may be cached")
	}
	if r := managementCall(t, s, "GET", ResourcePrefix+"/snapshot"); r.StatusCode != 404 {
		t.Fatal("snapshot exposed on public resource path")
	}
}

func TestSnapshotExposesGenerationCatalogSeparatelyFromCodexAccounts(t *testing.T) {
	s, base, _, _ := setupService(t)
	s.engine = &atomicSnapshotEngine{fakeEngine: base, view: scheduler.ManagementSnapshot{
		InstanceID: "generation-catalog", SnapshotSeq: 1, Config: config.Defaults(),
		State: core.CoreState{GenerationAccounts: map[core.AccountID]core.GenerationAccount{
			"ag": {ID: "ag", Provider: "antigravity", Generation: "identity-digest", Enabled: true},
		}},
	}}
	r := managementCall(t, s, "GET", ManagementPrefix+"/snapshot")
	var body struct {
		Accounts           map[core.AccountID]core.AccountState
		GenerationAccounts map[core.AccountID]core.GenerationAccount `json:"generation_accounts"`
		Summary            struct{ Accounts int }
	}
	if r.StatusCode != 200 || json.Unmarshal(r.Body, &body) != nil || len(body.Accounts) != 0 || body.Summary.Accounts != 0 || !body.GenerationAccounts["ag"].Enabled || body.GenerationAccounts["ag"].Provider != "antigravity" {
		t.Fatalf("generation catalog was lost or included in the Codex pool: %s", r.Body)
	}
}

type identifiedQuotaEngine struct{ *manualQueryEngine }

func (e *identifiedQuotaEngine) QueueQuotaQueryJob(id core.AccountID) (string, string) {
	if e.queued {
		return "already_queued", "quota-job-1"
	}
	e.queued = true
	e.requests++
	return "queued", "quota-job-1"
}

type identifiedWakeEngine struct{ *manualWakeEngine }

func (e *identifiedWakeEngine) QueueManualWakeJob(id core.AccountID) (string, string) {
	return e.QueueManualWake(id), "wake-job-1"
}

func TestAcceptedAsyncAPIsReturnTheCoalescedRuntimeJobIdentity(t *testing.T) {
	s, base, _, _ := setupService(t)
	quota := &identifiedQuotaEngine{&manualQueryEngine{fakeEngine: base, ready: true}}
	call := func(path string) map[string]string {
		t.Helper()
		payload, _ := json.Marshal(managementRequest{Method: "POST", Path: ManagementPrefix + path, Body: []byte(`{"account_id":"a"}`)})
		var result managementResponse
		if err := json.Unmarshal(rpcResult(t, s.Handle(context.Background(), "management.handle", payload)), &result); err != nil || result.StatusCode != 202 {
			t.Fatalf("request failed: %+v %v", result, err)
		}
		var reply map[string]string
		if err := json.Unmarshal(result.Body, &reply); err != nil {
			t.Fatal(err)
		}
		return reply
	}
	s.engine = quota
	if first, second := call("/quota-query"), call("/quota-query"); first["job_id"] != "quota-job-1" || second["job_id"] != first["job_id"] || second["status"] != "already_queued" || quota.requests != 1 {
		t.Fatal("quota acceptance lost or duplicated its identity")
	}
	s.engine = &identifiedWakeEngine{&manualWakeEngine{fakeEngine: base, ready: true}}
	if first, second := call("/manual-wake"), call("/manual-wake"); first["job_id"] != "wake-job-1" || second["job_id"] != first["job_id"] || second["status"] != "already_queued" {
		t.Fatal("wake acceptance lost its identity")
	}
}
