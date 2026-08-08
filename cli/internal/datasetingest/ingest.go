// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package datasetingest implements the byte-transfer core for tau dataset
// ingest: copying manifest-described files from a source to a destination with
// sha256 verification, per-file checkpointing, resume, and a version lock to
// prevent concurrent conflicting writes.
//
// The public surface consists of three interfaces (ByteSource, StagedSink,
// VersionLocker) and the RunWorker function that orchestrates them. Two
// implementations are provided:
//
//   - File-backend (FileSource, FileSink, FileLocker): operates entirely on
//     the local filesystem; fully testable without any Azure or cluster
//     dependencies.
//   - Azure-backend (AzureSource, AzureSink, AzureLocker): uses
//     DefaultAzureCredential for workload identity; intended for in-cluster
//     workers.
package datasetingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/dataset"
)

// ByteSource opens individual dataset files for reading. Implementations must
// be safe to call concurrently; RunWorker calls Open sequentially but callers
// may wish to call it from multiple goroutines.
type ByteSource interface {
	// Open returns a reader for the file at relPath (forward-slash, no leading
	// slash, as recorded in dataset.File.Path). The caller closes the reader.
	// The returned size is the declared size from the manifest; the actual
	// byte count must be validated by the reader.
	Open(ctx context.Context, relPath string) (io.ReadCloser, int64, error)
	// Describe returns a human-readable description of this source (e.g.,
	// "file:///data/fineweb" or "az://account/container/prefix").
	Describe() string
}

// WriteResult is the outcome of a single StagedSink.Write call.
type WriteResult struct {
	// Skipped is true when the destination already has a blob/file with the
	// correct sha256 and byte count. No bytes were transferred.
	Skipped bool
	// Bytes is the number of bytes written (0 when Skipped is true).
	Bytes int64
	// SHA256 is the hex sha256 of the content verified at the destination.
	SHA256 string
}

// StagedSink writes dataset files to a destination with staging semantics: a
// file becomes visible at its final path only after size and sha256 are
// verified. Overwriting a final file with different content is an error (it
// would silently corrupt the dataset).
type StagedSink interface {
	// Write copies the content from src to destPath. expectedBytes and
	// expectedSHA256 are the values from the immutable manifest; the
	// implementation must verify that the copied bytes match both. On
	// mismatch the write is rolled back and an error is returned.
	// If the destination already exists with matching content, Skipped=true
	// is returned. If it exists with different content, an error is returned.
	Write(ctx context.Context, destPath string, src io.Reader, expectedBytes int64, expectedSHA256 string) (WriteResult, error)
	// Describe returns a human-readable description of this sink.
	Describe() string
}

// VersionLocker provides exclusive per-version locking. Implementations must
// ensure that at most one lock-holder per (name, version) pair is active at
// any time. The returned context is canceled if the lock can no longer be
// safely held (for example, Azure lease renewal fails).
type VersionLocker interface {
	// Lock acquires the exclusive lock for (name, version). It blocks until
	// the lock is available or ctx is cancelled. The returned unlock function
	// releases the lock; calling it more than once is safe.
	Lock(ctx context.Context, name, version string) (lockCtx context.Context, unlock func() error, err error)
}

// WorkerConfig is the complete configuration for a single RunWorker call.
type WorkerConfig struct {
	// Registry must have IngestStatusFile wired in its Paths.
	Registry *dataset.Registry
	// Source provides readable streams for each manifest file.
	Source ByteSource
	// Sink receives the staged writes.
	Sink StagedSink
	// Locker provides per-version exclusive access.
	Locker VersionLocker
	// AttemptID identifies this attempt. If empty, a time-based ID is used.
	AttemptID string
	// Now, if non-nil, is used for timestamps. Defaults to time.Now.
	Now func() time.Time
}

// WorkerResult is returned by RunWorker on success.
type WorkerResult struct {
	// Status is the final IngestStatus (state == ready).
	Status dataset.IngestStatus
}

// RunWorker orchestrates one ingest attempt for name@version.
//
// Preconditions: the record must already be registered and the ingest status
// must have been initialised (via Registry.InitIngestStatus or equivalent).
//
// Flow:
//  1. Acquire the version lock and use its context for all registry and byte
//     operations.
//  2. Load the record and existing ingest status under lock.
//  3. Transition to ingesting.
//  5. For each file in the record (in path order), skip if already in
//     CompletedFiles; otherwise stream from Source to Sink with full sha256
//     verification; checkpoint after each successful file.
//  6. After all files, set state = ready.
//  7. Release lock, return result.
func RunWorker(ctx context.Context, name, version string, cfg WorkerConfig) (result WorkerResult, retErr error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AttemptID == "" {
		cfg.AttemptID = fmt.Sprintf("attempt-%d", cfg.Now().UnixNano())
	}

	// 1. Acquire the lock before reading any mutable or immutable ingest state.
	lockCtx, unlock, err := cfg.Locker.Lock(ctx, name, version)
	if err != nil {
		return WorkerResult{}, fmt.Errorf("acquire version lock for %s@%s: %w", name, version, err)
	}
	defer func() {
		if err := unlock(); err != nil {
			err = fmt.Errorf("release version lock for %s@%s: %w", name, version, err)
			if retErr == nil {
				result = WorkerResult{}
				retErr = err
			} else {
				retErr = errors.Join(retErr, err)
			}
		}
	}()

	// 2. Load record and status using the lock-scoped context. This prevents a
	// lease renewal failure from allowing a ready status to be written.
	rec, err := cfg.Registry.Get(lockCtx, name, version)
	if err != nil {
		return WorkerResult{}, fmt.Errorf("load record: %w", err)
	}

	status, err := cfg.Registry.GetIngestStatus(lockCtx, name, version)
	if err != nil {
		if !dataset.IsNotExist(err) {
			return WorkerResult{}, fmt.Errorf("read ingest status: %w", err)
		}
		// No status exists yet; initialise it.
		status, _, err = cfg.Registry.InitIngestStatus(lockCtx, rec)
		if err != nil {
			return WorkerResult{}, fmt.Errorf("init ingest status: %w", err)
		}
	}

	// Bind check: status must be for this record digest.
	if status.RecordDigest != rec.Digest {
		return WorkerResult{}, fmt.Errorf(
			"ingest-status digest %s does not match record digest %s — "+
				"status is bound to a different record version",
			status.RecordDigest, rec.Digest,
		)
	}

	// Fast path: already ready.
	if status.State == dataset.IngestStateReady {
		return WorkerResult{Status: status}, nil
	}

	// 3. Transition to ingesting.
	status.State = dataset.IngestStateIngesting
	status.AttemptID = cfg.AttemptID
	status.FailureSummary = ""
	if status.StartedAt == "" {
		status.StartedAt = cfg.Now().UTC().Format(time.RFC3339)
	}
	if err := cfg.Registry.WriteIngestStatus(lockCtx, status); err != nil {
		return WorkerResult{}, fmt.Errorf("write ingesting status: %w", err)
	}

	// 5. Ingest each file.
	result, ingestErr := ingestFiles(lockCtx, rec, &status, cfg)

	// 5. On success set ready; on failure record the failure. A canceled scoped
	// context is a lease-health failure (or caller cancellation), never a ready
	// result.
	if ingestErr != nil {
		summary := truncate(ingestErr.Error(), 512)
		status.State = dataset.IngestStateFailed
		status.FailureSummary = summary
		_ = cfg.Registry.WriteIngestStatus(lockCtx, status) // best-effort
		return WorkerResult{}, ingestErr
	}
	if err := lockContextError(lockCtx); err != nil {
		return WorkerResult{}, err
	}
	status.State = dataset.IngestStateReady
	status.FailureSummary = ""
	if err := cfg.Registry.WriteIngestStatus(lockCtx, status); err != nil {
		return WorkerResult{}, fmt.Errorf("write ready status: %w", err)
	}
	result.Status = status
	return result, nil
}

func lockContextError(ctx context.Context) error {
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return ctx.Err()
}

// ingestFiles copies all files in rec that are not already proven in status.
func ingestFiles(ctx context.Context, rec dataset.Record, status *dataset.IngestStatus, cfg WorkerConfig) (WorkerResult, error) {
	// Build a set of already-completed files keyed by path.
	done := make(map[string]dataset.FileProof, len(status.CompletedFiles))
	for _, fp := range status.CompletedFiles {
		done[fp.Path] = fp
	}

	for _, f := range sortedFiles(rec.Files) {
		if err := ctx.Err(); err != nil {
			return WorkerResult{}, fmt.Errorf("context cancelled before %s: %w", f.Path, err)
		}

		// Skip if already proven with matching content.
		if proof, ok := done[f.Path]; ok {
			if proof.SHA256 == f.SHA256 && proof.Bytes == f.Bytes {
				continue
			}
			// Proof exists but sha256 or bytes differ — this should not
			// happen with a correct implementation, but fail closed.
			return WorkerResult{}, fmt.Errorf(
				"completed_files proof for %s has sha256=%s bytes=%d but record has sha256=%s bytes=%d — corrupt status",
				f.Path, proof.SHA256, proof.Bytes, f.SHA256, f.Bytes,
			)
		}

		// Validate path safety.
		if err := validateRelPath(f.Path); err != nil {
			return WorkerResult{}, fmt.Errorf("unsafe path in record: %w", err)
		}

		// Open source.
		rc, sourceBytes, err := cfg.Source.Open(ctx, f.Path)
		if err != nil {
			return WorkerResult{}, fmt.Errorf("open source %s: %w", f.Path, err)
		}
		if sourceBytes >= 0 && sourceBytes != f.Bytes {
			_ = rc.Close()
			return WorkerResult{}, fmt.Errorf(
				"source byte count mismatch for %s: manifest=%d source=%d",
				f.Path, f.Bytes, sourceBytes,
			)
		}

		// Stream through sha256 counter to the sink.
		h := sha256.New()
		counted := &countReader{r: io.TeeReader(rc, h)}
		res, writeErr := cfg.Sink.Write(ctx, f.Path, counted, f.Bytes, f.SHA256)
		_ = rc.Close()
		if writeErr != nil {
			return WorkerResult{}, fmt.Errorf("write %s: %w", f.Path, writeErr)
		}
		if err := lockContextError(ctx); err != nil {
			return WorkerResult{}, fmt.Errorf("lock health after %s: %w", f.Path, err)
		}

		if res.Skipped {
			// Sink verified the existing content matches.
			proof := dataset.FileProof{
				Path:        f.Path,
				SHA256:      f.SHA256,
				Bytes:       f.Bytes,
				CommittedAt: cfg.Now().UTC().Format(time.RFC3339),
			}
			status.CompletedFiles = append(status.CompletedFiles, proof)
		} else {
			// Sink reports what it actually wrote; cross-check with manifest.
			gotSHA256 := hex.EncodeToString(h.Sum(nil))
			if gotSHA256 != f.SHA256 {
				return WorkerResult{}, fmt.Errorf(
					"sha256 mismatch for %s: manifest=%s written=%s",
					f.Path, f.SHA256, gotSHA256,
				)
			}
			if counted.n != f.Bytes {
				return WorkerResult{}, fmt.Errorf(
					"byte count mismatch for %s: manifest=%d written=%d",
					f.Path, f.Bytes, counted.n,
				)
			}
			proof := dataset.FileProof{
				Path:        f.Path,
				SHA256:      gotSHA256,
				Bytes:       res.Bytes,
				CommittedAt: cfg.Now().UTC().Format(time.RFC3339),
			}
			status.CompletedFiles = append(status.CompletedFiles, proof)
		}
		done[f.Path] = status.CompletedFiles[len(status.CompletedFiles)-1]

		// Recompute totals and checkpoint after each file.
		status.VerifiedFiles = len(status.CompletedFiles)
		status.VerifiedBytes = 0
		for _, fp := range status.CompletedFiles {
			status.VerifiedBytes += fp.Bytes
		}
		if err := cfg.Registry.WriteIngestStatus(ctx, *status); err != nil {
			return WorkerResult{}, fmt.Errorf("checkpoint after %s: %w", f.Path, err)
		}
	}
	return WorkerResult{}, nil
}

// validateRelPath rejects paths that escape the destination root via traversal.
func validateRelPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if filepath.IsAbs(filepath.FromSlash(p)) {
		return fmt.Errorf("path %q must be relative (no leading slash)", p)
	}
	if strings.Contains(p, "..") {
		// Check each component.
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return fmt.Errorf("path %q contains '..' traversal component", p)
			}
		}
	}
	return nil
}

// sortedFiles returns rec.Files in a deterministic order (by path). The Record
// files are already sorted when computed, but we sort defensively.
func sortedFiles(files []dataset.File) []dataset.File {
	out := make([]dataset.File, len(files))
	copy(out, files)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// truncate limits a string to maxBytes UTF-8 bytes, appending "..." if cut.
func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes-3] + "..."
}

// countReader wraps an io.Reader and counts bytes read.
type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
