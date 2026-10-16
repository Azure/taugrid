// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"strings"

	"github.com/Azure/taugrid/core/experiment"
)

type runExperimentMetadata = experiment.RunMetadata

func withRunExperimentMetadata(ctx context.Context, meta runExperimentMetadata) context.Context {
	return experiment.WithRunMetadata(ctx, meta)
}

func withRunExperimentDefaults(ctx context.Context, defaults runExperimentMetadata) context.Context {
	return experiment.WithRunMetadataDefaults(ctx, defaults)
}

func runExperimentMetadataFromContext(ctx context.Context) runExperimentMetadata {
	return experiment.RunMetadataFromContext(ctx)
}

func dash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
