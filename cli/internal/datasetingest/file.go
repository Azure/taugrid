// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package datasetingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type FileSource struct {
	Root string
}

func (s FileSource) Open(_ context.Context, relPath string) (io.ReadCloser, int64, error) {
	full, err := s.safePath(relPath)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, fmt.Errorf("source file not found: %s", relPath)
		}
		return nil, 0, fmt.Errorf("open source %s: %w", relPath, err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}

func (s FileSource) Describe() string { return "file://" + s.Root }

func (s FileSource) safePath(relPath string) (string, error) {
	clean := filepath.FromSlash(relPath)
	if strings.Contains(clean, "..") {
		for _, seg := range strings.Split(relPath, "/") {
			if seg == ".." {
				return "", fmt.Errorf("path %q contains '..' traversal", relPath)
			}
		}
	}
	full := filepath.Join(s.Root, clean)
	rootAbs := filepath.Clean(s.Root)
	if full != rootAbs && !strings.HasPrefix(full, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes source root", relPath)
	}
	return full, nil
}

type FileSink struct {
	Root string
}

func (s FileSink) Describe() string { return "file://" + s.Root }

func (s FileSink) Write(_ context.Context, destPath string, src io.Reader, expectedBytes int64, expectedSHA256 string) (WriteResult, error) {
	full, err := s.safePath(destPath)
	if err != nil {
		return WriteResult{}, err
	}

	if existing, ok, err := s.verifyExisting(full, expectedBytes, expectedSHA256); err != nil {
		return WriteResult{}, err
	} else if ok {
		return existing, nil
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return WriteResult{}, fmt.Errorf("mkdir for %s: %w", destPath, err)
	}

	tmp, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".ingest-*")
	if err != nil {
		return WriteResult{}, fmt.Errorf("create temp for %s: %w", destPath, err)
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = tmp.Close()
			_ = os.Remove(tmpPath)
		}
	}()

	h := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(src, h))
	if err != nil {
		return WriteResult{}, fmt.Errorf("write temp %s: %w", destPath, err)
	}
	gotSHA256 := hex.EncodeToString(h.Sum(nil))

	if gotSHA256 != expectedSHA256 {
		return WriteResult{}, fmt.Errorf(
			"sha256 mismatch for %s: expected=%s got=%s",
			destPath, expectedSHA256, gotSHA256,
		)
	}
	if n != expectedBytes {
		return WriteResult{}, fmt.Errorf(
			"byte count mismatch for %s: expected=%d got=%d",
			destPath, expectedBytes, n,
		)
	}
	if err := tmp.Sync(); err != nil {
		return WriteResult{}, fmt.Errorf("sync temp %s: %w", destPath, err)
	}
	if err := tmp.Close(); err != nil {
		return WriteResult{}, fmt.Errorf("close temp %s: %w", destPath, err)
	}
	cleanup = false

	if err := os.Rename(tmpPath, full); err != nil {
		_ = os.Remove(tmpPath)
		if existing, ok, verr := s.verifyExisting(full, expectedBytes, expectedSHA256); verr != nil {
			return WriteResult{}, verr
		} else if ok {
			return existing, nil
		}
		return WriteResult{}, fmt.Errorf("rename to %s: %w", destPath, err)
	}

	return WriteResult{Bytes: n, SHA256: gotSHA256}, nil
}

func (s FileSink) verifyExisting(full string, expectedBytes int64, expectedSHA256 string) (WriteResult, bool, error) {
	f, err := os.Open(full)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return WriteResult{}, false, nil
		}
		return WriteResult{}, false, fmt.Errorf("open existing: %w", err)
	}
	defer f.Close()

	fi, err := f.Stat()
	if err != nil {
		return WriteResult{}, false, err
	}
	if fi.Size() != expectedBytes {
		return WriteResult{}, false, fmt.Errorf(
			"destination exists with size=%d but expected=%d — refusing overwrite of mismatched content",
			fi.Size(), expectedBytes,
		)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return WriteResult{}, false, err
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expectedSHA256 {
		return WriteResult{}, false, fmt.Errorf(
			"destination exists with sha256=%s but expected=%s — refusing overwrite of mismatched content",
			got, expectedSHA256,
		)
	}
	return WriteResult{Skipped: true, SHA256: got}, true, nil
}

func (s FileSink) safePath(destPath string) (string, error) {
	clean := filepath.FromSlash(destPath)
	full := filepath.Join(s.Root, clean)
	rootAbs := filepath.Clean(s.Root)
	if full != rootAbs && !strings.HasPrefix(full, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("destination path %q escapes sink root", destPath)
	}
	return full, nil
}

type FileLocker struct {
	Dir string
}

const lockTimeout = 5 * time.Minute

func (l FileLocker) Lock(ctx context.Context, name, version string) (context.Context, func() error, error) {
	if err := os.MkdirAll(l.Dir, 0o755); err != nil {
		return nil, nil, fmt.Errorf("create lock dir %s: %w", l.Dir, err)
	}
	lockName := lockFilename(name, version)
	lockPath := filepath.Join(l.Dir, lockName)

	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, nil, fmt.Errorf("open lock file %s: %w", lockPath, err)
	}

	deadline := time.Now().Add(lockTimeout)
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, nil, fmt.Errorf("context cancelled waiting for lock on %s@%s: %w", name, version, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, nil, fmt.Errorf("timed out waiting for lock on %s@%s after %s", name, version, lockTimeout)
		}

		err := tryFileLock(f)
		if err == nil {
			break
		}
		if !isLockBusy(err) {
			_ = f.Close()
			return nil, nil, fmt.Errorf("acquire lock for %s@%s: %w", name, version, err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, nil, fmt.Errorf("context cancelled waiting for lock on %s@%s: %w", name, version, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}

	var once sync.Once
	var unlockErr error
	unlock := func() error {
		once.Do(func() {
			if err := releaseFileLock(f); err != nil {
				_ = f.Close()
				unlockErr = fmt.Errorf("release lock: %w", err)
				return
			}
			unlockErr = f.Close()
		})
		return unlockErr
	}
	return ctx, unlock, nil
}

func lockFilename(name, version string) string {
	return name + "@" + version + ".lock"
}
