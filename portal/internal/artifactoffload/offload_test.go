package artifactoffload

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/portal/internal/blobstore"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestRunUploadsDedupesIndexesAndResumes(t *testing.T) {
	ctx := context.Background()
	store, sourceA, sourceB := seedArtifactStore(t, []byte("same-payload"), []byte("same-payload"))
	objectStore, err := blobstore.NewFileStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, store, Options{
		RunID:         "run-1",
		Out:           filepath.Join(t.TempDir(), "offload"),
		ObjectStore:   objectStore,
		ObjectBaseURI: objectStore.BaseURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Uploaded != 1 || result.Deduped != 1 || result.Verified != 2 || result.Indexed != 2 || result.Failed != 0 {
		t.Fatalf("unexpected first result: %+v", result)
	}
	artifacts, err := store.ArtifactsForRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts=%d, want 2", len(artifacts))
	}
	if artifacts[0].DurableRef == "" || artifacts[1].DurableRef == "" || artifacts[0].DurableRef != artifacts[1].DurableRef {
		t.Fatalf("expected both artifacts to share durable ref after digest dedupe: %+v", artifacts)
	}
	ref, err := blobstore.ParseDurableRef(artifacts[0].DurableRef)
	if err != nil {
		t.Fatal(err)
	}
	if err := objectStore.Verify(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ref.BlobPath, "/experiment=image-resnet/") {
		t.Fatalf("durable artifact path does not contain the run experiment: %q", ref.BlobPath)
	}
	if artifacts[0].ContentType == "" || artifacts[0].Digest == "" || artifacts[0].SizeBytes == nil {
		t.Fatalf("artifact metadata not updated: %+v", artifacts[0])
	}
	if _, err := os.Stat(result.Checkpoint); err != nil {
		t.Fatalf("checkpoint not written: %v", err)
	}

	resume, err := Run(ctx, store, Options{
		RunID:         "run-1",
		Out:           filepath.Dir(result.Checkpoint),
		ObjectStore:   objectStore,
		ObjectBaseURI: objectStore.BaseURI,
	})
	if err != nil {
		t.Fatal(err)
	}
	if resume.Uploaded != 0 || resume.Deduped != 0 || resume.Skipped != 2 || resume.Indexed != 0 || resume.Failed != 0 {
		t.Fatalf("unexpected resume result: %+v", resume)
	}
	if _, err := os.Stat(sourceA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sourceB); err != nil {
		t.Fatal(err)
	}
}

func TestRunDoesNotIndexFailedVerify(t *testing.T) {
	ctx := context.Background()
	store, _, _ := seedArtifactStore(t, []byte("payload"), []byte("other"))
	objectStore := verifyFailStore{}
	result, err := Run(ctx, store, Options{
		RunID:         "run-1",
		Out:           filepath.Join(t.TempDir(), "offload"),
		ObjectStore:   objectStore,
		ObjectBaseURI: "file:///broken",
	})
	if !errors.Is(err, ErrPartialFailure) {
		t.Fatalf("expected partial failure, got %v", err)
	}
	if result.Failed != 2 || result.Indexed != 0 {
		t.Fatalf("unexpected failed result: %+v", result)
	}
	artifacts, err := store.ArtifactsForRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, artifact := range artifacts {
		if artifact.DurableRef != "" {
			t.Fatalf("failed upload should not update durable ref: %+v", artifact)
		}
	}
}

func TestRunIndexesVerifiedArtifactsWhenOneArtifactFails(t *testing.T) {
	ctx := context.Background()
	store, sourceA, sourceB := seedArtifactStore(t, []byte("good-payload"), []byte("missing-payload"))
	if err := os.Remove(sourceB); err != nil {
		t.Fatal(err)
	}
	objectStore, err := blobstore.NewFileStore(filepath.Join(t.TempDir(), "objects"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := Run(ctx, store, Options{
		RunID:         "run-1",
		Out:           filepath.Join(t.TempDir(), "offload"),
		ObjectStore:   objectStore,
		ObjectBaseURI: objectStore.BaseURI,
	})
	if !errors.Is(err, ErrPartialFailure) {
		t.Fatalf("expected partial failure, got %v", err)
	}
	if result.Uploaded != 1 || result.Verified != 1 || result.Indexed != 1 || result.Failed != 1 {
		t.Fatalf("unexpected partial result: %+v", result)
	}
	artifacts, err := store.ArtifactsForRun(ctx, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 2 {
		t.Fatalf("artifacts=%d, want 2", len(artifacts))
	}
	byID := map[string]expstore.ArtifactRecord{}
	for _, artifact := range artifacts {
		byID[artifact.ArtifactID] = artifact
	}
	if byID["artifact-run-1-a"].DurableRef == "" {
		t.Fatalf("verified artifact was not indexed: %+v", byID["artifact-run-1-a"])
	}
	if byID["artifact-run-1-b"].DurableRef != "" {
		t.Fatalf("failed artifact should not have durable ref: %+v", byID["artifact-run-1-b"])
	}
	if _, err := os.Stat(sourceA); err != nil {
		t.Fatal(err)
	}
}

func seedArtifactStore(t *testing.T, first, second []byte) (*expstore.Store, string, string) {
	t.Helper()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "store")
	store, _, err := expstore.Init(ctx, root, expstore.InitOptions{
		Name:        "image-resnet",
		Project:     "vision",
		Description: "Can a ResNet classify the sample images?",
		Group:       "h200",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	artifactDir := filepath.Join(root, expstore.ArtifactsDir, "run-1")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sourceA := filepath.Join(artifactDir, "sample-a.png")
	sourceB := filepath.Join(artifactDir, "sample-b.png")
	if err := os.WriteFile(sourceA, first, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourceB, second, 0o644); err != nil {
		t.Fatal(err)
	}
	relA, err := filepath.Rel(root, sourceA)
	if err != nil {
		t.Fatal(err)
	}
	relB, err := filepath.Rel(root, sourceB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:        "run-1",
			Project:      "vision",
			ExperimentID: "image-resnet",
			RunGroupID:   "h200",
			State:        "succeeded",
		},
		Artifacts: []expstore.ArtifactRecord{
			{
				ArtifactID: "artifact-run-1-a",
				RunID:      "run-1",
				Type:       "image",
				URI:        filepath.ToSlash(relA),
				Name:       "sample-a.png",
				CreatedAt:  "2026-06-16T00:00:00Z",
			},
			{
				ArtifactID: "artifact-run-1-b",
				RunID:      "run-1",
				Type:       "image",
				URI:        filepath.ToSlash(relB),
				Name:       "sample-b.png",
				CreatedAt:  "2026-06-16T00:00:01Z",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, sourceA, sourceB
}

type verifyFailStore struct{}

func (verifyFailStore) UploadFile(context.Context, blobstore.DurableRef, string) (blobstore.DurableRef, error) {
	return blobstore.DurableRef{}, nil
}

func (verifyFailStore) Verify(context.Context, blobstore.DurableRef) error {
	return errors.New("verify failed")
}

func (verifyFailStore) SignedURL(context.Context, blobstore.DurableRef, time.Duration) (string, error) {
	return "", errors.New("signed URL unavailable")
}
