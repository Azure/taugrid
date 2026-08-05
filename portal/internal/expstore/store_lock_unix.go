//go:build !windows

package expstore

import (
	"errors"
	"os"
	"syscall"
)

// tryFileLock attempts to acquire an exclusive, non-blocking advisory lock on
// f using flock(2). Returns nil on success, syscall.EWOULDBLOCK / EAGAIN if
// another process holds the lock (callers retry until storeLockTimeout
// elapses), or any other error fatally.
func tryFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// releaseFileLock removes the advisory lock acquired by tryFileLock. Closing
// the file would release the lock implicitly, but we call LOCK_UN explicitly
// to surface unlock errors separately from close errors.
func releaseFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// isLockBusy reports whether err returned by tryFileLock means "another
// process holds the lock" and the caller should retry, rather than a fatal
// I/O failure.
func isLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
