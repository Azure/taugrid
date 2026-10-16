// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build windows

package datasetingest

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const lockRangeLen uint32 = 0xFFFFFFFF

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

func releaseFileLock(f *os.File) error {
	handle := windows.Handle(f.Fd())
	var ol windows.Overlapped
	return windows.UnlockFileEx(handle, 0, lockRangeLen, lockRangeLen, &ol)
}

func isLockBusy(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION) ||
		errors.Is(err, windows.ERROR_IO_PENDING)
}
