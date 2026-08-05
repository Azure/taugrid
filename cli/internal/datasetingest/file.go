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

// FileSource implements ByteSource over a local directory. It joins the
// directory root with each manifest-relative path, verifying the joined
// result stays within the root (path-traversal-safe).
type FileSource struct {
	// Root is the absolute path of the source directory.
	Root string
}

// Open opens a file at relPath (relative to Root) for reading. The joined path
// must not escape Root.
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

// safePath joins Root with relPath and verifies the result stays under Root.
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

// FileSink implements StagedSink over a local directory. Each file is written
// to a sibling temporary path, sha256-verified, then renamed into place. If the
// final destination already exists with matching content, Skipped=true is
// returned. If it exists with different content, an error is returned.
type FileSink struct {
	// Root is the absolute path of the destination directory.
	Root string
}

func (s FileSink) Describe() string { return "file://" + s.Root }

// Write copies src to destPath under Root with staged commit semantics.
func (s FileSink) Write(_ context.Context, destPath string, src io.Reader, expectedBytes int64, expectedSHA256 string) (WriteResult, error) {
	// Validate destination path.
	full, err := s.safePath(destPath)
	if err != nil {
		return WriteResult{}, err
	}

	// Check existing destination.
	if existing, ok, err := s.verifyExisting(full, expectedBytes, expectedSHA256); err != nil {
		return WriteResult{}, err
	} else if ok {
		return existing, nil
	}

	// Ensure parent directory exists.
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return WriteResult{}, fmt.Errorf("mkdir for %s: %w", destPath, err)
	}

	// Write to a staged temp file in the same directory so rename is atomic.
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

	// Stream src through sha256 to temp file.
	h := sha256.New()
	n, err := io.Copy(tmp, io.TeeReader(src, h))
	if err != nil {
		return WriteResult{}, fmt.Errorf("write temp %s: %w", destPath, err)
	}
	gotSHA256 := hex.EncodeToString(h.Sum(nil))

	// Verify against manifest before committing.
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

	// Atomic rename into final position.
	if err := os.Rename(tmpPath, full); err != nil {
		_ = os.Remove(tmpPath)
		// If the rename failed because another writer won the race, check
		// the existing file.
		if existing, ok, verr := s.verifyExisting(full, expectedBytes, expectedSHA256); verr != nil {
			return WriteResult{}, verr
		} else if ok {
			return existing, nil
		}
		return WriteResult{}, fmt.Errorf("rename to %s: %w", destPath, err)
	}

	return WriteResult{Bytes: n, SHA256: gotSHA256}, nil
}

// verifyExisting checks whether full exists with matching content. Returns
// (result, true, nil) when the file exists and matches, (zero, false, nil) when
// absent, and (zero, false, err) when the file exists but differs.
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

// FileLocker implements VersionLocker using OS-level advisory file locks
// (flock on Linux/macOS, LockFileEx on Windows). The lock file is created at
// <dir>/<name>@<version>.lock. Stale locks from crashed processes are
// automatically released because the kernel closes fds on process exit.
type FileLocker struct {
	// Dir is the directory where lock files are created. It must exist before
	// calling Lock.
	Dir string
}

// lockTimeout is how long Lock will poll before giving up.
const lockTimeout = 5 * time.Minute

// Lock acquires an exclusive file lock for (name, version). It polls
// non-blockingly until the lock is acquired or ctx expires.
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
		// Another process holds the lock; sleep briefly and retry.
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

// lockFilename returns a filesystem-safe lock file name for (name, version).
// Both name and version are constrained by the dataset package's validation
// to [a-z0-9][a-z0-9._-]* so the resulting filename is always safe.
func lockFilename(name, version string) string {
	return name + "@" + version + ".lock"
}
