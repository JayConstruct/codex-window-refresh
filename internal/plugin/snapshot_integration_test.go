package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
	scheduler "codex-window-refresh/internal/runtime"
	"codex-window-refresh/internal/storage"
)

type snapshotIntegrationCallback struct{ modelCalls atomic.Int32 }

func (callback *snapshotIntegrationCallback) Call(_ context.Context, method string, request, response any) error {
	if strings.Contains(method, "model") {
		callback.modelCalls.Add(1)
		return errors.New("unexpected model request")
	}
	var value any
	switch method {
	case "host.auth.list":
		value = map[string]any{"files": []map[string]any{{"id": "healthy", "auth_index": "healthy-index", "provider": "codex"}, {"id": "broken", "auth_index": "broken-index", "provider": "codex"}}}
	case "host.auth.get_runtime":
		raw, _ := json.Marshal(request)
		var selected struct {
			Index string `json:"auth_index"`
		}
		_ = json.Unmarshal(raw, &selected)
		id := "healthy"
		if selected.Index == "broken-index" {
			id = "broken"
		}
		value = map[string]any{"auth": map[string]any{"id": id, "auth_index": selected.Index, "provider": "codex", "runtime_only": id == "broken"}}
	case "host.auth.get":
		value = map[string]any{"auth_index": "healthy-index", "json": map[string]any{"type": "codex", "account_id": "test-upstream"}}
	default:
		return errors.New("unexpected callback")
	}
	raw, _ := json.Marshal(value)
	return json.Unmarshal(raw, response)
}

type snapshotIntegrationReader struct{ calls atomic.Int32 }

func (reader *snapshotIntegrationReader) Read(_ context.Context, binding cpa.AuthBinding) ([]byte, error) {
	if binding.AccountID != "healthy" {
		return nil, errors.New("invalid account reached quota transport")
	}
	reader.calls.Add(1)
	return []byte(`{"rate_limit":{"allowed":true,"limit_reached":false}}`), nil
}

func TestSnapshotAndTrackedQueriesWithRealRuntimeAndMixedInventory(t *testing.T) {
	callback, reader := &snapshotIntegrationCallback{}, &snapshotIntegrationReader{}
	dir := t.TempDir()
	s := New(callback, Options{
		EngineFactory: func(cfg config.Config, host core.Host, store core.Store, clock core.Clock, random core.Random) (Engine, error) {
			return scheduler.New(cfg, host, store, clock, random)
		},
		HostFactory: func(cb cpa.Callback, _ WrapperConfig) (Host, error) { return cpa.NewHost(cb, reader), nil },
		OpenStore:   func(path string) (DurableStore, error) { return storage.Open(path) },
		LoadConfig:  func(string) (config.Config, error) { return config.Defaults(), nil },
	})
	wrapper := "config_file: " + filepath.Join(dir, "policy.yaml") + "\nstate_dir: " + filepath.Join(dir, "state") + "\n"
	rpcResult(t, s.Handle(context.Background(), "plugin.register", lifecyclePayload(t, wrapper, 6)))
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	type snapshot struct {
		InstanceID   string `json:"instance_id"`
		Seq          uint64 `json:"snapshot_seq"`
		Summary      struct{ Scheduler scheduler.SchedulingSummary }
		Accounts     map[core.AccountID]core.AccountState
		Capabilities struct {
			Inventory map[core.AccountID]string `json:"account_inventory_diagnostics"`
		}
	}
	read := func() snapshot {
		t.Helper()
		r := managementCall(t, s, "GET", ManagementPrefix+"/snapshot")
		var view snapshot
		if r.StatusCode != 200 || json.Unmarshal(r.Body, &view) != nil {
			t.Fatalf("snapshot failed: %s", r.Body)
		}
		return view
	}
	until := time.Now().Add(3 * time.Second)
	for len(read().Accounts) != 2 {
		if time.Now().After(until) {
			t.Fatal("mixed inventory never published")
		}
		time.Sleep(time.Millisecond)
	}
	initial := read()
	if initial.Accounts["broken"].Enabled || initial.Capabilities.Inventory["broken"] != "account_identity_unavailable" {
		t.Fatal("invalid credential was not isolated")
	}
	if reader.calls.Load() != 0 {
		t.Fatal("default configuration queried without user action")
	}
	for _, selection := range []struct {
		body            string
		success, failed int
	}{
		{`{"account_id":"healthy"}`, 1, 0},
		{`{"account_id":"broken"}`, 0, 1},
		{`{}`, 1, 1},
	} {
		payload, _ := json.Marshal(managementRequest{Method: "POST", Path: ManagementPrefix + "/quota-query", Body: []byte(selection.body)})
		var accepted managementResponse
		_ = json.Unmarshal(rpcResult(t, s.Handle(context.Background(), "management.handle", payload)), &accepted)
		var reply struct {
			ID string `json:"job_id"`
		}
		if accepted.StatusCode != 202 || json.Unmarshal(accepted.Body, &reply) != nil || reply.ID == "" {
			t.Fatalf("query not accepted: %s", accepted.Body)
		}
		until = time.Now().Add(3 * time.Second)
		for {
			view := read()
			var job *scheduler.JobStatus
			for index := range view.Summary.Scheduler.Jobs {
				if view.Summary.Scheduler.Jobs[index].JobID == reply.ID {
					job = &view.Summary.Scheduler.Jobs[index]
					break
				}
			}
			if job == nil {
				t.Fatal("accepted job missing from immediate snapshot")
			}
			if job.Phase == "completed" {
				if job.QuotaQuery == nil || job.QuotaQuery.Completed != selection.success || job.QuotaQuery.Failed != selection.failed {
					t.Fatalf("unexpected query result %+v", job)
				}
				if selection.failed > 0 && job.QuotaQuery.LastError != "account_identity_unavailable" {
					t.Fatal("invalid account failure lost diagnostic")
				}
				if view.InstanceID != initial.InstanceID || view.Seq <= initial.Seq {
					t.Fatal("result not versioned in original instance")
				}
				break
			}
			if time.Now().After(until) {
				t.Fatalf("query remained %s", job.Phase)
			}
			time.Sleep(time.Millisecond)
		}
	}
	if reader.calls.Load() != 2 || callback.modelCalls.Load() != 0 {
		t.Fatal("queries duplicated transport work or sent a model request")
	}
}
