// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package fileutil

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func WriteFileAtomic(path string, raw []byte, perm os.FileMode) error {
	return writeFileAtomic(path, raw, perm, func(f *os.File) error {
		return f.Sync()
	})
}

func writeFileAtomic(path string, raw []byte, perm os.FileMode, syncFile func(*os.File) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && !ChmodUnsupported(err) {
		_ = tmp.Close()
		return err
	}
	if err := syncFile(tmp); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func ChmodUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
