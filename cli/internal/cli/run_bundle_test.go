package cli

import (
	"path"
	"testing"
	"time"

	"github.com/Azure/taugrid/cli/internal/artifactbundle"
	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
)

func TestResolveArtifactBundleOwnsAllProducerPaths(t *testing.T) {
	publication := artifactpublish.Runtime{
		Mode:          artifactpublish.ModeStaged,
		OutputDir:     "/data/runs/training-1",
		StagingDir:    "/mnt/tau-output/training-1",
		PublicationID: "publication-1",
	}
	metrics := metricsoffload.Runtime{
		Image:               "registry.example/tau:v1",
		RunID:               "training-1",
		Project:             "project",
		Experiment:          "experiment",
		Group:               "group",
		Store:               "/var/run/tau/metrics/session/expstore",
		Out:                 "/data/runs/training-1/.tau/metrics/session/offload",
		History:             []string{"/data/runs/training-1/metrics/*.jsonl"},
		CompletionFile:      "/var/run/tau/metrics-completion.json",
		RemoteWriteEndpoint: "https://metrics.example/receive",
		Interval:            time.Second,
		DoneFile:            "/var/run/tau/metrics-done",
	}
	runtime, err := resolveArtifactBundle(
		"training-1",
		"research",
		"submission-1",
		"/data/runs/training-1",
		"blob-training",
		true,
		publication,
		metrics,
		"session-1",
		"last.safetensors",
	)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.BundleID != publication.PublicationID {
		t.Fatalf("bundle ID = %q, want publication ID", runtime.BundleID)
	}
	manifest, err := runtime.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Metrics == nil || !manifest.Metrics.Acknowledged || manifest.Metrics.SessionID != "session-1" {
		t.Fatalf("metrics contract = %+v", manifest.Metrics)
	}
	if manifest.Checkpoint == nil || manifest.Checkpoint.Index != "/data/checkpoints/finetunes/training-1/artifacts.json" {
		t.Fatalf("checkpoint contract = %+v", manifest.Checkpoint)
	}
	if manifest.Publication == nil ||
		manifest.Publication.Completion != path.Join(publication.PublishedDir(), artifactpublish.CompletionMarker) {
		t.Fatalf("publication contract = %+v", manifest.Publication)
	}
	if got := artifactbundle.GenerationManifestPath(runtime.OutputDir, runtime.BundleID); got != "/data/runs/training-1/.tau/bundles/publication-1.json" {
		t.Fatalf("generation manifest path = %q", got)
	}
}

func TestResolveArtifactBundleSkipsReadOnlyAndEphemeralResults(t *testing.T) {
	for _, test := range []struct {
		output   string
		pvc      string
		writable bool
	}{
		{output: "", pvc: "blob-training", writable: true},
		{output: "/data/runs/training-1", pvc: "", writable: true},
		{output: "/data/runs/training-1", pvc: "blob-training", writable: false},
		{output: "/data-nfs/runs/training-1", pvc: "shared-nfs", writable: true},
		{output: "/data/runs/training-1/result.json", pvc: "blob-training", writable: true},
	} {
		runtime, err := resolveArtifactBundle(
			"training-1", "research", "submission-1", test.output, test.pvc, test.writable,
			artifactpublish.Runtime{}, metricsoffload.Runtime{}, "", "",
		)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.Enabled() {
			t.Fatalf("runtime enabled for %+v", test)
		}
	}
}
