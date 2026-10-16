package plugin

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
	"codex-window-refresh/internal/storage"
)

func TestQuiesceHandsOffRealStateLockAndResumesAfterFailedReplacement(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	dir := t.TempDir() + "/state"
	payload := lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: "+dir+"\nstore:\n  version: 0.1.0-rc.20\n  release-tag: v0.1.0-rc.20\n", 6)
	started := 0
	s := New(&fakeCallback{}, Options{LoadConfig: func(string) (config.Config, error) { return cfg, nil },
		HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil },
		EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
			started++
			return &fakeEngine{}, nil
		}})
	t.Cleanup(func() { _ = s.Shutdown(ctx) })
	rpcResult(t, s.Handle(ctx, "plugin.register", payload))
	if second, err := storage.Open(dir); err == nil {
		second.Close()
		t.Fatal("two instances acquired the state lock")
	}
	rpcResult(t, s.Handle(ctx, "plugin.quiesce", nil))
	if s.engine != nil || s.host != nil || s.store != nil {
		t.Fatal("retired library retains its engine, account state or host")
	}
	rpcResult(t, s.Handle(ctx, "plugin.quiesce", nil))
	phase := managementCall(t, s, "GET", ManagementPrefix+"/lifecycle")
	var status struct{ Phase string }
	if json.Unmarshal(phase.Body, &status) != nil || status.Phase != "quiesced" {
		t.Fatal("pause did not publish lifecycle status")
	}
	second, err := storage.Open(dir)
	if err != nil {
		t.Fatal("quiesce retained state lock", err)
	}
	if err := s.configure(payload); err == nil {
		t.Fatal("rollback reopened state owned by the replacement")
	}
	second.Close()
	rpcResult(t, s.Handle(ctx, "plugin.reconfigure", payload))
	if started != 2 {
		t.Fatal("rollback reused closed runtime resources")
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if s.engine != nil {
		t.Fatal("shutdown retains scheduler state")
	}
	if err := s.configure(payload); err == nil {
		t.Fatal("final shutdown was incorrectly resumable")
	}
}

func TestQuiesceTimeoutRetainsOwnershipUntilRollbackCanDrain(t *testing.T) {
	ctx := context.Background()
	cfg := config.Defaults()
	dir := t.TempDir() + "/state"
	payload := lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: "+dir+"\n", 6)
	blocked := &fakeEngine{block: make(chan struct{})}
	count := 0
	s := New(&fakeCallback{}, Options{ShutdownTimeout: time.Millisecond,
		LoadConfig:  func(string) (config.Config, error) { return cfg, nil },
		HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil },
		EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
			count++
			if count == 1 {
				return blocked, nil
			}
			return &fakeEngine{}, nil
		}})
	rpcResult(t, s.Handle(ctx, "plugin.register", payload))
	if err := s.Quiesce(ctx); err == nil {
		t.Fatal("undrained callback reported a completed pause")
	}
	if second, err := storage.Open(dir); err == nil {
		second.Close()
		t.Fatal("timed-out quiesce released exclusive ownership")
	}
	close(blocked.block)
	s.options.ShutdownTimeout = time.Second
	rpcResult(t, s.Handle(ctx, "plugin.reconfigure", payload))
	if count != 2 {
		t.Fatal("rollback failed to create a fresh runtime")
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
