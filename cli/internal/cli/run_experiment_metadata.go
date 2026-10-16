// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"

	"github.com/Azure/taugrid/core/experiment"
)

type runExperimentMetadata = experiment.RunMetadata

func withRunExperimentMetadata(ctx context.Context, meta runExperimentMetadata) context.Context {
	return experiment.WithRunMetadata(ctx, meta)
}

func runExperimentMetadataFromContext(ctx context.Context) runExperimentMetadata {
	return experiment.RunMetadataFromContext(ctx)
}
