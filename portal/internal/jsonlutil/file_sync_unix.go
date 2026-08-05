//go:build !windows

package jsonlutil

import "os"

func syncHistoryFile(f *os.File) error {
	return f.Sync()
}
