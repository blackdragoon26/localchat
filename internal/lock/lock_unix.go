//go:build unix

package lock

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// Lock holds an exclusive advisory file lock to guarantee a single running
// localchat instance per data directory.
type Lock struct {
	file *os.File
}

// Acquire acquires a non-blocking exclusive flock on path.
// Returns (lock, true, nil) on success, (nil, false, nil) if another process
// already holds the lock, or an error for unexpected filesystem failures.
func Acquire(path string) (*Lock, bool, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return &Lock{file: f}, true, nil
}

// Release releases the file lock and closes the descriptor without unlinking
// the lockfile inode (avoiding Unix unlink-then-lock races).
func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	errUnlock := syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	errClose := l.file.Close()
	l.file = nil
	if errUnlock != nil {
		return errUnlock
	}
	return errClose
}
