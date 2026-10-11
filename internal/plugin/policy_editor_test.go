package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"codex-window-refresh/internal/config"
)

func setupEditor(t *testing.T, format string) (*Service, *fakeEngine, string, []byte) {
	t.Helper()
	s, engine, _, _ := setupService(t)
	path := filepath.Join(t.TempDir(), "policy."+format)
	raw, err := encodePolicy(s.cfg, format)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0640); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.wrapper.ConfigFile = path
	s.mu.Unlock()
	return s, engine, path, raw
}
func editorPost(t *testing.T, s *Service, body []byte) managementResponse {
	t.Helper()
	payload, _ := json.Marshal(managementRequest{Method: "POST", Path: ManagementPrefix + "/config", Body: body})
	var result managementResponse
	if err := json.Unmarshal(rpcResult(t, s.Handle(context.Background(), "management.handle", payload)), &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func editorBody(t *testing.T, cfg config.Config, revision string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"config": cfg, "revision": revision})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
func assertEditorUnchanged(t *testing.T, s *Service, path string, old []byte, cfg config.Config) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, old) {
		t.Fatalf("policy changed after failure: %v", err)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !reflect.DeepEqual(s.cfg, cfg) {
		t.Fatal("running service config changed after failure")
	}
}
func TestPolicyEditorSaveFormatsAndMetadata(t *testing.T) {
	for _, format := range []string{"yaml", "json"} {
		t.Run(format, func(t *testing.T) {
			s, engine, path, old := setupEditor(t, format)
			info, _ := os.Stat(path)
			cfg := s.cfg
			cfg.Enabled = false
			cfg.Execution.Prompt = "custom <prompt>"
			result := editorPost(t, s, editorBody(t, cfg, policyRevision(old)))
			if result.StatusCode != 200 {
				t.Fatalf("save failed: %s", result.Body)
			}
			raw, _ := os.ReadFile(path)
			parsed, err := config.Parse(raw, format)
			if err != nil || !reflect.DeepEqual(parsed, cfg) {
				t.Fatalf("saved representation invalid: %v", err)
			}
			updated, _ := os.Stat(path)
			if updated.Mode() != info.Mode() || !samePolicyOwner(info, updated) {
				t.Fatal("permissions or ownership changed")
			}
			engine.mu.Lock()
			defer engine.mu.Unlock()
			if !reflect.DeepEqual(engine.updated, cfg) {
				t.Fatal("engine not hot updated")
			}
			var reply struct {
				Revision string
				Config   config.Config
			}
			_ = json.Unmarshal(result.Body, &reply)
			if reply.Revision != policyRevision(raw) || !reflect.DeepEqual(reply.Config, cfg) {
				t.Fatal("revision or config response incorrect")
			}
		})
	}
}
func TestPolicyEditorShowsDiskSnapshot(t *testing.T) {
	s, _, path, _ := setupEditor(t, "yaml")
	cfg := s.cfg
	cfg.Enabled = false
	raw, _ := encodePolicy(cfg, "yaml")
	_ = os.WriteFile(path, raw, 0640)
	reply := managementCall(t, s, "GET", ManagementPrefix+"/config-editor")
	var got struct {
		Config   config.Config
		Revision string
		Matches  bool `json:"disk_matches_running"`
	}
	if reply.StatusCode != 200 || json.Unmarshal(reply.Body, &got) != nil || got.Config.Enabled || got.Matches || got.Revision != policyRevision(raw) {
		t.Fatalf("did not expose disk snapshot: %s", reply.Body)
	}
	active := managementCall(t, s, "GET", ManagementPrefix+"/config")
	var activeCfg config.Config
	_ = json.Unmarshal(active.Body, &activeCfg)
	if !activeCfg.Enabled {
		t.Fatal("legacy endpoint stopped showing running config")
	}
}
func TestPolicyEditorRejectsConflictAndInvalidInput(t *testing.T) {
	s, _, path, old := setupEditor(t, "json")
	initial := s.cfg
	valid := editorBody(t, initial, policyRevision(old))
	cases := [][]byte{
		editorBody(t, initial, strings.Repeat("0", 64)),
		[]byte(`{"config":{},"revision":"` + policyRevision(old) + `","revision":"` + policyRevision(old) + `"}`),
		[]byte(`{"config":{},"revision":"` + policyRevision(old) + `","extra":true}`),
		[]byte(`{"config":{"execution":{"model":"x"},"enabled":false,"enabled":true},"revision":"` + policyRevision(old) + `"}`),
		[]byte(`{"config":{"execution":{"model":"x"},"unknown_secret":"DO_NOT_ECHO"},"revision":"` + policyRevision(old) + `"}`),
		[]byte(`{"config":null,"revision":"` + policyRevision(old) + `"}`),
		append(valid, []byte(` {}`)...),
		bytes.Repeat([]byte("x"), (1<<20)+1),
	}
	cfg := initial
	cfg.Activity.IdentitySource = "host_user"
	cases = append(cases, editorBody(t, cfg, policyRevision(old)))
	for i, body := range cases {
		reply := editorPost(t, s, body)
		want := 400
		if i == 0 {
			want = 409
		}
		if reply.StatusCode != want {
			t.Fatalf("case %d status %d: %s", i, reply.StatusCode, reply.Body)
		}
		if strings.Contains(string(reply.Body), "DO_NOT_ECHO") {
			t.Fatal("response leaked request content")
		}
		assertEditorUnchanged(t, s, path, old, initial)
	}
}

type failedPolicyEngine struct{ *fakeEngine }

func (e failedPolicyEngine) UpdateConfig(config.Config) error { return errors.New("private failure") }
func TestPolicyEditorRollbackOnApplyAndFilesystemFailures(t *testing.T) {
	for _, failure := range []string{"apply", "rename", "sync"} {
		t.Run(failure, func(t *testing.T) {
			s, engine, path, old := setupEditor(t, "yaml")
			initial := s.cfg
			cfg := initial
			cfg.Enabled = false
			if failure == "apply" {
				s.engine = failedPolicyEngine{engine}
			}
			if failure == "rename" {
				s.policyOps = &policyFileOps{rename: func(string, string) error { return errors.New("failure") }}
			}
			if failure == "sync" {
				calls := 0
				s.policyOps = &policyFileOps{syncDirectory: func(p string) error {
					calls++
					if calls == 1 {
						return errors.New("failure")
					}
					return syncPolicyDirectory(p)
				}}
			}
			reply := editorPost(t, s, editorBody(t, cfg, policyRevision(old)))
			if reply.StatusCode != 500 {
				t.Fatalf("failure was reported as success: %s", reply.Body)
			}
			assertEditorUnchanged(t, s, path, old, initial)
			engine.mu.Lock()
			defer engine.mu.Unlock()
			if engine.updated.SchemaVersion != 0 {
				t.Fatal("engine updated before disk commit succeeded")
			}
			files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".window-refresh-policy-*"))
			if len(files) != 0 {
				t.Fatal("temporary files left behind")
			}
		})
	}
}
func TestPolicyEditorConflictDuringStaging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	old := []byte("old")
	_ = os.WriteFile(path, old, 0600)
	info, _ := os.Stat(path)
	_ = os.WriteFile(path, []byte("external edit"), 0600)
	if policyUnchanged(path, old, info) {
		t.Fatal("concurrent writer not detected")
	}
}
func TestPolicyEditorRejectsSymlinksAndSpecialFiles(t *testing.T) {
	s, _, path, old := setupEditor(t, "yaml")
	target := path + ".real"
	_ = os.Rename(path, target)
	_ = os.Symlink(target, path)
	if reply := editorPost(t, s, editorBody(t, s.cfg, policyRevision(old))); reply.StatusCode != 500 {
		t.Fatal("symlink accepted")
	}
	if raw, _ := os.ReadFile(target); !bytes.Equal(raw, old) {
		t.Fatal("symlink target modified")
	}
	_ = os.Remove(path)
	if err := syscall.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readPolicyFile(path); err == nil {
		t.Fatal("FIFO accepted")
	}
}
func TestPolicyEditorPreservesHostDisabled(t *testing.T) {
	s, engine, _, old := setupEditor(t, "yaml")
	disabled := false
	s.wrapper.HostEnabled = &disabled
	result := editorPost(t, s, editorBody(t, s.cfg, policyRevision(old)))
	if result.StatusCode != 200 {
		t.Fatal(string(result.Body))
	}
	engine.mu.Lock()
	defer engine.mu.Unlock()
	if engine.updated.Enabled || !s.cfg.Enabled {
		t.Fatal("host disable lost during hot update")
	}
}

func TestPolicyEditorRetainsRecoveryCopyWhenRollbackFails(t *testing.T) {
	s, engine, path, old := setupEditor(t, "yaml")
	initial := s.cfg
	s.engine = failedPolicyEngine{engine}
	calls := 0
	s.policyOps = &policyFileOps{rename: func(from, to string) error {
		calls++
		if calls == 2 {
			return errors.New("rollback failed")
		}
		return os.Rename(from, to)
	}}
	cfg := initial
	cfg.Enabled = false
	reply := editorPost(t, s, editorBody(t, cfg, policyRevision(old)))
	if reply.StatusCode != 500 || s.lastError != "policy_save_recovery_required" {
		t.Fatal("recovery failure hidden")
	}
	if !reflect.DeepEqual(s.cfg, initial) {
		t.Fatal("running policy changed")
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".window-refresh-policy-*.tmp"))
	if len(files) != 1 {
		t.Fatalf("expected preserved recovery file: %v", files)
	}
	raw, _ := os.ReadFile(files[0])
	if !bytes.Equal(raw, old) {
		t.Fatal("original bytes lost")
	}
}
