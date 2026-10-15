package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-window-refresh/internal/config"
	"codex-window-refresh/internal/core"
	"codex-window-refresh/internal/cpa"
	"codex-window-refresh/internal/storage"
)

func TestRuntimeVersionFileAcknowledgesConfigurationAndHandoff(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(dir, storage.RuntimeVersionFile)
	cfg := config.Defaults()
	newService := func(library string) *Service {
		return New(&fakeCallback{}, Options{LibraryPath: library,
			LoadConfig:  func(string) (config.Config, error) { return cfg, nil },
			HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil },
			EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
				return &fakeEngine{}, nil
			}})
	}
	first := newService("/tmp/version-a.so")
	second := newService("/tmp/version-b.so")
	t.Cleanup(func() { _ = first.Shutdown(ctx); _ = second.Shutdown(ctx) })
	payload := func(token string) []byte {
		return lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: "+dir+"\nstore:\n  local-reload-token: "+token+"\n", 6)
	}
	read := func() runtimeVersion {
		t.Helper()
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report runtimeVersion
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		return report
	}
	rpcResult(t, first.Handle(ctx, "plugin.register", payload(strings.Repeat("a", 64))))
	a := read()
	if a.Protocol != 2 || a.Phase != "running" || a.Version != Version || a.PID != os.Getpid() ||
		a.BootID == "" || a.ProcessStart == "" || a.InstanceID == "" || a.ReloadToken != strings.Repeat("a", 64) || a.LibraryPath != "/tmp/version-a.so" {
		t.Fatal("running receipt omitted live instance/configuration identity", a)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("version receipt is not private")
	}
	rpcResult(t, first.Handle(ctx, "plugin.reconfigure", payload(strings.Repeat("b", 64))))
	b := read()
	if b.InstanceID != a.InstanceID || b.ReloadToken != strings.Repeat("b", 64) {
		t.Fatal("reconfigure failed to acknowledge the same running instance")
	}
	rpcResult(t, first.Handle(ctx, "plugin.quiesce", nil))
	if read().Phase != "quiesced" {
		t.Fatal("pause left a stale running receipt")
	}
	rpcResult(t, second.Handle(ctx, "plugin.register", payload(strings.Repeat("c", 64))))
	c := read()
	if c.InstanceID == b.InstanceID || c.LibraryPath != "/tmp/version-b.so" {
		t.Fatal("replacement reused the old runtime identity")
	}
	if err := first.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if read().InstanceID != c.InstanceID || read().Phase != "running" {
		t.Fatal("retired shutdown overwrote the new owner's receipt")
	}
	if err := second.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if read().Phase != "stopped" {
		t.Fatal("final shutdown left a running receipt")
	}
}

func TestWorkerFailurePublishesStoppedAndCannotAcknowledgeReload(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	cfg := config.Defaults()
	s := New(&fakeCallback{}, Options{
		LoadConfig:  func(string) (config.Config, error) { return cfg, nil },
		HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil },
		EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
			return &backgroundPanicEngine{}, nil
		}})
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	payload := lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: "+dir+"\n", 6)
	rpcResult(t, s.Handle(context.Background(), "plugin.register", payload))
	s.mu.RLock()
	done := s.done
	s.mu.RUnlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not finish")
	}
	data, err := os.ReadFile(filepath.Join(dir, storage.RuntimeVersionFile))
	if err != nil || !strings.Contains(string(data), `"phase":"stopped"`) {
		t.Fatal("worker failure left an active receipt", err)
	}
	if err := s.configure(payload); err == nil {
		t.Fatal("failed runtime acknowledged a new configuration")
	}
}

type failedVersionStore struct{ *storage.FileStore }

func (failedVersionStore) PublishRuntimeVersion([]byte) error {
	return errors.New("fixture write failure")
}

func TestVersionFileWriteFailureRejectsRegistrationAndReleasesStateLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	cfg := config.Defaults()
	s := New(&fakeCallback{}, Options{
		LoadConfig:  func(string) (config.Config, error) { return cfg, nil },
		HostFactory: func(cpa.Callback, WrapperConfig) (Host, error) { return fakeHost{}, nil },
		OpenStore: func(path string) (DurableStore, error) {
			store, err := storage.Open(path)
			if err != nil {
				return nil, err
			}
			return failedVersionStore{store}, nil
		},
		EngineFactory: func(config.Config, core.Host, core.Store, core.Clock, core.Random) (Engine, error) {
			return &fakeEngine{}, nil
		}})
	payload := lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: "+dir+"\n", 6)
	if err := s.configure(payload); err == nil {
		t.Fatal("registration succeeded without publishing its runtime receipt")
	}
	store, err := storage.Open(dir)
	if err != nil {
		t.Fatal("failed registration retained the state lock", err)
	}
	store.Close()
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
}
