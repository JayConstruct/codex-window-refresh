package ipc

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"codex-window-refresh/internal/v2/hostlink"
)

type Listener struct {
	*net.UnixListener
	lock *os.File
}

func (l *Listener) Close() error { err := l.UnixListener.Close(); l.lock.Close(); return err }
func (l *Listener) Accept() (net.Conn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		if hostlink.PeerOwned(conn) == nil {
			return conn, nil
		}
		conn.Close()
	}
}
func Listen(dir string) (*Listener, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("runtime_dir_must_be_absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("runtime_dir_must_be_private_and_owned")
	}
	fd, err := syscall.Open(filepath.Join(dir, ".daemon-socket.lock"), syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(fd), "socket lock")
	if err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("daemon_socket_already_owned")
	}
	path := filepath.Join(dir, "daemon.sock")
	if old, e := os.Lstat(path); e == nil {
		if old.Mode()&os.ModeSocket == 0 || old.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			lock.Close()
			return nil, errors.New("unsafe_existing_socket")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		conn, dialErr := (&net.Dialer{}).DialContext(ctx, "unix", path)
		cancel()
		if dialErr == nil {
			conn.Close()
			lock.Close()
			return nil, errors.New("daemon_socket_alive")
		}
		if err = os.Remove(path); err != nil {
			lock.Close()
			return nil, err
		}
	} else if !os.IsNotExist(e) {
		lock.Close()
		return nil, e
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		lock.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		listener.Close()
		lock.Close()
		return nil, err
	}
	return &Listener{listener, lock}, nil
}
