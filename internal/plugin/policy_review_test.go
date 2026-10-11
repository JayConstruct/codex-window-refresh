package plugin

import (
	"context"
	"encoding/json"
	"testing"

	"codex-window-refresh/internal/config"
)

// A browser leaving while an edit waits for another lifecycle operation must
// not turn that abandoned request into a later enable/disable of scheduling.
func TestCanceledPolicySaveWhileLifecycleBusy(t *testing.T) {
	s, _, path, old := setupEditor(t, "yaml")
	previous := s.cfg
	updated := previous
	updated.Enabled = !previous.Enabled
	body := editorBody(t, updated, policyRevision(old))
	ctx, cancel := context.WithCancel(context.Background())
	s.lifecycle.Lock()
	started := make(chan struct{})
	done := make(chan managementResponse, 1)
	go func() {
		close(started)
		done <- s.savePolicyEditor(ctx, body)
	}()
	<-started
	cancel()
	s.lifecycle.Unlock()
	reply := <-done
	if reply.StatusCode < 400 {
		t.Fatalf("abandoned request committed: %s", reply.Body)
	}
	assertEditorUnchanged(t, s, path, old, previous)
}

// Reconfigure and the editor share a lifecycle transaction. Loading before
// that transaction can apply a stale snapshot over a newer saved policy.
func TestReconfigureLoadsPolicyInsideLifecycleTransaction(t *testing.T) {
	s, _, _, _ := setupService(t)
	cfg := s.cfg
	cfg.Enabled = false
	loaded := false
	s.options.LoadConfig = func(string) (config.Config, error) {
		loaded = true
		if s.lifecycle.TryLock() {
			s.lifecycle.Unlock()
			t.Error("policy snapshot loaded outside the lifecycle transaction")
		}
		return cfg, nil
	}
	result := s.Handle(context.Background(), "plugin.reconfigure", lifecyclePayload(t, "config_file: /tmp/policy.yaml\nstate_dir: /tmp/scheduler-state\n", 6))
	var envelope rpcEnvelope
	if err := json.Unmarshal(result, &envelope); err != nil || !envelope.OK || !loaded {
		t.Fatalf("reconfigure failed: %s", result)
	}
	if s.cfg.Enabled {
		t.Fatal("fresh loaded policy was not applied")
	}
}
