package cli

import (
	"context"

	"github.com/Azure/taugrid/core/experiment"
)

// The run-experiment metadata contract moved to taucore/experiment when the
// Stellar and Portal surface became its own binary: taugrid-portal reads the
// same identity back out of a run config. These aliases keep the tau call
// sites spelled the way they always were.
type runExperimentMetadata = experiment.RunMetadata

func withRunExperimentMetadata(ctx context.Context, meta runExperimentMetadata) context.Context {
	return experiment.WithRunMetadata(ctx, meta)
}

func runExperimentMetadataFromContext(ctx context.Context) runExperimentMetadata {
	return experiment.RunMetadataFromContext(ctx)
}
