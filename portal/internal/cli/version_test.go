// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"strings"
	"testing"
)

func TestVersionCommandPrintsPortalBinaryName(t *testing.T) {
	out, _, err := executeExpCommand("version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	fields := strings.Fields(out)
	if len(fields) == 0 {
		t.Fatalf("version printed nothing")
	}
	if fields[0] != "taugrid-portal" {
		t.Errorf("version identifies binary as %q, want taugrid-portal (full output: %q)", fields[0], out)
	}
}

func TestVersionShortOmitsBinaryName(t *testing.T) {
	out, _, err := executeExpCommand("version", "--short")
	if err != nil {
		t.Fatalf("version --short: %v", err)
	}
	if got := strings.TrimSpace(out); strings.ContainsAny(got, " \t") {
		t.Errorf("version --short = %q, want a bare version string", got)
	}
}
