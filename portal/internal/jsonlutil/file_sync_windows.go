//go:build windows

package jsonlutil

import "os"

func syncHistoryFile(_ *os.File) error {
	// FlushFileBuffers requires a write-capable handle; JSONL readers are
	// intentionally opened read-only. The cache-backed PVC path runs on Linux.
	return nil
}
