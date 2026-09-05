// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package frontendtest runs dependency-free JavaScript regression tests from Go.
package frontendtest

import (
	"errors"
	"os"
	"os/exec"
	"testing"
)

// RunNode runs a Node.js test script relative to the calling package.
// CI and make test require Node; Go-only consumers may skip frontend tests.
func RunNode(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		if !errors.Is(err, exec.ErrNotFound) || os.Getenv("TAUGRID_REQUIRE_FRONTEND_TESTS") == "1" {
			t.Fatalf("frontend tests require Node.js 24 or newer: %v", err)
		}
		t.Skip("Node.js unavailable; install Node.js 24 or newer to run frontend tests")
	}
	cmd := exec.CommandContext(t.Context(), node, "--test", script)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("frontend tests (%s): %v\n%s", script, err, output)
	}
	t.Logf("frontend tests (%s):\n%s", script, output)
}
