// Package lock gives one process exclusive ownership of a state directory.
//
// The lock is an OS file lock (flock on Unix, LockFileEx on Windows), so it is released by the
// kernel when the holder exits, however it exits. There is no expiry and no stale-lock
// takeover: a live holder is never evicted, and a dead holder never blocks anyone.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrHeld is returned by Acquire when another process or handle holds the lock.
var ErrHeld = errors.New("lock: held by another process")

const fileName = "workstave.lock"

// Lock is a held state-directory lock.
type Lock struct {
	f *os.File
}

// Acquire creates dir if needed and takes the exclusive lock without waiting. It returns an
// error wrapping ErrHeld when the lock is already taken. Acquire it before opening the store
// and release it only after the store is closed.
func Acquire(dir string) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lock: create state directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, fileName), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock: open lock file: %w", err)
	}
	if err := tryLock(f); err != nil {
		_ = f.Close()
		if errors.Is(err, ErrHeld) {
			return nil, fmt.Errorf("lock: state directory %q is in use by another workstave process: %w", dir, ErrHeld)
		}
		return nil, fmt.Errorf("lock: lock %q: %w", dir, err)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock. It is safe to call more than once and on a nil Lock.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	f := l.f
	l.f = nil
	uerr := unlock(f)
	cerr := f.Close()
	if uerr != nil {
		return fmt.Errorf("lock: unlock: %w", uerr)
	}
	if cerr != nil {
		return fmt.Errorf("lock: close: %w", cerr)
	}
	return nil
}
