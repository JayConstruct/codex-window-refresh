package plugin

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
	scheduler "codex-window-refresh/internal/runtime"
	"codex-window-refresh/internal/storage"
)

func TestRefreshAccountsReadsCPALocalStateWithoutQueryingOrWaking(t *testing.T) {
	callback, reader := &snapshotIntegrationCallback{}, &snapshotIntegrationReader{}
	host := cpa.NewHost(callback, reader)
	store, err := storage.Open(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	cfg := config.Defaults()
	cfg.Enabled, cfg.QuotaQuery.Automatic = true, true
	cfg.Execution.Model = "test-model"
	engine, err := scheduler.New(cfg, host, store, realClock{}, secureRandom{})
	if err != nil {
		t.Fatal(err)
	}
	// Drive only the management action; automatic work is enabled but the
	// execution loop is deliberately not started, so any upstream work is a bug.
	s := New(callback, Options{})
	s.engine, s.host, s.cfg, s.done = engine, host, cfg, make(chan struct{})
	type browserSnapshot struct {
		SnapshotSeq uint64                               `json:"snapshot_seq"`
		Accounts    map[core.AccountID]core.AccountState `json:"accounts"`
	}
	read := func(method, path string) browserSnapshot {
		t.Helper()
		r := managementCall(t, s, method, ManagementPrefix+path)
		var view browserSnapshot
		if r.StatusCode != 200 || json.Unmarshal(r.Body, &view) != nil {
			t.Fatalf("refresh/snapshot failed: %d %s", r.StatusCode, r.Body)
		}
		return view
	}
	initial := read("POST", "/refresh-accounts")
	if !initial.Accounts["healthy"].Enabled || initial.Accounts["broken"].Enabled {
		t.Fatal("local CPA inventory was not synchronized")
	}
	callback.disabled.Store(true)
	if !read("GET", "/snapshot").Accounts["healthy"].Enabled {
		t.Fatal("cached snapshot unexpectedly refreshed CPA state")
	}
	fresh := read("POST", "/refresh-accounts")
	if fresh.Accounts["healthy"].Enabled || fresh.Accounts["healthy"].Availability != core.AccountDisabled || fresh.SnapshotSeq <= initial.SnapshotSeq {
		t.Fatal("CPA disable change did not appear in the returned snapshot")
	}
	callback.disabled.Store(false)
	if !read("POST", "/refresh-accounts").Accounts["healthy"].Enabled {
		t.Fatal("CPA re-enable change was not synchronized")
	}
	if reader.calls.Load() != 0 || callback.modelCalls.Load() != 0 || len(engine.Snapshot().History) != 0 {
		t.Fatal("local inventory refresh queried upstream or executed a model request")
	}
	for _, request := range []struct{ method, path string }{
		{"GET", ManagementPrefix + "/refresh-accounts"},
		{"POST", ResourcePrefix + "/refresh-accounts"},
	} {
		if r := managementCall(t, s, request.method, request.path); r.StatusCode != 404 {
			t.Fatal("refresh exposed on a read-only or public route")
		}
	}
	s.stopping = true
	if r := managementCall(t, s, "POST", ManagementPrefix+"/refresh-accounts"); r.StatusCode != 409 {
		t.Fatal("refresh accepted during shutdown")
	}
}
