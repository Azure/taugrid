// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The retry env contract is the one place a doc typo fails inside the
// researcher's own training code rather than at submit time -- and only on a
// retry, which is already the unhappy path. `TAU_MAX_RETRIES` was documented in
// two places for long enough to be copied into scripts; the injected name is
// `TAU_RETRY_MAX`, so those scripts got a KeyError that reads like a platform
// bug. Three of the four names being right is what made it convincing.
//
// This guard walks the retry sections of the docs that describe that contract
// and asserts every TAU_ name in them is one Tau actually injects.

// retryDocs are the files whose retry sections describe the injected env
// contract, relative to the repo root.
var retryDocs = []string{
	filepath.Join("skills", "taugrid", "references", "run-config.md"),
}

// retryDocLine matches a documentation line that enumerates injected retry env
// vars. Scoping by line rather than by whole file keeps the guard from tripping
// over the many other TAU_ names these docs legitimately mention (TAU_CONTEXT,
// TAU_PROFILE_MODE, ...) which are not part of this contract.
var retryDocLine = regexp.MustCompile(`TAU_(RESUME_FROM|RETRY_[A-Z]+|MAX_RETRIES)`)

var tauEnvToken = regexp.MustCompile(`\bTAU_[A-Z0-9_]+`)

func TestRetryDocsNameOnlyInjectedEnvVars(t *testing.T) {
	root := repoRoot(t)
	checkedAnyLine := false

	for _, doc := range retryDocs {
		path := filepath.Join(root, doc)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", doc, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !retryDocLine.MatchString(line) {
				continue
			}
			checkedAnyLine = true
			for _, token := range tauEnvToken.FindAllString(line, -1) {
				if !slices.Contains(TauEnvAllowed, token) {
					t.Errorf("%s:%d names %q, which Tau never injects; the retry contract is %s",
						doc, i+1, token, strings.Join(TauEnvAllowed, ", "))
				}
			}
		}
	}

	// A doc reorganization that moves these sections would otherwise silently
	// turn this guard into a no-op that still reports success.
	if !checkedAnyLine {
		t.Fatalf("no retry env documentation found in %s; the guard matched nothing and proved nothing",
			strings.Join(retryDocs, ", "))
	}
}

// TestTauEnvAllowedIsSorted keeps the set in a stable order: it is joined
// verbatim into the rejection message, so an unordered append would reword
// every error for no reason.
func TestTauEnvAllowedIsSorted(t *testing.T) {
	if !slices.IsSorted(TauEnvAllowed) {
		t.Fatalf("TauEnvAllowed is not sorted: %v", TauEnvAllowed)
	}
}

// repoRoot walks up from the module root, matching the pattern in
// core/workloadmeta/contract_test.go. The docs under test live outside this
// module, so a relative path from the package directory would not reach them.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Dir(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate module root")
		}
		dir = parent
	}
}
