// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import "testing"

func TestRootSilencesCobraErrorPrinting(t *testing.T) {
	if !NewRoot().SilenceErrors {
		t.Fatal("root must set SilenceErrors so main() is the only error printer")
	}
}
