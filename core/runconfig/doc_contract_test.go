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

var retryDocs = []string{
	filepath.Join("skills", "taugrid", "references", "run-config.md"),
}

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

	if !checkedAnyLine {
		t.Fatalf("no retry env documentation found in %s; the guard matched nothing and proved nothing",
			strings.Join(retryDocs, ", "))
	}
}

func TestTauEnvAllowedIsSorted(t *testing.T) {
	if !slices.IsSorted(TauEnvAllowed) {
		t.Fatalf("TauEnvAllowed is not sorted: %v", TauEnvAllowed)
	}
}

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
