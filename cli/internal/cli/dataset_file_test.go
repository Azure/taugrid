package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/storage"
)

// fileRegistry builds a Registry backed by a fresh fileBackend rooted at a temp
// dir, mirroring how registryClient() wires file://<dir>.
func fileRegistry(t *testing.T) (*dataset.Registry, string) {
	t.Helper()
	root := t.TempDir()
	return dataset.NewRegistry(newFileBackend(root), datasetRegistryPaths(), nil), root
}

func TestFileBackendRoundTrip(t *testing.T) {
	root := t.TempDir()
	b := newFileBackend(root)
	ctx := context.Background()

	p := storage.DatasetRegistryRecordFile("demo", "v1")
	if err := b.WriteFile(ctx, p, []byte(`{"ok":true}`), false); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// The absolute registry path must be mapped under root, stripping the
	// DatasetRegistryDir prefix.
	lp, err := b.localPath(p)
	if err != nil {
		t.Fatalf("localPath: %v", err)
	}
	rel, err := filepath.Rel(root, lp)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if filepath.IsAbs(rel) || rel == "" || rel[0] == '.' {
		t.Fatalf("localPath escaped root: %q", rel)
	}

	got, err := b.ReadFile(ctx, p)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != `{"ok":true}` {
		t.Fatalf("ReadFile = %q", got)
	}
}

func TestFileBackendImmutability(t *testing.T) {
	b := newFileBackend(t.TempDir())
	ctx := context.Background()
	p := storage.DatasetRegistryRecordFile("demo", "v1")

	if err := b.WriteFile(ctx, p, []byte("first"), false); err != nil {
		t.Fatalf("first WriteFile: %v", err)
	}

	// overwrite=false must refuse with ErrExist (O_EXCL), proving immutability
	// does not rely on a racy read-then-write.
	if err := b.WriteFile(ctx, p, []byte("second"), false); !dataset.IsExist(err) {
		t.Fatalf("overwrite=false: want IsExist, got %v", err)
	}
	got, _ := b.ReadFile(ctx, p)
	if string(got) != "first" {
		t.Fatalf("record mutated: %q", got)
	}
	// overwrite=true is allowed (aliases use it for CAS).
	if err := b.WriteFile(ctx, p, []byte("third"), true); err != nil {
		t.Fatalf("overwrite=true: %v", err)
	}
	got, _ = b.ReadFile(ctx, p)
	if string(got) != "third" {
		t.Fatalf("overwrite=true not applied: %q", got)
	}
}

func TestFileBackendIngestStatusAtomicReplacement(t *testing.T) {
	reg, root := fileRegistryWithIngest(t)
	rec := ingestRecord(t, reg, "ds", "v1", []dataset.File{{
		Path: "part..0001.jsonl", Bytes: 1, SHA256: strings.Repeat("a", 64),
	}})
	status := dataset.IngestStatus{
		SchemaVersion: dataset.IngestStatusSchemaVersion,
		Name:          rec.Name,
		Version:       rec.Version,
		RecordDigest:  rec.Digest,
		State:         dataset.IngestStateIngesting,
		AttemptID:     "attempt-atomic",
	}
	if err := reg.WriteIngestStatus(context.Background(), status); err != nil {
		t.Fatalf("first status write: %v", err)
	}
	status.State = dataset.IngestStateReady
	if err := reg.WriteIngestStatus(context.Background(), status); err != nil {
		t.Fatalf("replacement status write: %v", err)
	}
	statusPath, err := newFileBackend(root).localPath(storage.DatasetRegistryIngestStatusFile("ds", "v1"))
	if err != nil {
		t.Fatalf("resolve status path: %v", err)
	}
	raw, err := os.ReadFile(statusPath)
	if err != nil {
		t.Fatalf("read replaced status: %v", err)
	}
	parsed, err := dataset.ParseIngestStatus(raw)
	if err != nil || parsed.State != dataset.IngestStateReady {
		t.Fatalf("replacement must leave valid ready JSON: status=%+v err=%v", parsed, err)
	}
	artifacts, err := filepath.Glob(filepath.Join(filepath.Dir(statusPath), ".tau-ds-tmp-*"))
	if err != nil {
		t.Fatalf("glob temp artifacts: %v", err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("atomic replacement left temp artifacts: %v", artifacts)
	}
}

func TestFileBackendReadMissing(t *testing.T) {
	b := newFileBackend(t.TempDir())
	if _, err := b.ReadFile(context.Background(), storage.DatasetRegistryRecordFile("nope", "v1")); !dataset.IsNotExist(err) {
		t.Fatalf("ReadFile missing: want IsNotExist, got %v", err)
	}
}

func TestFileBackendList(t *testing.T) {
	b := newFileBackend(t.TempDir())
	ctx := context.Background()
	for _, v := range []string{"v1", "v2"} {
		if err := b.WriteFile(ctx, storage.DatasetRegistryRecordFile("demo", v), []byte("{}"), false); err != nil {
			t.Fatalf("WriteFile %s: %v", v, err)
		}
	}
	names, err := b.List(ctx, storage.DatasetRegistryDatasetDir("demo"))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if !got["v1"] || !got["v2"] {
		t.Fatalf("List = %v; want v1,v2", names)
	}
}

func TestFileBackendRejectsOutsidePaths(t *testing.T) {
	b := newFileBackend(t.TempDir())
	ctx := context.Background()
	for _, p := range []string{
		"/etc/passwd",
		"/data/model-registry/models/x/y.json",
		storage.DatasetRegistryDir + "/../escape.json",
	} {
		if _, err := b.localPath(p); err == nil {
			t.Errorf("localPath(%q): want error, got nil", p)
		}
		if err := b.WriteFile(ctx, p, []byte("x"), false); err == nil {
			t.Errorf("WriteFile(%q): want error, got nil", p)
		}
	}
}

// TestRegisterManifestDefaultsAssurance verifies the CLI defaults --manifest
// registrations to manifest-supplied (not verified) when --assurance is unset,
// since no bytes are hashed on the manifest path.
func TestRegisterManifestDefaultsAssurance(t *testing.T) {
	root := t.TempDir()
	regURL := "file://" + root

	mdir := t.TempDir()
	manifestPath := filepath.Join(mdir, "m.json")
	man := datasetManifest{
		Source: dataset.Source{Kind: "huggingface", Repo: "org/ds", Revision: "abc123"},
		Components: []dataset.Component{{
			Source: "source-a", Domain: "science", Split: "train",
			License: "cc-by-4.0", Provenance: "https://example.test/source-a",
		}},
		Files: []manifestFile{
			{
				Path: "a.parquet", Bytes: 10,
				SHA256: "00000000000000000000000000000000000000000000000000000000000000aa",
				Source: "source-a", Domain: "science", Split: "train",
			},
		},
	}
	raw, _ := json.Marshal(man)
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newDatasetRegisterCmd()
	cmd.SetArgs([]string{
		"manifest-ds@v1",
		"--registry", regURL,
		"--purpose", "pretrain",
		"--format", "parquet",
		"--tokenizer", "none",
		"--manifest", manifestPath,
	})
	cmd.SetOut(new(bytes.Buffer))
	cmd.SetErr(new(bytes.Buffer))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("register --manifest: %v", err)
	}

	reg := dataset.NewRegistry(newFileBackend(root), datasetRegistryPaths(), nil)
	rec, err := reg.Get(context.Background(), "manifest-ds", "v1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if rec.Assurance != dataset.AssuranceManifestSupplied {
		t.Fatalf("assurance = %q; want %q", rec.Assurance, dataset.AssuranceManifestSupplied)
	}
	if len(rec.Components) != 1 || rec.Files[0].Domain != "science" {
		t.Fatalf("component metadata was not preserved: %+v", rec)
	}
}

func TestLoadDatasetManifest(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.json")
	m := datasetManifest{
		Source: dataset.Source{Kind: "huggingface", Repo: "org/ds", Revision: "abc"},
		Files: []manifestFile{
			{Path: "a.parquet", Bytes: 10, SHA256: "00"},
			{Path: "b.parquet", Bytes: 20, SHA256: "11"},
		},
	}
	raw, _ := json.Marshal(m)
	if err := os.WriteFile(good, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadDatasetManifest(good)
	if err != nil {
		t.Fatalf("loadDatasetManifest: %v", err)
	}
	if loaded.Source.Repo != "org/ds" || len(loaded.Files) != 2 || loaded.Files[0].Path != "a.parquet" {
		t.Fatalf("loaded mismatch: %+v", loaded)
	}

	// Empty file list is rejected.
	empty := filepath.Join(dir, "empty.json")
	os.WriteFile(empty, []byte(`{"source":{"kind":"huggingface"},"files":[]}`), 0o644)
	if _, err := loadDatasetManifest(empty); err == nil {
		t.Fatal("empty manifest: want error")
	}

	// Missing file is an error.
	if _, err := loadDatasetManifest(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("missing manifest: want error")
	}

	// Invalid JSON is an error.
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte(`{not json`), 0o644)
	if _, err := loadDatasetManifest(bad); err == nil {
		t.Fatal("invalid manifest: want error")
	}
}

// TestFileBackendRegistryEndToEnd exercises register -> get -> list -> alias ->
// resolve over the file backend, the same path the seed script drives.
func TestFileBackendRegistryEndToEnd(t *testing.T) {
	reg, _ := fileRegistry(t)
	ctx := context.Background()

	rec := dataset.Record{
		Name:      "demo-ds",
		Version:   "v1",
		Purpose:   "pretrain",
		Assurance: dataset.AssuranceManifestSupplied,
		Files: []dataset.File{
			{Path: "shard-0.parquet", Bytes: 100, SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
			{Path: "shard-1.parquet", Bytes: 200, SHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
		},
		Pretrain: &dataset.Pretrain{Format: "parquet", Tokenizer: "none"},
	}
	written, err := reg.Register(ctx, rec)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if written.Digest == "" || written.TotalBytes != 300 {
		t.Fatalf("Register filled record wrong: digest=%q total=%d", written.Digest, written.TotalBytes)
	}

	// Re-register must refuse (immutability) end to end.
	if _, err := reg.Register(ctx, rec); err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("re-register: want already-registered error, got %v", err)
	}

	got, err := reg.Get(ctx, "demo-ds", "v1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Digest != written.Digest {
		t.Fatalf("Get digest mismatch: %q vs %q", got.Digest, written.Digest)
	}

	// Alias CAS: set latest -> v1, then resolve.
	if _, err := reg.SetAlias(ctx, "demo-ds", "latest", "v1", dataset.SetAliasOptions{}); err != nil {
		t.Fatalf("SetAlias: %v", err)
	}
	resolved, err := reg.Resolve(ctx, dataset.Ref{Name: "demo-ds", Alias: "latest"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if resolved.Version != "v1" {
		t.Fatalf("Resolve latest = %q; want v1", resolved.Version)
	}
}
