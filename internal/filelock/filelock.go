// Package filelock provides cross-process advisory locks for JSON indexes.
package filelock

import (
	"os"
	"path/filepath"
	"syscall"
)

// Lock is an acquired advisory lock.
type Lock struct {
	f *os.File
}

// Acquire blocks until an exclusive lock for path+".lock" is held.
func Acquire(path string) (*Lock, error) {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return &Lock{f: f}, nil
}

// Close releases the lock.
func (l *Lock) Close() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
