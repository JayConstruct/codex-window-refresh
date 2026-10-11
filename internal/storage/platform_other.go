//go:build !linux

package storage

import (
	"errors"
	"os"
)

func ownedByCurrentUser(os.FileInfo) bool { return true }
func openProcessLock(string) (*os.File, error) {
	return nil, errors.New("storage: durable FileStore currently supports Linux only")
}
func checkProcessLock(*os.File, string) error { return errors.New("storage: unsupported platform") }
func closeProcessLock(*os.File) error         { return errors.New("storage: unsupported platform") }
func openStateRead(string) (*os.File, error)  { return nil, errors.New("storage: unsupported platform") }
