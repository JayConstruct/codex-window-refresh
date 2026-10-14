package plugin

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
)

type connectionLifecycleHost struct {
	fakeHost
	closed atomic.Bool
}

func (host *connectionLifecycleHost) Close() error { host.closed.Store(true); return nil }

func TestQuotaConnectionsCloseAfterWorkerDrain(t *testing.T) {
	service, _, _, _ := setupService(t)
	host := &connectionLifecycleHost{}
	service.mu.Lock()
	service.host = host
	service.mu.Unlock()
	if err := service.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !host.closed.Load() {
		t.Fatal("quota connections leaked on normal shutdown")
	}
}

func TestFailedConfigurationReleasesQuotaConnections(t *testing.T) {
	host := &connectionLifecycleHost{}
	service := New(nil, Options{HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return host, nil },
		OpenStore:  func(string) (DurableStore, error) { return nil, errors.New("test store unavailable") },
		LoadConfig: func(string) (config.Config, error) { return config.Defaults(), nil },
		EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
			return &fakeEngine{}, nil
		},
	})
	err := service.configure(lifecyclePayload(t, "config_file: /tmp/test-policy.yaml\nstate_dir: /tmp/test-state\n", 6))
	if err == nil || !host.closed.Load() {
		t.Fatal("failed configuration leaked quota connections")
	}
}
