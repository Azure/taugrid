// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !windows

package jsonlutil

import (
	"os"
	"syscall"
)

func fileIdentity(f *os.File) (uint64, uint64) {
	info, err := f.Stat()
	if err != nil {
		return 0, 0
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, 0
	}
	return uint64(stat.Dev), uint64(stat.Ino)
}
