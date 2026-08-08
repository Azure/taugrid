package cli

import (
	"path"
	"strings"

	"github.com/Azure/taugrid/cli/internal/artifactbundle"
	"github.com/Azure/taugrid/cli/internal/artifactpublish"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/cli/internal/storage"
)

func resolveArtifactBundle(
	run, namespace, submissionID, outputDir, resultPVC string,
	outputWritable bool,
	publication artifactpublish.Runtime,
	metrics metricsoffload.Runtime,
	metricsSessionID string,
	checkpointArtifact string,
) (artifactbundle.Runtime, error) {
	outputDir = path.Clean(strings.TrimSpace(outputDir))
	if outputDir == "." || strings.TrimSpace(resultPVC) == "" || !outputWritable {
		return artifactbundle.Runtime{}, nil
	}
	if outputDir != "/data" && !strings.HasPrefix(outputDir, "/data/") {
		return artifactbundle.Runtime{}, nil
	}
	bundleID := firstNonEmpty(publication.PublicationID, submissionID)
	if bundleID == "" {
		return artifactbundle.Runtime{}, nil
	}
	runtime := artifactbundle.Runtime{
		BundleID:           bundleID,
		Run:                run,
		Namespace:          namespace,
		ResultPVC:          resultPVC,
		OutputDir:          outputDir,
		PublicationMode:    publication.Mode,
		PublicationID:      publication.PublicationID,
		MetricsSessionID:   strings.TrimSpace(metricsSessionID),
		MetricsHistory:     append([]string(nil), metrics.History...),
		MetricsOffloadDir:  metrics.Out,
		MetricsEnabled:     metrics.Enabled(),
		CheckpointArtifact: strings.TrimSpace(checkpointArtifact),
	}
	if publication.Enabled() {
		runtime.PublicationRoot = publication.PublishedDir()
		runtime.PublicationMarker = path.Join(publication.PublishedDir(), artifactpublish.CompletionMarker)
	}
	if runtime.CheckpointArtifact != "" {
		runtime.CheckpointRoot = storage.DurableFinetuneDir(run)
		runtime.CheckpointIndex = storage.DurableFinetuneArtifactsFile(run)
	}
	if err := runtime.Validate(); err != nil {
		return artifactbundle.Runtime{}, err
	}
	return runtime, nil
}
