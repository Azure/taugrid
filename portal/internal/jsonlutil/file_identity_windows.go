// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build windows

package jsonlutil

import (
	"os"

	"golang.org/x/sys/windows"
)

// fileIdentity returns (volume-serial, file-index) for the open file f on
// Windows. The pair plays the same role as (device, inode) on POSIX in
// jsonlutil: ReadHistoryChunk uses it to detect rotation/truncation between
// tail reads.
//
// We query GetFileInformationByHandle on f's existing handle — no extra
// CreateFile, and no TOCTOU window between identity and the data read
// performed through the same handle. Any query error degrades to (0, 0) —
// callers treat that as "identity unknown" and keep the existing offset,
// matching the POSIX fallback.
func fileIdentity(f *os.File) (uint64, uint64) {
	handle := windows.Handle(f.Fd())
	var bi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &bi); err != nil {
		return 0, 0
	}
	device := uint64(bi.VolumeSerialNumber)
	inode := uint64(bi.FileIndexHigh)<<32 | uint64(bi.FileIndexLow)
	return device, inode
}
