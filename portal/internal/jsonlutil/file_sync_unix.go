// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build !windows

package jsonlutil

import "os"

func syncHistoryFile(f *os.File) error {
	return f.Sync()
}
