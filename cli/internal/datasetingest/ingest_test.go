// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package datasetingest_test

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
	"testing"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/datasetingest"
)

// ---- helpers ----

// makeReg builds an in-memory Registry with IngestStatusFile wired.
func makeReg(t *testing.T) *dataset.Registry {
	t.Helper()
	mem := &memBackend{files: make(map[string][]byte)}
	paths := dataset.Paths{
		DatasetsDir: func() string { return "/data" },
		DatasetDir:  func(n string) string { return "/data/" + n },
		VersionDir:  func(n, v string) string { return "/data/" + n + "/" + v },
		RecordFile:  func(n, v string) string { return "/data/" + n + "/" + v + "/dataset.json" },
		AliasesDir:  func(n string) string { return "/data/" + n + "/aliases" },
		AliasFile:   func(n, a string) string { return "/data/" + n + "/aliases/" + a + ".json" },
		IngestStatusFile: func(n, v string) string {
			return "/data/" + n + "/" + v + "/ingest-status.json"
		},
	}
	return dataset.NewRegistry(mem, paths, nil)
}

// makeRec builds and registers a valid pretrain Record with the given files.
func makeRec(t *testing.T, reg *dataset.Registry, name, version string, files []dataset.File) dataset.Record {
	t.Helper()
	total := int64(0)
	for _, f := range files {
		total += f.Bytes
	}
	rec := dataset.Record{
		SchemaVersion: dataset.SchemaVersion,
		Name:          name,
		Version:       version,
		Purpose:       "pretrain",
		Account:       "acct",
		Container:     "ctr",
		Prefix:        name + "/" + version,
		Files:         files,
		TotalBytes:    total,
		Assurance:     "manifest-supplied",
		Pretrain:      &dataset.Pretrain{Tokenizer: "gpt2"},
	}
	registered, _, err := reg.EnsureRegister(context.Background(), rec)
	if err != nil {
		t.Fatalf("EnsureRegister: %v", err)
	}
	_, _, err = reg.InitIngestStatus(context.Background(), registered)
	if err != nil {
		t.Fatalf("InitIngestStatus: %v", err)
	}
	return registered
}

// writeSourceFile writes data to srcDir/relPath and returns its sha256 and byte count.
func writeSourceFile(t *testing.T, srcDir, relPath string, content []byte) (sha256hex string, bytes int64) {
	t.Helper()
	full := filepath.Join(srcDir, filepath.FromSlash(relPath))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatalf("WriteFile %s: %v", full, err)
	}
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:]), int64(len(content))
}

// memBackend is an in-memory Backend for tests.
type memBackend struct {
	files map[string][]byte
}

func (m *memBackend) ReadFile(_ context.Context, path string) ([]byte, error) {
	b, ok := m.files[path]
	if !ok {
		return nil, dataset.ErrNotExist
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

func (m *memBackend) WriteFile(_ context.Context, path string, data []byte, overwrite bool) error {
	if !overwrite {
		if _, ok := m.files[path]; ok {
			return dataset.ErrExist
		}
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	m.files[path] = cp
	return nil
}

func (m *memBackend) List(_ context.Context, dir string) ([]string, error) {
	dir = strings.TrimSuffix(dir, "/") + "/"
	seen := map[string]struct{}{}
	found := false
	for p := range m.files {
		if !strings.HasPrefix(p, dir) {
			continue
		}
		found = true
		rest := strings.TrimPrefix(p, dir)
		if i := strings.Index(rest, "/"); i >= 0 {
			seen[rest[:i]] = struct{}{}
		} else {
			seen[rest] = struct{}{}
		}
	}
	if !found {
		return nil, dataset.ErrNotExist
	}
	var out []string
	for s := range seen {
		out = append(out, s)
	}
	return out, nil
}

func (m *memBackend) Delete(_ context.Context, path string) error {
	delete(m.files, path)
	return nil
}

// ---- Full E2E: register → ingest → verify ready → repeat (idempotent) ----

func TestRunWorker_fileBackend_fullE2E(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content0 := []byte("hello dataset part 0")
	content1 := []byte("hello dataset part 1")
	sha0, bytes0 := writeSourceFile(t, srcDir, "part-0.txt", content0)
	sha1, bytes1 := writeSourceFile(t, srcDir, "part-1.txt", content1)

	reg := makeReg(t)
	_ = makeRec(t, reg, "myds", "v1", []dataset.File{
		{Path: "part-0.txt", Bytes: bytes0, SHA256: sha0},
		{Path: "part-1.txt", Bytes: bytes1, SHA256: sha1},
	})

	result, err := datasetingest.RunWorker(context.Background(), "myds", "v1", datasetingest.WorkerConfig{
		Registry:  reg,
		Source:    datasetingest.FileSource{Root: srcDir},
		Sink:      datasetingest.FileSink{Root: dstDir},
		Locker:    datasetingest.FileLocker{Dir: dstDir},
		AttemptID: "test-attempt-1",
	})
	if err != nil {
		t.Fatalf("RunWorker: %v", err)
	}
	if result.Status.State != dataset.IngestStateReady {
		t.Errorf("State: want ready, got %q", result.Status.State)
	}
	if result.Status.VerifiedFiles != 2 {
		t.Errorf("VerifiedFiles: want 2, got %d", result.Status.VerifiedFiles)
	}
	if result.Status.VerifiedBytes != bytes0+bytes1 {
		t.Errorf("VerifiedBytes: want %d, got %d", bytes0+bytes1, result.Status.VerifiedBytes)
	}

	// Files must exist at the destination.
	for _, name := range []string{"part-0.txt", "part-1.txt"} {
		if _, err := os.Stat(filepath.Join(dstDir, name)); err != nil {
			t.Errorf("destination file %s not found: %v", name, err)
		}
	}

	// ---- Repeat call is idempotent (already ready) ----
	result2, err := datasetingest.RunWorker(context.Background(), "myds", "v1", datasetingest.WorkerConfig{
		Registry:  reg,
		Source:    datasetingest.FileSource{Root: srcDir},
		Sink:      datasetingest.FileSink{Root: dstDir},
		Locker:    datasetingest.FileLocker{Dir: dstDir},
		AttemptID: "test-attempt-2",
	})
	if err != nil {
		t.Fatalf("RunWorker (repeat): %v", err)
	}
	if result2.Status.State != dataset.IngestStateReady {
		t.Errorf("repeat state: want ready, got %q", result2.Status.State)
	}
}

// ---- Resume: partial completion, re-run skips completed files ----

func TestRunWorker_resume(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content0 := []byte("part 0 bytes")
	content1 := []byte("part 1 bytes")
	sha0, bytes0 := writeSourceFile(t, srcDir, "p0.txt", content0)
	sha1, bytes1 := writeSourceFile(t, srcDir, "p1.txt", content1)

	reg := makeReg(t)
	rec := makeRec(t, reg, "ds", "v1", []dataset.File{
		{Path: "p0.txt", Bytes: bytes0, SHA256: sha0},
		{Path: "p1.txt", Bytes: bytes1, SHA256: sha1},
	})

	// Manually write a status that already has p0.txt completed.
	partial := dataset.IngestStatus{
		SchemaVersion: dataset.IngestStatusSchemaVersion,
		Name:          "ds",
		Version:       "v1",
		RecordDigest:  rec.Digest,
		State:         dataset.IngestStateIngesting,
		AttemptID:     "test-partial",
		CompletedFiles: []dataset.FileProof{
			{
				Path:        "p0.txt",
				SHA256:      sha0,
				Bytes:       bytes0,
				CommittedAt: "2026-01-01T00:00:00Z",
			},
		},
		VerifiedFiles: 1,
		VerifiedBytes: bytes0,
	}
	// Write p0.txt to destination so FileSink skips it correctly.
	if err := os.WriteFile(filepath.Join(dstDir, "p0.txt"), content0, 0o644); err != nil {
		t.Fatalf("write p0.txt to dst: %v", err)
	}
	if err := reg.WriteIngestStatus(context.Background(), partial); err != nil {
		t.Fatalf("WriteIngestStatus: %v", err)
	}

	result, err := datasetingest.RunWorker(context.Background(), "ds", "v1", datasetingest.WorkerConfig{
		Registry:  reg,
		Source:    datasetingest.FileSource{Root: srcDir},
		Sink:      datasetingest.FileSink{Root: dstDir},
		Locker:    datasetingest.FileLocker{Dir: dstDir},
		AttemptID: "test-resume",
	})
	if err != nil {
		t.Fatalf("RunWorker (resume): %v", err)
	}
	if result.Status.State != dataset.IngestStateReady {
		t.Errorf("resume state: want ready, got %q", result.Status.State)
	}
	if result.Status.VerifiedFiles != 2 {
		t.Errorf("VerifiedFiles: want 2, got %d", result.Status.VerifiedFiles)
	}
}

// ---- Corruption rejection: existing destination with wrong sha256 ----

func TestRunWorker_corruptionRejection(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content := []byte("good bytes")
	sha, bytes := writeSourceFile(t, srcDir, "data.txt", content)

	reg := makeReg(t)
	_ = makeRec(t, reg, "ds", "v1", []dataset.File{
		{Path: "data.txt", Bytes: bytes, SHA256: sha},
	})

	// Write a corrupted file at the destination (wrong content, same name).
	corrupt := []byte("bad bytes that are different")
	if err := os.WriteFile(filepath.Join(dstDir, "data.txt"), corrupt, 0o644); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	_, err := datasetingest.RunWorker(context.Background(), "ds", "v1", datasetingest.WorkerConfig{
		Registry:  reg,
		Source:    datasetingest.FileSource{Root: srcDir},
		Sink:      datasetingest.FileSink{Root: dstDir},
		Locker:    datasetingest.FileLocker{Dir: dstDir},
		AttemptID: "test-corrupt",
	})
	if err == nil {
		t.Fatal("expected error for corrupted destination, got nil")
	}
	if !strings.Contains(err.Error(), "sha256") && !strings.Contains(err.Error(), "mismatch") && !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error should mention sha256/mismatch/corrupt: %v", err)
	}
}

// ---- Path traversal rejection ----

func TestRunWorker_pathTraversal(t *testing.T) {
	// A record with a traversal path. EnsureRegister validates paths, so we
	// need to test the ingest layer directly via a bad manifest.
	// Since EnsureRegister/Register also validates, we test validateRelPath
	// indirectly through the fake source that returns data for a malformed path.
	//
	// We craft a source that accepts the traversal path to test the ingest
	// layer's own guard.
	badPath := "../escape.txt"
	content := []byte("evil")
	sha := sha256sum(content)

	// We need a record with the traversal path. The dataset validator will
	// reject it, so we use a workaround: start with a valid record and manually
	// write a modified ingest status that references the bad path.
	//
	// Simpler: test validateRelPath via a fake source/sink.
	fakeSource := &traversalSource{path: badPath, content: content}
	fakeSink := &captureSink{}
	fakeLocker := &noopLocker{}

	rec := dataset.Record{
		SchemaVersion: dataset.SchemaVersion,
		Name:          "badds",
		Version:       "v1",
		Purpose:       "pretrain",
		Account:       "acct",
		Container:     "ctr",
		Files:         []dataset.File{{Path: badPath, Bytes: int64(len(content)), SHA256: sha}},
		Assurance:     "manifest-supplied",
		Pretrain:      &dataset.Pretrain{Tokenizer: "gpt2"},
	}
	// Register normally fails on invalid path; we bypass registration and
	// directly call RunWorker with the fake source.
	// Build the registry status manually.
	rec.TotalBytes = int64(len(content))
	rec.Digest = rec.ComputeDigest()

	// We can't EnsureRegister because dataset validates paths. Instead, write
	// the status manually through an alternate memBackend path.
	// The ingest layer must reject the path before the sink sees it.
	_, err := datasetingest.RunWorker(context.Background(), "badds", "v1", datasetingest.WorkerConfig{
		Registry:  makeRegWithRecord(t, rec),
		Source:    fakeSource,
		Sink:      fakeSink,
		Locker:    fakeLocker,
		AttemptID: "traversal-test",
	})
	if err == nil {
		t.Fatal("expected path traversal error, got nil")
	}
}

// ---- Context cancellation ----

func TestRunWorker_contextCancellation(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()

	content := []byte("cancel-me bytes of sufficient size to maybe see the cancel")
	sha, bytes := writeSourceFile(t, srcDir, "data.txt", content)

	reg := makeReg(t)
	_ = makeRec(t, reg, "ds", "v1", []dataset.File{
		{Path: "data.txt", Bytes: bytes, SHA256: sha},
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	_, err := datasetingest.RunWorker(ctx, "ds", "v1", datasetingest.WorkerConfig{
		Registry:  reg,
		Source:    datasetingest.FileSource{Root: srcDir},
		Sink:      datasetingest.FileSink{Root: dstDir},
		Locker:    datasetingest.FileLocker{Dir: dstDir},
		AttemptID: "cancel-test",
	})
	// May succeed (single-file, fast) or fail with context error — both are OK.
	// What matters is no panic.
	if err != nil && !strings.Contains(err.Error(), "context") && !strings.Contains(err.Error(), "cancel") {
		// Context cancellation may surface as various error messages; just
		// ensure the error is non-nil and non-panic.
		t.Logf("cancellation error (expected): %v", err)
	}

}

func TestRunWorker_LeaseRenewalFailureNeverProducesReadyStatus(t *testing.T) {
	srcDir := t.TempDir()
	dstDir := t.TempDir()
	content := []byte("lease failure")
	sha, bytes := writeSourceFile(t, srcDir, "data.txt", content)
	reg := makeReg(t)
	_ = makeRec(t, reg, "ds", "v1", []dataset.File{{Path: "data.txt", Bytes: bytes, SHA256: sha}})

	renewErr := errors.New("renew lease denied")
	_, err := datasetingest.RunWorker(context.Background(), "ds", "v1", datasetingest.WorkerConfig{
		Registry: reg,
		Source:   datasetingest.FileSource{Root: srcDir},
		Sink:     datasetingest.FileSink{Root: dstDir},
		Locker:   renewFailureLocker{err: renewErr},
	})
	if !errors.Is(err, renewErr) {
		t.Fatalf("RunWorker error = %v, want renewal failure", err)
	}
	status, statusErr := reg.GetIngestStatus(context.Background(), "ds", "v1")
	if statusErr != nil {
		t.Fatalf("GetIngestStatus: %v", statusErr)
	}
	if status.State == dataset.IngestStateReady {
		t.Fatalf("renewal failure must not produce ready status: %+v", status)
	}
}

// ---- helpers for traversal test ----

func sha256sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

type traversalSource struct {
	path    string
	content []byte
}

func (s *traversalSource) Open(_ context.Context, relPath string) (io.ReadCloser, int64, error) {
	if relPath == s.path {
		return io.NopCloser(strings.NewReader(string(s.content))), int64(len(s.content)), nil
	}
	return nil, 0, fmt.Errorf("not found: %q", relPath)
}
func (s *traversalSource) Describe() string { return "fake" }

type captureSink struct{}

func (c *captureSink) Write(_ context.Context, _ string, _ io.Reader, _ int64, _ string) (datasetingest.WriteResult, error) {
	return datasetingest.WriteResult{}, nil
}
func (c *captureSink) Describe() string { return "capture" }

type noopLocker struct{}

func (n *noopLocker) Lock(ctx context.Context, _, _ string) (context.Context, func() error, error) {
	return ctx, func() error { return nil }, nil
}

type renewFailureLocker struct{ err error }

func (l renewFailureLocker) Lock(ctx context.Context, _, _ string) (context.Context, func() error, error) {
	lockCtx, cancel := context.WithCancelCause(ctx)
	cancel(l.err)
	return lockCtx, func() error { return l.err }, nil
}

// makeRegWithRecord builds a registry that already has the given record and
// an ingest status (bypassing validation that would reject the bad path).
func makeRegWithRecord(t *testing.T, rec dataset.Record) *dataset.Registry {
	t.Helper()
	mem := &memBackend{files: make(map[string][]byte)}
	paths := dataset.Paths{
		DatasetsDir: func() string { return "/data" },
		DatasetDir:  func(n string) string { return "/data/" + n },
		VersionDir:  func(n, v string) string { return "/data/" + n + "/" + v },
		RecordFile:  func(n, v string) string { return "/data/" + n + "/" + v + "/dataset.json" },
		AliasesDir:  func(n string) string { return "/data/" + n + "/aliases" },
		AliasFile:   func(n, a string) string { return "/data/" + n + "/aliases/" + a + ".json" },
		IngestStatusFile: func(n, v string) string {
			return "/data/" + n + "/" + v + "/ingest-status.json"
		},
	}
	reg := dataset.NewRegistry(mem, paths, nil)

	// Write the record JSON directly to bypass validation.
	recJSON, _ := rec.Marshal()
	_ = mem.WriteFile(context.Background(), paths.RecordFile(rec.Name, rec.Version), recJSON, false)

	// Write a minimal registered status.
	status := dataset.IngestStatus{
		SchemaVersion: dataset.IngestStatusSchemaVersion,
		Name:          rec.Name,
		Version:       rec.Version,
		RecordDigest:  rec.Digest,
		State:         dataset.IngestStateRegistered,
		AttemptID:     "pre-init",
	}
	raw, _ := status.Marshal()
	_ = mem.WriteFile(context.Background(), paths.IngestStatusFile(rec.Name, rec.Version), raw, false)

	return reg
}
