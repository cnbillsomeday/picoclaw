// PicoClaw - Ultra-lightweight personal AI agent

//go:build !unix

package server

import (
	"fmt"
	"os"
	"path/filepath"
)

// dataLock is a best-effort lock file on platforms without flock. It prevents
// concurrent starts but can go stale if the process crashes.
type dataLock struct {
	path string
	f    *os.File
}

func acquireDataLock(dir string) (*dataLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".a2a.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf(
				"a2a server: data directory %s is already in use (lock file %s exists; "+
					"remove it if no server is running)",
				dir, path,
			)
		}
		return nil, err
	}
	return &dataLock{path: path, f: f}, nil
}

func (l *dataLock) Release() {
	if l == nil {
		return
	}
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
	if l.path != "" {
		_ = os.Remove(l.path)
		l.path = ""
	}
}
