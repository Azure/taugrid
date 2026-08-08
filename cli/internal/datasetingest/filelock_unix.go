// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !windows

package datasetingest

import (
	"errors"
	"os"
	"syscall"
)

// tryFileLock attempts to acquire an exclusive, non-blocking advisory lock on f.
// Returns nil on success, syscall.EWOULDBLOCK/EAGAIN if another process holds
// the lock (caller retries), or any other error fatally.
func tryFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// releaseFileLock releases the advisory lock acquired by tryFileLock.
func releaseFileLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// isLockBusy reports whether the error from tryFileLock means the lock is
// held by another process (caller should retry) vs a fatal I/O failure.
func isLockBusy(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
