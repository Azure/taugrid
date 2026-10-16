// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build windows

package jsonlutil

import "os"

func syncHistoryFile(_ *os.File) error {
	return nil
}
