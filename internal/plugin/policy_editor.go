package plugin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"codex-window-refresh/internal/config"
	"gopkg.in/yaml.v3"
)

const maxPolicyBytes = 2 << 20

// These local hooks exercise failures at the filesystem commit boundary without
// using a live scheduler or host. Production uses the normal atomic operations.
type policyFileOps struct {
	rename        func(string, string) error
	syncDirectory func(string) error
}

func (s *Service) editorOps() policyFileOps {
	ops := policyFileOps{rename: os.Rename, syncDirectory: syncPolicyDirectory}
	if s.policyOps != nil {
		if s.policyOps.rename != nil {
			ops.rename = s.policyOps.rename
		}
		if s.policyOps.syncDirectory != nil {
			ops.syncDirectory = s.policyOps.syncDirectory
		}
	}
	return ops
}
func policyRevision(raw []byte) string { v := sha256.Sum256(raw); return hex.EncodeToString(v[:]) }
func policyFormat(path string) string {
	return strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), ".")
}
func policyError(status int, message string) managementResponse {
	return response(status, map[string]string{"error": message})
}

// Read without following the final symlink, and verify that the name still
// references the opened inode. Parents must be real directories as well.
func readPolicyFile(path string) ([]byte, os.FileInfo, error) {
	if !filepath.IsAbs(path) {
		return nil, nil, errors.New("invalid policy path")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		info, err := os.Lstat(parent)
		if err != nil || !info.IsDir() {
			return nil, nil, errors.New("invalid policy directory")
		}
		if parent == filepath.Dir(parent) {
			break
		}
	}
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, nil, errors.New("policy must be a regular file")
	}
	if info.Size() > maxPolicyBytes {
		return nil, nil, errors.New("policy is too large")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxPolicyBytes+1))
	if err != nil || len(raw) > maxPolicyBytes {
		return nil, nil, errors.New("policy read failed")
	}
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(info, named) || !named.Mode().IsRegular() {
		return nil, nil, errors.New("policy file changed")
	}
	return raw, info, nil
}
func (s *Service) readPolicyEditor() managementResponse {
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	s.mu.RLock()
	path, running := s.wrapper.ConfigFile, s.cfg
	s.mu.RUnlock()
	raw, _, err := readPolicyFile(path)
	if err != nil {
		return policyError(500, "policy file cannot be read safely")
	}
	cfg, err := config.Parse(raw, policyFormat(path))
	if err != nil {
		return policyError(400, "policy file failed strict validation")
	}
	return response(200, map[string]any{"config": cfg, "revision": policyRevision(raw), "disk_matches_running": reflect.DeepEqual(cfg, running)})
}

func parsePolicyEdit(raw []byte) (config.Config, string, error) {
	if len(raw) > 1<<20 {
		return config.Config{}, "", errors.New("request too large")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return config.Config{}, "", errors.New("expected object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return config.Config{}, "", err
		}
		key, ok := tok.(string)
		if !ok || (key != "config" && key != "revision") || fields[key] != nil {
			return config.Config{}, "", errors.New("invalid or duplicate field")
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return config.Config{}, "", err
		}
		fields[key] = value
	}
	if _, err := dec.Token(); err != nil {
		return config.Config{}, "", err
	}
	if _, err := dec.Token(); err != io.EOF {
		return config.Config{}, "", errors.New("extra JSON value")
	}
	var revision string
	if json.Unmarshal(fields["revision"], &revision) != nil || len(revision) != 64 {
		return config.Config{}, "", errors.New("invalid revision")
	}
	if _, err := hex.DecodeString(revision); err != nil || revision != strings.ToLower(revision) {
		return config.Config{}, "", errors.New("invalid revision")
	}
	cfg, err := config.Parse(fields["config"], "json")
	if err != nil {
		return config.Config{}, "", err
	}
	if cfg.Activity.IdentitySource != "host_caller_scope" {
		return config.Config{}, "", errors.New("identity source unsupported by CPA adapter")
	}
	return cfg, revision, nil
}
func encodePolicy(cfg config.Config, format string) ([]byte, error) {
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, err
	}
	if format == "json" {
		return append(raw, '\n'), nil
	}
	if format != "yaml" && format != "yml" {
		return nil, errors.New("unsupported policy format")
	}
	// Convert using JSON tags and the Duration JSON serializer; direct YAML
	// marshaling of Config would otherwise persist raw nanosecond integers.
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return yaml.Marshal(value)
}
func stagePolicy(path string, raw []byte, original os.FileInfo) (string, error) {
	f, err := os.CreateTemp(filepath.Dir(path), ".window-refresh-policy-*.tmp")
	if err != nil {
		return "", err
	}
	name := f.Name()
	keep := false
	defer func() {
		_ = f.Close()
		if !keep {
			_ = os.Remove(name)
		}
	}()
	stat, ok := original.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("policy ownership unavailable")
	}
	current, err := f.Stat()
	if err != nil {
		return "", err
	}
	owner, ok := current.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("temporary ownership unavailable")
	}
	if owner.Uid != stat.Uid || owner.Gid != stat.Gid {
		if err := f.Chown(int(stat.Uid), int(stat.Gid)); err != nil {
			return "", err
		}
	}
	if err := f.Chmod(original.Mode()); err != nil {
		return "", err
	}
	if _, err := f.Write(raw); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	keep = true
	return name, nil
}
func syncPolicyDirectory(path string) error {
	f, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
func policyUnchanged(path string, original []byte, info os.FileInfo) bool {
	raw, current, err := readPolicyFile(path)
	return err == nil && bytes.Equal(raw, original) && os.SameFile(info, current) && info.Mode() == current.Mode() && samePolicyOwner(info, current)
}
func samePolicyOwner(a, b os.FileInfo) bool {
	x, ok := a.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	y, ok := b.Sys().(*syscall.Stat_t)
	return ok && x.Uid == y.Uid && x.Gid == y.Gid
}
func (s *Service) savePolicyEditor(ctx context.Context, body []byte) managementResponse {
	cfg, expected, err := parsePolicyEdit(body)
	if err != nil {
		return policyError(400, "invalid policy: use valid schema fields, values, and a current revision")
	}
	s.lifecycle.Lock()
	defer s.lifecycle.Unlock()
	if ctx.Err() != nil {
		return policyError(408, "save request canceled before commit")
	}
	s.mu.RLock()
	wrapper, engine, stopping := s.wrapper, s.engine, s.stopping
	s.mu.RUnlock()
	if engine == nil || stopping {
		return policyError(409, "scheduler unavailable or stopping")
	}
	old, info, err := readPolicyFile(wrapper.ConfigFile)
	if err != nil {
		return policyError(500, "policy file cannot be read safely")
	}
	if policyRevision(old) != expected {
		return policyError(409, "policy changed; reload the editor before saving")
	}
	updated, err := encodePolicy(cfg, policyFormat(wrapper.ConfigFile))
	if err != nil {
		return policyError(400, "unsupported policy file format")
	}
	// Re-validate the exact persisted representation before any replacement.
	if _, err := config.Parse(updated, policyFormat(wrapper.ConfigFile)); err != nil {
		return policyError(400, "policy cannot be persisted safely")
	}
	newPath, err := stagePolicy(wrapper.ConfigFile, updated, info)
	if err != nil {
		return policyError(500, "policy staging failed; previous configuration retained")
	}
	defer os.Remove(newPath)
	rollbackPath, err := stagePolicy(wrapper.ConfigFile, old, info)
	if err != nil {
		return policyError(500, "policy rollback staging failed; previous configuration retained")
	}
	keepRollback := false
	defer func() {
		if !keepRollback {
			_ = os.Remove(rollbackPath)
		}
	}()
	if !policyUnchanged(wrapper.ConfigFile, old, info) {
		return policyError(409, "policy changed; reload the editor before saving")
	}
	if ctx.Err() != nil {
		return policyError(408, "save request canceled before commit")
	}
	ops := s.editorOps()
	if err := ops.rename(newPath, wrapper.ConfigFile); err != nil {
		return policyError(500, "policy replacement failed; previous configuration retained")
	}
	rollback := func() managementResponse {
		// Refuse to overwrite a different edit made after our replacement.
		current, _, readErr := readPolicyFile(wrapper.ConfigFile)
		if readErr != nil || !bytes.Equal(current, updated) || ops.rename(rollbackPath, wrapper.ConfigFile) != nil || ops.syncDirectory(wrapper.ConfigFile) != nil {
			keepRollback = true // Preserve the original bytes for manual recovery.
			s.mu.Lock()
			s.lastError = "policy_save_recovery_required"
			s.mu.Unlock()
			return policyError(500, "policy save failed; running configuration retained, check the policy file and .window-refresh-policy-*.tmp recovery copy")
		}
		return policyError(500, "policy save failed; previous file and running configuration retained")
	}
	// Commit disk first: a failed write must never briefly enable scheduling.
	if err := ops.syncDirectory(wrapper.ConfigFile); err != nil {
		return rollback()
	}
	if err := engine.UpdateConfig(wrapper.effective(cfg)); err != nil {
		return rollback()
	}
	s.mu.Lock()
	s.cfg = cfg
	if s.lastError == "policy_save_recovery_required" {
		s.lastError = ""
	}
	s.mu.Unlock()
	return response(200, map[string]any{"config": cfg, "revision": policyRevision(updated)})
}
