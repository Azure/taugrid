package dataset

// Tests for ingest_status.go and ingest_registry.go.
// This file is in package dataset (not dataset_test) so it can reuse the
// memBackend and testPaths helpers defined in registry_test.go.

import (
	"context"
	"testing"
)

// ingestTestPaths extends testPaths with IngestStatusFile.
func ingestTestPaths() Paths {
	p := testPaths()
	root := "/data/dataset-registry/datasets"
	p.IngestStatusFile = func(n, v string) string {
		return root + "/" + n + "/" + v + "/ingest-status.json"
	}
	return p
}

// makeIngestRec builds a minimal valid Record for ingest tests.
func makeIngestRec(name, version string) Record {
	return Record{
		SchemaVersion: SchemaVersion,
		Name:          name,
		Version:       version,
		Purpose:       PurposePretrain,
		Account:       "acct",
		Container:     "ctr",
		Prefix:        name + "/" + version,
		Files: []File{
			{Path: "part-0.jsonl", Bytes: 100, SHA256: shaA},
			{Path: "part-1.jsonl", Bytes: 200, SHA256: shaB},
		},
		Assurance: AssuranceManifestSupplied,
		Pretrain:  &Pretrain{Tokenizer: "gpt2"},
	}
}

// newIngestRegistry creates a Registry backed by memBackend with IngestStatusFile wired.
func newIngestRegistry() (*Registry, *memBackend) {
	b := newMemBackend()
	reg := NewRegistry(b, ingestTestPaths(), nil)
	return reg, b
}

// ---- ParseIngestStatus tests ----

func TestParseIngestStatus_valid(t *testing.T) {
	raw := []byte(`{
		"schema_version": 1,
		"name": "fineweb",
		"version": "v1",
		"record_digest": "sha256:abc123",
		"state": "ready",
		"completed_files": [
			{"path": "part-0.jsonl", "sha256": "` + shaA + `", "bytes": 4000, "committed_at": "2026-01-01T00:00:00Z"},
			{"path": "part-1.jsonl", "sha256": "` + shaB + `", "bytes": 5000, "committed_at": "2026-01-01T00:01:00Z"}
		],
		"verified_files": 2,
		"verified_bytes": 9000
	}`)
	s, err := ParseIngestStatus(raw)
	if err != nil {
		t.Fatalf("ParseIngestStatus: %v", err)
	}
	if s.Name != "fineweb" {
		t.Errorf("Name: want fineweb, got %q", s.Name)
	}
	if s.State != IngestStateReady {
		t.Errorf("State: want ready, got %q", s.State)
	}
}

func TestParseIngestStatus_invalidJSON(t *testing.T) {
	_, err := ParseIngestStatus([]byte(`not json`))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
}

func TestParseIngestStatus_wrongSchemaVersion(t *testing.T) {
	raw := []byte(`{"schema_version": 999, "name": "x", "version": "v1", "record_digest": "sha256:a", "state": "ready"}`)
	_, err := ParseIngestStatus(raw)
	if err == nil {
		t.Fatal("expected error for unknown schema_version")
	}
}

func TestParseIngestStatus_invalidState(t *testing.T) {
	raw := []byte(`{"schema_version": 1, "name": "x", "version": "v1", "record_digest": "sha256:a", "state": "unknown-state"}`)
	_, err := ParseIngestStatus(raw)
	if err == nil {
		t.Fatal("expected error for unknown state")
	}
}

func TestIngestStatus_Marshal_roundtrip(t *testing.T) {
	s := IngestStatus{
		SchemaVersion: IngestStatusSchemaVersion,
		Name:          "ds",
		Version:       "v1",
		RecordDigest:  "sha256:cafebabe",
		State:         IngestStateIngesting,
		CompletedFiles: []FileProof{
			{Path: "a.jsonl", SHA256: shaA, Bytes: 2048, CommittedAt: "2026-01-01T00:00:00Z"},
			{Path: "b.jsonl", SHA256: shaB, Bytes: 2048, CommittedAt: "2026-01-01T00:01:00Z"},
		},
		VerifiedFiles: 2,
		VerifiedBytes: 4096,
		AttemptID:     "attempt-1",
	}

	raw, err := s.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := ParseIngestStatus(raw)
	if err != nil {
		t.Fatalf("ParseIngestStatus after Marshal: %v", err)
	}
	if got.State != s.State {
		t.Errorf("State: want %q, got %q", s.State, got.State)
	}
	if got.VerifiedBytes != s.VerifiedBytes {
		t.Errorf("VerifiedBytes: want %d, got %d", s.VerifiedBytes, got.VerifiedBytes)
	}
}

func TestIngestStatus_AllowsBenignDoubleDotsInProofName(t *testing.T) {
	s := IngestStatus{
		SchemaVersion: IngestStatusSchemaVersion,
		Name:          "ds",
		Version:       "v1",
		RecordDigest:  "sha256:abc",
		State:         IngestStateReady,
		CompletedFiles: []FileProof{{
			Path: "shards/part..0001.jsonl", SHA256: shaA, Bytes: 1, CommittedAt: "2026-01-01T00:00:00Z",
		}},
		VerifiedFiles: 1,
		VerifiedBytes: 1,
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("benign '..' filename must be accepted: %v", err)
	}
}

func TestIngestStatus_RejectsTraversalAndAbsoluteProofPaths(t *testing.T) {
	for _, path := range []string{"shards/../secret.jsonl", "/absolute/part.jsonl"} {
		s := IngestStatus{
			SchemaVersion: IngestStatusSchemaVersion,
			Name:          "ds",
			Version:       "v1",
			RecordDigest:  "sha256:abc",
			State:         IngestStateReady,
			CompletedFiles: []FileProof{{
				Path: path, SHA256: shaA, Bytes: 1, CommittedAt: "2026-01-01T00:00:00Z",
			}},
			VerifiedFiles: 1,
			VerifiedBytes: 1,
		}
		if err := s.Validate(); err == nil {
			t.Errorf("proof path %q must be rejected", path)
		}
	}
}

// ---- EnsureRegister tests ----

func TestEnsureRegister_noOp(t *testing.T) {
	reg, _ := newIngestRegistry()
	rec := makeIngestRec("ds", "v1")

	// First call: registers.
	_, created, err := reg.EnsureRegister(context.Background(), rec)
	if err != nil {
		t.Fatalf("EnsureRegister (first): %v", err)
	}
	if !created {
		t.Error("EnsureRegister (first): want created=true")
	}

	// Second call: no-op.
	_, created2, err := reg.EnsureRegister(context.Background(), rec)
	if err != nil {
		t.Fatalf("EnsureRegister (second): %v", err)
	}
	if created2 {
		t.Error("EnsureRegister (second): want created=false")
	}
}

func TestEnsureRegister_driftDetected(t *testing.T) {
	reg, _ := newIngestRegistry()
	rec := makeIngestRec("ds", "v1")

	if _, _, err := reg.EnsureRegister(context.Background(), rec); err != nil {
		t.Fatalf("EnsureRegister (first): %v", err)
	}

	// Same name@version but different file content.
	rec2 := makeIngestRec("ds", "v1")
	rec2.Files = append(rec2.Files, File{Path: "extra.jsonl", Bytes: 50, SHA256: "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"})

	_, _, err := reg.EnsureRegister(context.Background(), rec2)
	if err == nil {
		t.Fatal("expected drift error, got nil")
	}
}

// ---- GetIngestStatus / WriteIngestStatus / InitIngestStatus tests ----

func TestInitIngestStatus_firstCall(t *testing.T) {
	reg, _ := newIngestRegistry()
	rec := makeIngestRec("ds", "v1")

	registered, _, err := reg.EnsureRegister(context.Background(), rec)
	if err != nil {
		t.Fatalf("EnsureRegister: %v", err)
	}

	status, created, err := reg.InitIngestStatus(context.Background(), registered)
	if err != nil {
		t.Fatalf("InitIngestStatus: %v", err)
	}
	if !created {
		t.Error("InitIngestStatus: want created=true on first call")
	}
	if status.State != IngestStateRegistered {
		t.Errorf("State: want registered, got %q", status.State)
	}
}

func TestInitIngestStatus_idempotent(t *testing.T) {
	reg, _ := newIngestRegistry()
	rec := makeIngestRec("ds", "v1")

	registered, _, err := reg.EnsureRegister(context.Background(), rec)
	if err != nil {
		t.Fatalf("EnsureRegister: %v", err)
	}

	s1, _, _ := reg.InitIngestStatus(context.Background(), registered)
	s2, created2, err := reg.InitIngestStatus(context.Background(), registered)
	if err != nil {
		t.Fatalf("InitIngestStatus (second): %v", err)
	}
	if created2 {
		t.Error("InitIngestStatus (second): want created=false")
	}
	if s1.AttemptID != s2.AttemptID {
		t.Errorf("AttemptID changed between idempotent calls: %q vs %q", s1.AttemptID, s2.AttemptID)
	}
}

func TestWriteIngestStatus_nilPathError(t *testing.T) {
	b := newMemBackend()
	// Registry with no IngestStatusFile path.
	reg := NewRegistry(b, testPaths(), nil)
	s := IngestStatus{
		SchemaVersion: IngestStatusSchemaVersion,
		Name:          "x",
		Version:       "v1",
		RecordDigest:  "sha256:x",
		State:         IngestStateReady,
	}
	err := reg.WriteIngestStatus(context.Background(), s)
	if err == nil {
		t.Fatal("expected error for nil IngestStatusFile path")
	}
}

func TestGetIngestStatus_notFound(t *testing.T) {
	reg, _ := newIngestRegistry()
	_, err := reg.GetIngestStatus(context.Background(), "nope", "v1")
	if err == nil {
		t.Fatal("expected error for missing status")
	}
	if !IsNotExist(err) {
		t.Errorf("expected IsNotExist error, got %v", err)
	}
}

func TestGetIngestStatus_nilPathError(t *testing.T) {
	b := newMemBackend()
	reg := NewRegistry(b, testPaths(), nil)
	_, err := reg.GetIngestStatus(context.Background(), "ds", "v1")
	if err == nil {
		t.Fatal("expected error when IngestStatusFile path is nil")
	}
}

func TestWriteAndGetIngestStatus(t *testing.T) {
	reg, _ := newIngestRegistry()
	rec := makeIngestRec("ds", "v1")

	registered, _, err := reg.EnsureRegister(context.Background(), rec)
	if err != nil {
		t.Fatalf("EnsureRegister: %v", err)
	}

	s := IngestStatus{
		SchemaVersion: IngestStatusSchemaVersion,
		Name:          "ds",
		Version:       "v1",
		RecordDigest:  registered.Digest,
		State:         IngestStateIngesting,
		AttemptID:     "attempt-xyz",
	}
	if err := reg.WriteIngestStatus(context.Background(), s); err != nil {
		t.Fatalf("WriteIngestStatus: %v", err)
	}

	got, err := reg.GetIngestStatus(context.Background(), "ds", "v1")
	if err != nil {
		t.Fatalf("GetIngestStatus: %v", err)
	}
	if got.State != IngestStateIngesting {
		t.Errorf("State: want ingesting, got %q", got.State)
	}
	if got.AttemptID != "attempt-xyz" {
		t.Errorf("AttemptID: want attempt-xyz, got %q", got.AttemptID)
	}
}
