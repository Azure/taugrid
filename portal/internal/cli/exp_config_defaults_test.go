// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"testing"
)

func TestRunExperimentContextPreventsChildDefaultRediscovery(t *testing.T) {
	selected := runExperimentMetadata{
		Workspace:  "sample",
		Project:    "selected-project",
		RunGroupID: "selected-group",
		Tags:       map[string]string{"source": "selected"},
	}
	directoryDefaults := runExperimentMetadata{
		Project:    "cwd-project",
		RunGroupID: "cwd-group",
		Tags:       map[string]string{"source": "cwd"},
	}

	ctx := withRunExperimentMetadata(context.Background(), selected)
	got := runExperimentMetadataFromContext(withRunExperimentDefaults(ctx, directoryDefaults))
	if got.Project != selected.Project || got.RunGroupID != selected.RunGroupID || got.Tags["source"] != "selected" {
		t.Fatalf("child defaults replaced selected run metadata: %#v", got)
	}

	ctx = withRunExperimentMetadata(context.Background(), runExperimentMetadata{})
	got = runExperimentMetadataFromContext(withRunExperimentDefaults(ctx, directoryDefaults))
	if !got.Empty() {
		t.Fatalf("explicit config without experiment metadata inherited cwd defaults: %#v", got)
	}
}
