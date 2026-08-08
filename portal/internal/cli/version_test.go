// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"strings"
	"testing"
)

// core/version is shared with the tau CLI, so `taugrid-portal version` once
// printed "tau". These cover the seam rather than the formatting: they run the
// real command tree and assert on what a user sees.

func TestVersionCommandPrintsPortalBinaryName(t *testing.T) {
	out, _, err := executeExpCommand("version")
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	// Assert on the leading token, not Contains: "tau" is a prefix of
	// "taugrid-portal", so a substring check would also accept output that
	// still names the wrong binary.
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
