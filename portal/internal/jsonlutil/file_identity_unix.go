//go:build !windows

package jsonlutil

import (
	"os"
	"syscall"
)

// fileIdentity returns (device, inode) for the open file f on POSIX
// systems. Used by ReadHistoryChunk to detect file rotation/truncation
// between tail reads: if the (dev, ino) pair changed since the last
// checkpoint, the offset is reset to 0.
//
// f must be an already-open handle; the function calls f.Stat() (i.e.
// fstat(2)) so identity and any size/modtime obtained from the same
// handle are guaranteed consistent — no TOCTOU window with a separate
// os.Stat call. Returns (0, 0) when fstat fails or Sys() is not
// *syscall.Stat_t (exotic filesystems); callers treat that as
// "identity unknown" and keep the existing offset.
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
