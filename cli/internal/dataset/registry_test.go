package dataset

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"
)

// memBackend is an in-memory Backend for unit tests.
type memBackend struct {
	files map[string][]byte
}

func newMemBackend() *memBackend { return &memBackend{files: map[string][]byte{}} }

func (m *memBackend) ReadFile(_ context.Context, path string) ([]byte, error) {
	b, ok := m.files[path]
	if !ok {
		return nil, ErrNotExist
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out, nil
}

func (m *memBackend) WriteFile(_ context.Context, path string, data []byte, overwrite bool) error {
	if !overwrite {
		if _, ok := m.files[path]; ok {
			return ErrExist
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
		return nil, ErrNotExist
	}
	var out []string
	for s := range seen {
		out = append(out, s)
	}
	sort.Strings(out)
	return out, nil
}

func (m *memBackend) Delete(_ context.Context, path string) error {
	delete(m.files, path)
	return nil
}

// testPaths mirrors the storage.DatasetRegistry* layout without importing it.
func testPaths() Paths {
	root := "/data/dataset-registry/datasets"
	return Paths{
		DatasetsDir: func() string { return root },
		DatasetDir:  func(n string) string { return root + "/" + n },
		VersionDir:  func(n, v string) string { return root + "/" + n + "/" + v },
		RecordFile:  func(n, v string) string { return root + "/" + n + "/" + v + "/dataset.json" },
		AliasesDir:  func(n string) string { return root + "/" + n + "/aliases" },
		AliasFile:   func(n, a string) string { return root + "/" + n + "/aliases/" + a + ".json" },
	}
}

const (
	shaA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	shaB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func validPretrain() Record {
	rec := Record{
		SchemaVersion: SchemaVersion,
		Name:          "fineweb-sample-10bt",
		Version:       "v1",
		Purpose:       PurposePretrain,
		Account:       "datasetsacct",
		Container:     "datasets",
		Prefix:        "pretrain/fineweb-sample-10bt/v1",
		Assurance:     AssuranceVerified,
		Files: []File{
			{Path: "shard-000.bin", Bytes: 200, SHA256: shaA, TokenCount: 100},
			{Path: "shard-001.bin", Bytes: 400, SHA256: shaB, TokenCount: 200},
		},
		Pretrain: &Pretrain{Tokenizer: "gpt2", Format: FormatTokenizedBinUint16, TotalTokens: 300},
		Tags:     map[string]string{"team": "taugrid"},
	}
	rec.TotalBytes = rec.SumBytes()
	rec.Digest = rec.ComputeDigest()
	return rec
}

func TestRecordRoundTrip(t *testing.T) {
	rec := validPretrain()
	raw, err := rec.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseRecord(raw)
	if err != nil {
		t.Fatalf("ParseRecord: %v", err)
	}
	if got.Name != rec.Name || got.Version != rec.Version || got.Digest != rec.Digest {
		t.Fatalf("round-trip mismatch: got %+v", got)
	}
	if got.Pretrain == nil || got.Pretrain.Format != FormatTokenizedBinUint16 {
		t.Fatalf("pretrain section lost in round-trip: %+v", got.Pretrain)
	}
}

func TestRegistryCompatibilityFixturesParse(t *testing.T) {
	fixture := validPretrain()
	digest := fixture.ComputeDigest()
	raw := []byte(fmt.Sprintf(`{
  "schema_version": 1,
  "name": "fineweb-sample-10bt",
  "version": "v1",
  "purpose": "pretrain",
  "source": {
    "kind": "huggingface",
    "repo": "HuggingFaceFW/fineweb",
    "revision": "abc123",
    "config": "sample-10BT"
  },
  "account": "datasetsacct",
  "container": "datasets",
  "prefix": "pretrain/fineweb-sample-10bt/v1",
  "files": [
    { "path": "shard-000.bin", "bytes": 200, "sha256": %q, "token_count": 100 },
    { "path": "shard-001.bin", "bytes": 400, "sha256": %q, "token_count": 200 }
  ],
  "total_bytes": 600,
  "digest": %q,
  "assurance": "verified",
  "created_at": "2026-06-02T00:00:00Z",
  "tags": { "team": "taugrid" },
  "pretrain": {
    "tokenizer": "gpt2",
    "format": "tokenized-bin-uint16",
    "total_tokens": 300,
    "sequence_packing": false
  }
}`, shaA, shaB, digest))
	rec, err := ParseRecord(raw)
	if err != nil {
		t.Fatalf("ParseRecord compatibility fixture: %v", err)
	}
	if rec.SchemaVersion != SchemaVersion || rec.Name != "fineweb-sample-10bt" || rec.Pretrain == nil {
		t.Fatalf("parsed compatibility record lost required fields: %+v", rec)
	}

	aliasRaw := []byte(`{
  "schema_version": 1,
  "name": "fineweb-sample-10bt",
  "alias": "latest",
  "version": "v1",
  "digest": "sha256:fixture",
  "record_path": "/data/dataset-registry/datasets/fineweb-sample-10bt/v1/dataset.json",
  "updated_at": "2026-06-02T00:00:00Z"
}`)
	alias, err := ParseAlias(aliasRaw)
	if err != nil {
		t.Fatalf("ParseAlias compatibility fixture: %v", err)
	}
	if alias.SchemaVersion != SchemaVersion || alias.RecordPath == "" {
		t.Fatalf("parsed compatibility alias lost required fields: %+v", alias)
	}
}

func TestDigestDeterministicOrderIndependent(t *testing.T) {
	a := validPretrain()
	b := validPretrain()
	// Reverse file order in b; digest must be identical.
	b.Files[0], b.Files[1] = b.Files[1], b.Files[0]
	if a.ComputeDigest() != b.ComputeDigest() {
		t.Fatalf("digest is order-dependent: %s != %s", a.ComputeDigest(), b.ComputeDigest())
	}
	// Changing a file hash must change the digest.
	c := validPretrain()
	c.Files[0].SHA256 = shaB
	if c.ComputeDigest() == a.ComputeDigest() {
		t.Fatal("digest did not change when a file hash changed")
	}
}

func TestValidatePurposeExclusivity(t *testing.T) {
	rec := validPretrain()
	rec.Eval = &Eval{Task: "x"} // two purpose sections now present
	if err := rec.Validate(); err == nil {
		t.Fatal("expected validation to reject two purpose sections")
	}

	rec = validPretrain()
	rec.Purpose = PurposeRL // section is pretrain, mismatch
	if err := rec.Validate(); err == nil {
		t.Fatal("expected validation to reject purpose/section mismatch")
	}
}

func TestValidateTokenAccounting(t *testing.T) {
	rec := validPretrain()
	rec.Files[0].TokenCount = 99 // not bytes/2
	rec.Digest = rec.ComputeDigest()
	rec.Pretrain.TotalTokens = 0
	if err := rec.Validate(); err == nil {
		t.Fatal("expected verified uint16 token_count!=bytes/2 to fail")
	}

	// manifest-supplied relaxes the bytes/2 requirement.
	rec = validPretrain()
	rec.Assurance = AssuranceManifestSupplied
	rec.Files[0].TokenCount = 99
	rec.Pretrain.TotalTokens = 0
	rec.Digest = rec.ComputeDigest()
	if err := rec.Validate(); err != nil {
		t.Fatalf("manifest-supplied counts should be allowed: %v", err)
	}
}

func TestValidateRejectsBadFilePaths(t *testing.T) {
	for _, bad := range []string{"/abs/shard.bin", "../escape.bin", "a//b.bin", "./x.bin"} {
		rec := validPretrain()
		rec.Files = []File{{Path: bad, Bytes: 2, SHA256: shaA, TokenCount: 1}}
		rec.Pretrain.TotalTokens = 0
		rec.TotalBytes = rec.SumBytes()
		rec.Digest = rec.ComputeDigest()
		if err := rec.Validate(); err == nil {
			t.Fatalf("expected path %q to be rejected", bad)
		}
	}
}

func TestValidateDigestMismatch(t *testing.T) {
	rec := validPretrain()
	rec.Digest = "sha256:deadbeef"
	if err := rec.Validate(); err == nil {
		t.Fatal("expected digest mismatch to fail validation")
	}
}

func TestValidateComponents(t *testing.T) {
	rec := validPretrain()
	rec.Components = []Component{{
		Source:     "source-a",
		Domain:     "science",
		Split:      "train",
		License:    "cc-by-4.0",
		Provenance: "https://example.test/source-a",
	}}
	for i := range rec.Files {
		rec.Files[i].Source = "source-a"
		rec.Files[i].Domain = "science"
		rec.Files[i].Split = "train"
	}
	if err := rec.Validate(); err != nil {
		t.Fatalf("component metadata should validate: %v", err)
	}

	rec.Files[0].Split = "test"
	if err := rec.Validate(); err == nil || !strings.Contains(err.Error(), "does not match component") {
		t.Fatalf("mismatched file split should fail, got %v", err)
	}

	rec = validPretrain()
	rec.Files[0].Domain = "science"
	if err := rec.Validate(); err == nil || !strings.Contains(err.Error(), "requires record components") {
		t.Fatalf("file metadata without components should fail, got %v", err)
	}
}

func newTestRegistry() (*Registry, *memBackend) {
	b := newMemBackend()
	fixed := func() time.Time { return time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC) }
	return NewRegistry(b, testPaths(), fixed), b
}

func TestRegisterRefusesOverwrite(t *testing.T) {
	reg, _ := newTestRegistry()
	ctx := context.Background()
	rec := validPretrain()
	if _, err := reg.Register(ctx, rec); err != nil {
		t.Fatalf("first register: %v", err)
	}
	_, err := reg.Register(ctx, rec)
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("expected immutability refusal, got %v", err)
	}
}

func TestRegisterFillsDigestAndCreatedAt(t *testing.T) {
	reg, _ := newTestRegistry()
	rec := validPretrain()
	rec.Digest = ""
	rec.TotalBytes = 0
	rec.CreatedAt = ""
	got, err := reg.Register(context.Background(), rec)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if got.Digest == "" || got.TotalBytes != 600 || got.CreatedAt == "" {
		t.Fatalf("register did not fill derived fields: %+v", got)
	}
}

func TestAliasCompareAndSwap(t *testing.T) {
	reg, _ := newTestRegistry()
	ctx := context.Background()
	v1 := validPretrain()
	if _, err := reg.Register(ctx, v1); err != nil {
		t.Fatal(err)
	}
	v2 := validPretrain()
	v2.Version = "v2"
	if _, err := reg.Register(ctx, v2); err != nil {
		t.Fatal(err)
	}

	// First set requires absence.
	if _, err := reg.SetAlias(ctx, "fineweb-sample-10bt", "latest", "v1", SetAliasOptions{ExpectAbsent: true}); err != nil {
		t.Fatalf("initial alias set: %v", err)
	}
	// Wrong expectation aborts.
	if _, err := reg.SetAlias(ctx, "fineweb-sample-10bt", "latest", "v2", SetAliasOptions{Expect: "v2"}); err == nil {
		t.Fatal("expected CAS abort when current!=expect")
	}
	// Correct expectation succeeds.
	if _, err := reg.SetAlias(ctx, "fineweb-sample-10bt", "latest", "v2", SetAliasOptions{Expect: "v1"}); err != nil {
		t.Fatalf("CAS move v1->v2: %v", err)
	}
	got, err := reg.GetAlias(ctx, "fineweb-sample-10bt", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "v2" {
		t.Fatalf("alias=%s want v2", got.Version)
	}
}

func TestAliasTargetMustExist(t *testing.T) {
	reg, _ := newTestRegistry()
	_, err := reg.SetAlias(context.Background(), "fineweb-sample-10bt", "latest", "v9", SetAliasOptions{})
	if err == nil {
		t.Fatal("expected alias to a missing version to fail")
	}
}

func TestResolveAliasToVersion(t *testing.T) {
	reg, _ := newTestRegistry()
	ctx := context.Background()
	rec := validPretrain()
	if _, err := reg.Register(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetAlias(ctx, "fineweb-sample-10bt", "prod", "v1", SetAliasOptions{}); err != nil {
		t.Fatal(err)
	}

	// Bare name -> default alias.
	if _, err := reg.SetAlias(ctx, "fineweb-sample-10bt", DefaultAlias, "v1", SetAliasOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []string{"fineweb-sample-10bt", "fineweb-sample-10bt@v1", "fineweb-sample-10bt@prod"} {
		parsed, err := ParseRef(ref)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", ref, err)
		}
		got, err := reg.Resolve(ctx, parsed)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", ref, err)
		}
		if got.Version != "v1" {
			t.Fatalf("Resolve(%q)=%s want v1", ref, got.Version)
		}
	}
}

func TestRemoveBlockedByAlias(t *testing.T) {
	reg, _ := newTestRegistry()
	ctx := context.Background()
	rec := validPretrain()
	if _, err := reg.Register(ctx, rec); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetAlias(ctx, "fineweb-sample-10bt", "latest", "v1", SetAliasOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove(ctx, "fineweb-sample-10bt", "v1"); err == nil {
		t.Fatal("expected remove to be blocked by alias")
	}
	// Move the alias to a different version, then removal should proceed.
	v2 := validPretrain()
	v2.Version = "v2"
	if _, err := reg.Register(ctx, v2); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.SetAlias(ctx, "fineweb-sample-10bt", "latest", "v2", SetAliasOptions{Expect: "v1"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Remove(ctx, "fineweb-sample-10bt", "v1"); err != nil {
		t.Fatalf("remove after alias move: %v", err)
	}
	if _, err := reg.Get(ctx, "fineweb-sample-10bt", "v1"); err == nil {
		t.Fatal("expected v1 to be gone after removal")
	}
}

func TestListFiltersByPurposeAndTag(t *testing.T) {
	reg, _ := newTestRegistry()
	ctx := context.Background()
	if _, err := reg.Register(ctx, validPretrain()); err != nil {
		t.Fatal(err)
	}
	rl := Record{
		SchemaVersion: SchemaVersion, Name: "ultrafeedback", Version: "v1", Purpose: PurposeRL,
		Assurance: AssuranceTrusted,
		Files:     []File{{Path: "prefs.jsonl", Bytes: 10, SHA256: shaA}},
		RL:        &RL{Kind: "preferences"},
		Tags:      map[string]string{"team": "rl"},
	}
	if _, err := reg.Register(ctx, rl); err != nil {
		t.Fatal(err)
	}

	got, warns, err := reg.List(ctx, PurposeRL, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if len(got) != 1 || got[0].Name != "ultrafeedback" {
		t.Fatalf("purpose filter wrong: %+v", got)
	}

	got, _, err = reg.List(ctx, "", map[string]string{"team": "taugrid"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "fineweb-sample-10bt" {
		t.Fatalf("tag filter wrong: %+v", got)
	}
}

func TestBuildReferenceModes(t *testing.T) {
	rec := validPretrain()
	rec.Components = []Component{{
		Source:     "source-a",
		Domain:     "science",
		Split:      "train",
		License:    "cc-by-4.0",
		Provenance: "https://example.test/source-a",
	}}
	for i := range rec.Files {
		rec.Files[i].Source = "source-a"
		rec.Files[i].Domain = "science"
		rec.Files[i].Split = "train"
	}
	ref := BuildReference(rec, ReferenceOptions{
		DurableDatasetsDir: "/data/datasets",
		HotDatasetsDir:     "/mnt/datasets",
		ManifestPath:       "/data/dataset-registry/datasets/fineweb-sample-10bt/v1/dataset.json",
	})
	if ref.Recommended != ModeNodeLocalStage {
		t.Fatalf("recommended=%s want node_local_stage", ref.Recommended)
	}
	if ref.Modes.DurableMount != "/data/datasets/pretrain/fineweb-sample-10bt/v1" {
		t.Fatalf("durable_mount=%s", ref.Modes.DurableMount)
	}
	if ref.Modes.HotCache != "/mnt/datasets/fineweb-sample-10bt/v1" {
		t.Fatalf("hot_cache=%s", ref.Modes.HotCache)
	}
	if len(ref.Modes.NodeLocalStage.Files) != 2 {
		t.Fatalf("expected 2 stage files, got %d", len(ref.Modes.NodeLocalStage.Files))
	}
	if ref.Modes.NodeLocalStage.Files[0].Path != "shard-000.bin" {
		t.Fatalf("stage files not sorted: %+v", ref.Modes.NodeLocalStage.Files)
	}
	if ref.Modes.NodeLocalStage.Files[0].Domain != "science" || len(ref.Components) != 1 {
		t.Fatalf("component metadata lost from reference: %+v", ref)
	}
	url, err := ref.Modes.NodeLocalStage.BlobURL(ref.Modes.NodeLocalStage.Files[0])
	if err != nil {
		t.Fatal(err)
	}
	want := "https://datasetsacct.blob.core.windows.net/datasets/pretrain/fineweb-sample-10bt/v1/shard-000.bin"
	if url != want {
		t.Fatalf("BlobURL=%s want %s", url, want)
	}
}

func TestBuildReferenceEvalRecommendsDurable(t *testing.T) {
	rec := Record{
		SchemaVersion: SchemaVersion, Name: "mmlu", Version: "v1", Purpose: PurposeEval,
		Assurance: AssuranceTrusted,
		Files:     []File{{Path: "test.jsonl", Bytes: 10, SHA256: shaA}},
		Eval:      &Eval{Task: "mmlu", Split: "test", Metric: "accuracy"},
	}
	rec.TotalBytes = rec.SumBytes()
	rec.Digest = rec.ComputeDigest()
	ref := BuildReference(rec, ReferenceOptions{DurableDatasetsDir: "/data/datasets", HotDatasetsDir: "/mnt/datasets"})
	if ref.Recommended != ModeDurableMount {
		t.Fatalf("eval recommended=%s want durable_mount", ref.Recommended)
	}
}

func TestParseRef(t *testing.T) {
	cases := map[string]Ref{
		"fineweb":    {Name: "fineweb", Alias: DefaultAlias},
		"fineweb@v1": {Name: "fineweb", Version: "v1"},
	}
	for in, want := range cases {
		got, err := ParseRef(in)
		if err != nil {
			t.Fatalf("ParseRef(%q): %v", in, err)
		}
		if got != want {
			t.Fatalf("ParseRef(%q)=%+v want %+v", in, got, want)
		}
	}
	if _, err := ParseRef("fineweb@"); err == nil {
		t.Fatal("expected empty version/alias to fail")
	}
}
