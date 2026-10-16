// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build windows

package jsonlutil

import (
	"os"

	"golang.org/x/sys/windows"
)

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
