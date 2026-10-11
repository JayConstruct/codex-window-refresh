// Package storage provides durable, single-writer scheduler state.
package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync"

	"codex-window-refresh/internal/core"
)

var (
	ErrRevisionConflict = errors.New("storage: revision conflict")
	ErrExternalChange   = errors.New("storage: state changed outside this store")
	ErrClosed           = errors.New("storage: store is closed")
)

const maxStateBytes = 128 << 20

type envelope struct {
	Schema int             `json:"schema"`
	State  *core.CoreState `json:"state"`
}

// FileStore owns a directory and its cooperative process lock. All methods are
// serialized. Unknown/corrupt state and external edits fail closed; callers must
// stop scheduling when storage returns an error.
type FileStore struct {
	mu          sync.Mutex
	dir         string
	dirInfo     os.FileInfo
	lock        *os.File
	fingerprint [32]byte
	exists      bool
	poisoned    error
	closed      bool
	// Kept on the instance so uncertain commits can be tested without replacing
	// process-wide filesystem operations or weakening the public API.
	syncDirectory func(*os.File) error
}

var _ core.Store = (*FileStore)(nil)

// Open creates a private directory when absent. Existing directories must be
// owned by this user, mode 0700, and not symlinks. A second writer is rejected.
// It never replaces or resets an unreadable, corrupt, or incompatible state.
func Open(dir string) (*FileStore, error) {
	if dir == "" {
		return nil, errors.New("storage: empty directory")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("storage: resolve directory: %w", err)
	}
	if err := os.MkdirAll(abs, 0700); err != nil {
		return nil, fmt.Errorf("storage: create directory: %w", err)
	}
	info, err := os.Lstat(abs)
	if err != nil {
		return nil, fmt.Errorf("storage: stat directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
		return nil, errors.New("storage: directory must be an owned, non-symlink directory with mode 0700")
	}
	lock, err := openProcessLock(filepath.Join(abs, ".lock"))
	if err != nil {
		return nil, err
	}
	s := &FileStore{dir: abs, dirInfo: info, lock: lock, syncDirectory: (*os.File).Sync}
	_, sum, exists, err := s.readDisk()
	if err != nil {
		_ = closeProcessLock(lock)
		return nil, err
	}
	s.fingerprint, s.exists = sum, exists
	return s, nil
}

// Load reads a fresh state with independent maps, pointers, and slices. A new
// directory starts at revision zero with initialized Accounts and Activity.
func (s *FileStore) Load(ctx context.Context) (core.CoreState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return core.CoreState{}, err
	}
	state, err := s.readCurrent()
	if err != nil {
		return core.CoreState{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.CoreState{}, err
	}
	return state, nil
}

// Commit atomically persists next with expectedRevision+1. A returned revision
// means the file and directory fsync both succeeded. Cancellation is checked
// before replacement; once replaced, completion of the durable commit takes
// precedence over cancellation. Errors after replacement have uncertain commit
// status: this instance is poisoned and must be closed/reopened before recovery.
func (s *FileStore) Commit(ctx context.Context, expectedRevision uint64, next core.CoreState) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(ctx); err != nil {
		return 0, err
	}
	current, err := s.readCurrent()
	if err != nil {
		return 0, err
	}
	if current.Revision != expectedRevision {
		return 0, ErrRevisionConflict
	}
	if expectedRevision == math.MaxUint64 {
		return 0, errors.New("storage: revision overflow")
	}
	next.Revision = expectedRevision + 1
	initializeMaps(&next)
	data, err := json.Marshal(envelope{Schema: 1, State: &next})
	if err != nil {
		return 0, errors.New("storage: state cannot be encoded as JSON")
	}
	if len(data) > maxStateBytes {
		return 0, errors.New("storage: state exceeds size limit")
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return 0, fmt.Errorf("storage: create temporary state: %w", err)
	}
	tempPath := tmp.Name()
	defer os.Remove(tempPath)
	defer tmp.Close()
	if err := tmp.Chmod(0600); err != nil {
		return 0, fmt.Errorf("storage: set state permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return 0, fmt.Errorf("storage: write state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return 0, fmt.Errorf("storage: sync state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return 0, fmt.Errorf("storage: close state: %w", err)
	}
	// Recheck the pathname immediately before replacement. Cooperative writers
	// cannot race this check because the process lock remains held.
	if _, err := s.readCurrent(); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := os.Rename(tempPath, filepath.Join(s.dir, "state.json")); err != nil {
		return 0, fmt.Errorf("storage: replace state: %w", err)
	}
	directory, err := os.Open(s.dir)
	if err != nil {
		return 0, s.poison(fmt.Errorf("storage: open directory after replacement: %w", err))
	}
	syncErr := s.syncDirectory(directory)
	closeErr := directory.Close()
	if syncErr != nil {
		return 0, s.poison(fmt.Errorf("storage: sync directory after replacement: %w", syncErr))
	}
	if closeErr != nil {
		return 0, s.poison(fmt.Errorf("storage: close directory after replacement: %w", closeErr))
	}
	s.fingerprint, s.exists = sha256.Sum256(data), true
	return next.Revision, nil
}

func (s *FileStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return closeProcessLock(s.lock)
}

func (s *FileStore) check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return ErrClosed
	}
	if s.poisoned != nil {
		return s.poisoned
	}
	return nil
}

func (s *FileStore) poison(err error) error { s.poisoned = err; return err }

func (s *FileStore) readCurrent() (core.CoreState, error) {
	state, sum, exists, err := s.readDisk()
	if err != nil {
		return core.CoreState{}, s.poison(err)
	}
	if exists != s.exists || sum != s.fingerprint {
		return core.CoreState{}, s.poison(ErrExternalChange)
	}
	return state, nil
}

func (s *FileStore) readDisk() (core.CoreState, [32]byte, bool, error) {
	var zero [32]byte
	info, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(s.dirInfo, info) || !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
		return core.CoreState{}, zero, false, errors.New("storage: state directory changed or is inaccessible")
	}
	if err := checkProcessLock(s.lock, filepath.Join(s.dir, ".lock")); err != nil {
		return core.CoreState{}, zero, false, err
	}
	path := filepath.Join(s.dir, "state.json")
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		state := core.CoreState{}
		initializeMaps(&state)
		return state, zero, false, nil
	}
	if err != nil {
		return core.CoreState{}, zero, false, fmt.Errorf("storage: stat state: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ownedByCurrentUser(info) {
		return core.CoreState{}, zero, false, errors.New("storage: state must be an owned regular file with mode 0600")
	}
	f, err := openStateRead(path)
	if err != nil {
		return core.CoreState{}, zero, false, fmt.Errorf("storage: open state: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return core.CoreState{}, zero, false, errors.New("storage: state file changed during read")
	}
	data, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return core.CoreState{}, zero, false, fmt.Errorf("storage: read state: %w", err)
	}
	if len(data) > maxStateBytes {
		return core.CoreState{}, zero, false, errors.New("storage: state exceeds size limit")
	}
	var saved envelope
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&saved); err != nil {
		return core.CoreState{}, zero, false, errors.New("storage: corrupt or incompatible state JSON")
	}
	if saved.Schema != 1 || saved.State == nil {
		return core.CoreState{}, zero, false, errors.New("storage: unsupported schema or missing state")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return core.CoreState{}, zero, false, errors.New("storage: trailing state JSON")
	}
	initializeMaps(saved.State)
	return *saved.State, sha256.Sum256(data), true, nil
}

func initializeMaps(state *core.CoreState) {
	if state.Accounts == nil {
		state.Accounts = make(map[core.AccountID]core.AccountState)
	}
	if state.Activity == nil {
		state.Activity = make(map[core.UserIdentity]core.UserActivity)
	}
	if state.Requests == nil {
		state.Requests = make(map[core.RequestID]core.RequestActivity)
	}
}
