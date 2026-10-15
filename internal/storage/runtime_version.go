package storage

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

const RuntimeVersionFile = "runtime-version.json"

// PublishRuntimeVersion is separate from durable scheduler state. Only the
// current store owner can publish; a retired instance cannot replace a newer
// instance's receipt after releasing its state lock.
func (s *FileStore) PublishRuntimeVersion(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.check(context.Background()); err != nil {
		return err
	}
	if len(data) == 0 || len(data) > 16384 {
		return errors.New("invalid runtime version report size")
	}
	if err := checkProcessLock(s.lock, filepath.Join(s.dir, ".lock")); err != nil {
		return err
	}
	info, err := os.Lstat(s.dir)
	if err != nil || !os.SameFile(s.dirInfo, info) || !info.IsDir() || info.Mode().Perm() != 0700 || !ownedByCurrentUser(info) {
		return errors.New("runtime version directory changed")
	}
	tmp, err := os.CreateTemp(s.dir, ".runtime-version-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(data)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Rename(tmp.Name(), filepath.Join(s.dir, RuntimeVersionFile)); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
