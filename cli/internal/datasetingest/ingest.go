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
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/dataset"
)

type ByteSource interface {
	Open(ctx context.Context, relPath string) (io.ReadCloser, int64, error)
	Describe() string
}

type WriteResult struct {
	Skipped bool
	Bytes   int64
	SHA256  string
}

type StagedSink interface {
	Write(ctx context.Context, destPath string, src io.Reader, expectedBytes int64, expectedSHA256 string) (WriteResult, error)
	Describe() string
}

type VersionLocker interface {
	Lock(ctx context.Context, name, version string) (lockCtx context.Context, unlock func() error, err error)
}

type WorkerConfig struct {
	Registry  *dataset.Registry
	Source    ByteSource
	Sink      StagedSink
	Locker    VersionLocker
	AttemptID string
	Now       func() time.Time
}

type WorkerResult struct {
	Status dataset.IngestStatus
}

func RunWorker(ctx context.Context, name, version string, cfg WorkerConfig) (result WorkerResult, retErr error) {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AttemptID == "" {
		cfg.AttemptID = fmt.Sprintf("attempt-%d", cfg.Now().UnixNano())
	}

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

	rec, err := cfg.Registry.Get(lockCtx, name, version)
	if err != nil {
		return WorkerResult{}, fmt.Errorf("load record: %w", err)
	}

	status, err := cfg.Registry.GetIngestStatus(lockCtx, name, version)
	if err != nil {
		if !dataset.IsNotExist(err) {
			return WorkerResult{}, fmt.Errorf("read ingest status: %w", err)
		}
		status, _, err = cfg.Registry.InitIngestStatus(lockCtx, rec)
		if err != nil {
			return WorkerResult{}, fmt.Errorf("init ingest status: %w", err)
		}
	}

	if status.RecordDigest != rec.Digest {
		return WorkerResult{}, fmt.Errorf(
			"ingest-status digest %s does not match record digest %s — "+
				"status is bound to a different record version",
			status.RecordDigest, rec.Digest,
		)
	}

	if status.State == dataset.IngestStateReady {
		return WorkerResult{Status: status}, nil
	}

	status.State = dataset.IngestStateIngesting
	status.AttemptID = cfg.AttemptID
	status.FailureSummary = ""
	if status.StartedAt == "" {
		status.StartedAt = cfg.Now().UTC().Format(time.RFC3339)
	}
	if err := cfg.Registry.WriteIngestStatus(lockCtx, status); err != nil {
		return WorkerResult{}, fmt.Errorf("write ingesting status: %w", err)
	}

	result, ingestErr := ingestFiles(lockCtx, rec, &status, cfg)

	if ingestErr != nil {
		summary := truncate(ingestErr.Error(), 512)
		status.State = dataset.IngestStateFailed
		status.FailureSummary = summary
		_ = cfg.Registry.WriteIngestStatus(lockCtx, status)
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

func ingestFiles(ctx context.Context, rec dataset.Record, status *dataset.IngestStatus, cfg WorkerConfig) (WorkerResult, error) {
	done := make(map[string]dataset.FileProof, len(status.CompletedFiles))
	for _, fp := range status.CompletedFiles {
		done[fp.Path] = fp
	}

	for _, f := range sortedFiles(rec.Files) {
		if err := ctx.Err(); err != nil {
			return WorkerResult{}, fmt.Errorf("context cancelled before %s: %w", f.Path, err)
		}

		if proof, ok := done[f.Path]; ok {
			if proof.SHA256 == f.SHA256 && proof.Bytes == f.Bytes {
				continue
			}
			return WorkerResult{}, fmt.Errorf(
				"completed_files proof for %s has sha256=%s bytes=%d but record has sha256=%s bytes=%d — corrupt status",
				f.Path, proof.SHA256, proof.Bytes, f.SHA256, f.Bytes,
			)
		}

		if err := validateRelPath(f.Path); err != nil {
			return WorkerResult{}, fmt.Errorf("unsafe path in record: %w", err)
		}

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
			proof := dataset.FileProof{
				Path:        f.Path,
				SHA256:      f.SHA256,
				Bytes:       f.Bytes,
				CommittedAt: cfg.Now().UTC().Format(time.RFC3339),
			}
			status.CompletedFiles = append(status.CompletedFiles, proof)
		} else {
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

func validateRelPath(p string) error {
	if p == "" {
		return fmt.Errorf("empty path")
	}
	if filepath.IsAbs(filepath.FromSlash(p)) {
		return fmt.Errorf("path %q must be relative (no leading slash)", p)
	}
	if strings.Contains(p, "..") {
		for _, seg := range strings.Split(p, "/") {
			if seg == ".." {
				return fmt.Errorf("path %q contains '..' traversal component", p)
			}
		}
	}
	return nil
}

func sortedFiles(files []dataset.File) []dataset.File {
	out := make([]dataset.File, len(files))
	copy(out, files)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func truncate(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes-3] + "..."
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
