// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import "testing"

func TestResolveRunManagedWorkflowOptionsPreservesTTL(t *testing.T) {
	const ttlSecondsAfterFinished = int64(3600)

	input := unresolvedRunOptions{}
	input.ttlSecondsAfterFinished = ttlSecondsAfterFinished
	got := resolveRunManagedWorkflowOptions(input)
	if got.ttlSecondsAfterFinished != ttlSecondsAfterFinished {
		t.Fatalf("ttlSecondsAfterFinished = %d, want %d", got.ttlSecondsAfterFinished, ttlSecondsAfterFinished)
	}
}
