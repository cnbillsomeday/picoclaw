// PicoClaw - Ultra-lightweight personal AI agent

//go:build unix

package server

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// dataLock is an advisory exclusive lock on <data_dir>/.a2a.lock. The old
// server ignores unknown files, so the lock file does not affect rollback.
type dataLock struct {
	f *os.File
}

func acquireDataLock(dir string) (*dataLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".a2a.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf(
			"a2a server: data directory %s is already in use by another a2a server: %w",
			dir, err,
		)
	}
	return &dataLock{f: f}, nil
}

func (l *dataLock) Release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
