//go:build windows

package datasetingest

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const lockRangeLen uint32 = 0xFFFFFFFF

// tryFileLock attempts to acquire an exclusive, non-blocking lock on the
// entire file using LockFileEx. Returns nil on success, ERROR_LOCK_VIOLATION
// when another process holds the lock, or another error fatally.
func tryFileLock(f *os.File) error {
	handle := windows.Handle(f.Fd())
	var ol windows.Overlapped
	return windows.LockFileEx(
		handle,
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		lockRangeLen, lockRangeLen,
		&ol,
	)
}

// releaseFileLock releases the lock acquired by tryFileLock.
func releaseFileLock(f *os.File) error {
	handle := windows.Handle(f.Fd())
	var ol windows.Overlapped
	return windows.UnlockFileEx(handle, 0, lockRangeLen, lockRangeLen, &ol)
}

// isLockBusy reports whether err means another process holds the lock.
func isLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_IO_PENDING)
}
