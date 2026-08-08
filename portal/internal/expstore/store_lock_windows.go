// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build windows

package expstore

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockRangeLen is the byte-length passed as both the low and high DWORD
// arguments to LockFileEx / UnlockFileEx. Together they describe a lock
// over byte range [0, 0xFFFFFFFF_FFFFFFFF) — a whole-file lock that
// matches flock(2)'s semantics. Kept as a single package-level const so
// tryFileLock and releaseFileLock cannot drift; a mismatch would make
// UnlockFileEx target a different range and silently fail.
const lockRangeLen uint32 = 0xFFFFFFFF

// tryFileLock attempts to acquire an exclusive, non-blocking lock on the
// entire content of f using LockFileEx. Returns nil on success,
// windows.ERROR_LOCK_VIOLATION (mapped from Win32 ERROR_LOCK_VIOLATION) when
// another process holds the lock (callers retry until storeLockTimeout
// elapses), or any other error fatally.
//
// We lock a maximum-length range so the lock guards any future appends to
// the same file (which is how the POSIX flock branch behaves).
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

// releaseFileLock removes the lock acquired by tryFileLock.
func releaseFileLock(f *os.File) error {
	handle := windows.Handle(f.Fd())
	var ol windows.Overlapped
	return windows.UnlockFileEx(handle, 0, lockRangeLen, lockRangeLen, &ol)
}

// isLockBusy reports whether err returned by tryFileLock means "another
// process holds the lock" and the caller should retry. On Windows,
// LockFileEx with LOCKFILE_FAIL_IMMEDIATELY returns ERROR_LOCK_VIOLATION (33)
// when the lock is held, or ERROR_IO_PENDING in some race windows.
func isLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_IO_PENDING)
}
