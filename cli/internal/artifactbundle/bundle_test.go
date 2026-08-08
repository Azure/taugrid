package artifactbundle

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type memoryStore map[string][]byte

func (s memoryStore) Read(_ context.Context, name string) ([]byte, error) {
	raw, ok := s[name]
	if !ok {
		return nil, os.ErrNotExist
	}
	return append([]byte(nil), raw...), nil
}

func (s memoryStore) List(_ context.Context, prefix string) ([]Object, error) {
	var out []Object
	for name, raw := range s {
		if strings.HasPrefix(name, prefix) {
			out = append(out, Object{Name: name, Size: int64(len(raw))})
		}
	}
	return out, nil
}

func (s memoryStore) Download(_ context.Context, name string, out io.Writer) error {
	raw, ok := s[name]
	if !ok {
		return os.ErrNotExist
	}
	_, err := out.Write(raw)
	return err
}

type listErrorStore struct {
	memoryStore
	err error
}

func (s listErrorStore) List(context.Context, string) ([]Object, error) {
	return nil, s.err
}

type downloadErrorStore struct {
	memoryStore
	fail string
}

func (s downloadErrorStore) Download(ctx context.Context, name string, out io.Writer) error {
	if name == s.fail {
		return errors.New("download interrupted")
	}
	return s.memoryStore.Download(ctx, name, out)
}

func testRuntime() Runtime {
	return Runtime{
		BundleID:           "bundle-1",
		Run:                "training-1",
		Namespace:          "research",
		ResultPVC:          "blob-training",
		OutputDir:          "/data/runs/training-1",
		PublicationMode:    "staged",
		PublicationID:      "publication-1",
		PublicationRoot:    "/data/runs/training-1/.tau-artifacts/publication-1",
		PublicationMarker:  "/data/runs/training-1/.tau-artifacts/publication-1/.tau-artifacts-complete",
		MetricsSessionID:   "metrics-1",
		MetricsHistory:     []string{"/data/runs/training-1/metrics/*.jsonl"},
		MetricsOffloadDir:  "/data/runs/training-1/.tau/metrics/metrics-1/offload",
		MetricsEnabled:     true,
		CheckpointArtifact: "last.safetensors",
		CheckpointRoot:     "/data/checkpoints/finetunes/training-1",
		CheckpointIndex:    "/data/checkpoints/finetunes/training-1/artifacts.json",
	}
}

func TestWrapperCommitsOnlyAfterNestedAcknowledgements(t *testing.T) {
	runtime := testRuntime()
	root := t.TempDir()
	replace := func(script string) string {
		return strings.ReplaceAll(script, "/data", filepath.Join(root, "data"))
	}
	publicationMarker := replace(runtime.PublicationMarker)
	if err := os.MkdirAll(filepath.Dir(publicationMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicationMarker, []byte("complete publication-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpointIndex := replace(runtime.CheckpointIndex)
	if err := os.MkdirAll(filepath.Dir(checkpointIndex), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointIndex, []byte(`{"bundle_id":"bundle-1","artifacts":[{"status":"ready"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := WrapShellScript("true", runtime)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-c", replace(script)).CombinedOutput(); err != nil {
		t.Fatalf("bundle wrapper failed: %v\n%s", err, out)
	}
	manifestPath := replace(GenerationManifestPath(runtime.OutputDir, runtime.BundleID))
	if _, err := os.Stat(manifestPath); err != nil {
		t.Fatalf("generation manifest not committed: %v", err)
	}
	markerPath := replace(GenerationCompletionPath(runtime.OutputDir, runtime.BundleID))
	if raw, err := os.ReadFile(markerPath); err != nil || string(raw) != "complete bundle-1\n" {
		t.Fatalf("bundle acknowledgement = %q, %v", raw, err)
	}
}

func TestWrapperFailsClosedWithoutPublicationAcknowledgement(t *testing.T) {
	runtime := testRuntime()
	root := t.TempDir()
	script, err := WrapShellScript("true", runtime)
	if err != nil {
		t.Fatal(err)
	}
	script = strings.ReplaceAll(script, "/data", filepath.Join(root, "data"))
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "publication acknowledgement") {
		t.Fatalf("missing publication marker = %v\n%s", err, out)
	}
}

func TestWrapperFailsClosedWithoutDeclaredCheckpointIndex(t *testing.T) {
	runtime := testRuntime()
	root := t.TempDir()
	replace := func(script string) string {
		return strings.ReplaceAll(script, "/data", filepath.Join(root, "data"))
	}
	publicationMarker := replace(runtime.PublicationMarker)
	if err := os.MkdirAll(filepath.Dir(publicationMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicationMarker, []byte("complete publication-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := WrapShellScript("true", runtime)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "-c", replace(script)).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "checkpoint index is missing or belongs to another bundle") {
		t.Fatalf("missing checkpoint index = %v\n%s", err, out)
	}
}

func TestWrapperFailsClosedWithCheckpointIndexFromAnotherBundle(t *testing.T) {
	runtime := testRuntime()
	root := t.TempDir()
	replace := func(script string) string {
		return strings.ReplaceAll(script, "/data", filepath.Join(root, "data"))
	}
	publicationMarker := replace(runtime.PublicationMarker)
	if err := os.MkdirAll(filepath.Dir(publicationMarker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(publicationMarker, []byte("complete publication-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	checkpointIndex := replace(runtime.CheckpointIndex)
	if err := os.MkdirAll(filepath.Dir(checkpointIndex), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(checkpointIndex, []byte(`{"bundle_id":"old-bundle"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	script, err := WrapShellScript("true", runtime)
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "-c", replace(script)).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "belongs to another bundle") {
		t.Fatalf("stale checkpoint index = %v\n%s", err, out)
	}
}

func TestCompleteBundleLocalFixtureEnumeratesAndDownloadsWithoutKubernetes(t *testing.T) {
	runtime := testRuntime()
	manifest, err := runtime.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	rawManifest, err := Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	store := memoryStore{
		"runs/training-1/.tau/bundles/bundle-1.json":                                     rawManifest,
		"runs/training-1/.tau/bundles/bundle-1.complete":                                 []byte("complete bundle-1\n"),
		"runs/training-1/.tau-artifacts/publication-1/.tau-artifacts-complete":           []byte("complete publication-1\n"),
		"runs/training-1/.tau-artifacts/publication-1/result.json":                       []byte(`{"score":0.9}`),
		"runs/training-1/metrics/epoch-0001.jsonl":                                       []byte("{\"epoch\":1}\n"),
		"runs/training-1/.tau/metrics/metrics-1/offload/metrics-status/run-status.jsonl": []byte("{\"state\":\"succeeded\"}\n"),
		"checkpoints/finetunes/training-1/artifacts.json":                                []byte(`{"bundle_id":"bundle-1","artifacts":[{"status":"ready"}]}`),
		"checkpoints/finetunes/training-1/artifacts/last.safetensors":                    []byte("weights"),
	}
	loaded, err := Load(context.Background(), store, runtime.OutputDir, runtime.BundleID)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := Enumerate(context.Background(), store, loaded)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != len(store) {
		t.Fatalf("enumerated %d objects, want all %d: %+v", len(objects), len(store), objects)
	}
	destination := filepath.Join(t.TempDir(), "bundle")
	files, err := Download(context.Background(), store, loaded, objects, destination)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(store) {
		t.Fatalf("downloaded %d files, want %d", len(files), len(store))
	}
	for name, want := range store {
		got, err := os.ReadFile(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			t.Fatalf("read downloaded %s: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("downloaded %s = %q, want %q", name, got, want)
		}
	}
}

func TestLoadRejectsMissingBundleAcknowledgement(t *testing.T) {
	manifest, err := testRuntime().Manifest()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	store := memoryStore{"runs/training-1/.tau/bundles/bundle-1.json": raw}
	_, err = Load(context.Background(), store, manifest.ResultRoot, manifest.BundleID)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing acknowledgement error = %v", err)
	}
}

func TestEnumerateDoesNotSwallowOptionalPathErrors(t *testing.T) {
	manifest, err := testRuntime().Manifest()
	if err != nil {
		t.Fatal(err)
	}
	want := errors.New("storage unavailable")
	_, err = Enumerate(context.Background(), listErrorStore{memoryStore: memoryStore{}, err: want}, manifest)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("optional path listing error = %v", err)
	}
}

func TestDownloadRejectsExistingDestinationWithoutReplacingIt(t *testing.T) {
	store := memoryStore{"runs/training-1/result.json": []byte("new")}
	root := t.TempDir()
	target := filepath.Join(root, "runs", "training-1", "result.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Download(context.Background(), store, Manifest{}, []Object{{
		Name: "runs/training-1/result.json",
		Size: 3,
	}}, root)
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("existing destination error = %v", err)
	}
	raw, readErr := os.ReadFile(target)
	if readErr != nil || string(raw) != "existing" {
		t.Fatalf("existing destination changed to %q, err=%v", raw, readErr)
	}
}

func TestDownloadChecksSizeBeforePublishingDestination(t *testing.T) {
	store := memoryStore{"runs/training-1/result.json": []byte("actual")}
	root := filepath.Join(t.TempDir(), "bundle")
	target := filepath.Join(root, "runs", "training-1", "result.json")
	_, err := Download(context.Background(), store, Manifest{}, []Object{{
		Name: "runs/training-1/result.json",
		Size: 999,
	}}, root)
	if err == nil || !strings.Contains(err.Error(), "expected 999") {
		t.Fatalf("size mismatch error = %v", err)
	}
	if _, statErr := os.Stat(target); !os.IsNotExist(statErr) {
		t.Fatalf("size mismatch published destination: %v", statErr)
	}
}

func TestDownloadPublishesNothingWhenLaterObjectFails(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "bundle")
	store := downloadErrorStore{
		memoryStore: memoryStore{
			"runs/training-1/first.json":  []byte("first"),
			"runs/training-1/second.json": []byte("second"),
		},
		fail: "runs/training-1/second.json",
	}
	_, err := Download(context.Background(), store, Manifest{}, []Object{
		{Name: "runs/training-1/first.json", Size: 5},
		{Name: "runs/training-1/second.json", Size: 6},
	}, root)
	if err == nil || !strings.Contains(err.Error(), "download interrupted") {
		t.Fatalf("interrupted download error = %v", err)
	}
	if _, statErr := os.Stat(root); !os.IsNotExist(statErr) {
		t.Fatalf("interrupted download published destination: %v", statErr)
	}
	stages, globErr := filepath.Glob(filepath.Join(parent, ".bundle.tau-download-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(stages) != 0 {
		t.Fatalf("interrupted download left staging directories: %v", stages)
	}
}
