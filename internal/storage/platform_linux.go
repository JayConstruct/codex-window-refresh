//go:build linux

package storage

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Geteuid())
}

func openProcessLock(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("storage: open process lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !ownedByCurrentUser(info) {
		_ = file.Close()
		return nil, errors.New("storage: lock must be an owned regular file with mode 0600")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("storage: acquire exclusive process lock: %w", err)
	}
	return file, nil
}

func checkProcessLock(file *os.File, path string) error {
	held, err := file.Stat()
	if err != nil {
		return errors.New("storage: lock file is inaccessible")
	}
	named, err := os.Lstat(path)
	if err != nil || !os.SameFile(held, named) || !named.Mode().IsRegular() || named.Mode().Perm() != 0600 || !ownedByCurrentUser(named) {
		return errors.New("storage: lock file changed or is inaccessible")
	}
	return nil
}

func closeProcessLock(file *os.File) error {
	unlockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}

func openStateRead(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
