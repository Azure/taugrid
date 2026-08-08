// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import "testing"

// TestRootSilencesCobraErrorPrinting asserts the root does not let cobra print
// the error itself. main() prints `error: %v` and exits 1; without
// SilenceErrors cobra prints its own `Error: %v` first and every failure is
// reported twice.
func TestRootSilencesCobraErrorPrinting(t *testing.T) {
	if !NewRoot().SilenceErrors {
		t.Fatal("root must set SilenceErrors so main() is the only error printer")
	}
}
