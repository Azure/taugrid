// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

const defaultResultFile = "e2e-results.jsonl"

var (
	globalOnce    sync.Once
	globalEmitter ResultEmitter
	globalErr     error
)

// Init initialises the process-wide emitter (idempotent via sync.Once).
// Call this early — NewTestContext calls it automatically.
func Init() (ResultEmitter, error) {
	globalOnce.Do(func() {
		path := os.Getenv("E2E_RESULT_FILE")
		if path == "" {
			path = defaultResultFile
		}

		jsonl, err := NewJSONLSink(path)
		if err != nil {
			globalErr = err
			return
		}

		emitters := []ResultEmitter{jsonl}

		// Kusto sink (opt-in via E2E_KUSTO_URI).
		if uri := os.Getenv("E2E_KUSTO_URI"); uri != "" {
			db := os.Getenv("E2E_KUSTO_DB")
			if db == "" {
				db = "CITests"
			}
			// Provenance guard: Kusto is enabled and we're in CI, but no run ID
			// resolved (RunID()==0). Rows would all collide under RunID=0 and
			// dashboard per-run rollups would be meaningless. Warn loudly — this
			// is the trap that degraded TestOutcomes ingestion for days after the
			// GHA->ADO migration. Non-fatal: telemetry must never block tests.
			if RunID() == 0 && inCI() {
				fmt.Fprintln(os.Stderr, "results: WARNING — Kusto sink enabled in CI but no run ID resolved (GITHUB_RUN_ID / BUILD_BUILDID both unset); TestOutcomes rows will collide under RunID=0. Wire the run-id env var.")
			}
			ks, err := NewKustoSink(uri, db)
			if err != nil {
				// Log and continue without Kusto — don't block tests.
				fmt.Fprintf(os.Stderr, "results: kusto sink init failed (continuing without): %v\n", err)
			} else {
				emitters = append(emitters, ks)
			}
		}

		globalEmitter = &Multi{Emitters: emitters}
	})
	return globalEmitter, globalErr
}

// FlushAll flushes the global emitter. Call in TestMain before os.Exit.
// Safe to call if Init was never called (no-op).
func FlushAll() {
	if globalEmitter != nil {
		if err := globalEmitter.Flush(); err != nil {
			fmt.Fprintf(os.Stderr, "results: flush error: %v\n", err)
		}
	}
}

// RunID returns the CI run ID, resolved in a CI-system-agnostic way:
// GITHUB_RUN_ID (GitHub Actions), else BUILD_BUILDID (Azure DevOps, set as
// $(Build.BuildId)). Returns 0 for local runs (neither set).
func RunID() int64 {
	s := os.Getenv("GITHUB_RUN_ID")
	if s == "" {
		s = os.Getenv("BUILD_BUILDID")
	}
	if s == "" {
		return 0
	}
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return id
}

// RunAttempt returns the CI run attempt, resolved CI-system-agnostically:
// GITHUB_RUN_ATTEMPT (GitHub Actions), else SYSTEM_JOBATTEMPT (Azure DevOps,
// set as $(System.JobAttempt)). Re-runs share the same RunID and only increment
// the attempt — without this, percentile and pass-rate queries would double-count.
// Defaults to 1 when unset (local runs, or runners that don't set it).
func RunAttempt() int64 {
	s := os.Getenv("GITHUB_RUN_ATTEMPT")
	if s == "" {
		s = os.Getenv("SYSTEM_JOBATTEMPT")
	}
	if s == "" {
		return 1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// Branch returns the source branch name for the current CI run, resolved
// CI-system-agnostically.
//
// GitHub Actions:
//   - pull_request events: GITHUB_HEAD_REF (the PR head branch, e.g. "contributor/fix-race").
//   - push/workflow_dispatch: GITHUB_REF_NAME (e.g. "main").
//
// Azure DevOps (fallbacks, refs/heads/ prefix stripped):
//   - PR builds: SYSTEM_PULLREQUEST_SOURCEBRANCH.
//   - branch/manual builds: BUILD_SOURCEBRANCHNAME (already unqualified, e.g. "main").
//
// Returns "" for local runs.
func Branch() string {
	// GITHUB_HEAD_REF is only set for pull_request events and contains the
	// actual head branch name. GITHUB_REF_NAME on pull_request events is
	// "<pr_number>/merge" which is not useful for filtering.
	if head := os.Getenv("GITHUB_HEAD_REF"); head != "" {
		return head
	}
	if ref := os.Getenv("GITHUB_REF_NAME"); ref != "" {
		return ref
	}
	// Azure DevOps fallbacks.
	if adoPR := os.Getenv("SYSTEM_PULLREQUEST_SOURCEBRANCH"); adoPR != "" {
		return strings.TrimPrefix(adoPR, "refs/heads/")
	}
	return os.Getenv("BUILD_SOURCEBRANCHNAME")
}

// inCI reports whether we're running inside a recognised CI system. Both GitHub
// Actions (GITHUB_ACTIONS=true) and Azure DevOps (TF_BUILD=True) set a generic
// CI=true as well. Used to distinguish "local run, no telemetry expected" from
// "in CI but provenance env is misconfigured".
func inCI() bool {
	return os.Getenv("GITHUB_ACTIONS") == "true" ||
		os.Getenv("TF_BUILD") == "True" ||
		os.Getenv("CI") == "true"
}
