// Package results provides structured outcome recording for e2e tests.
//
// Every test that calls [e2e.NewTestContext] automatically records its outcome
// (pass/fail/skip) as a JSON line in e2e-results.jsonl. The JSONL file is
// uploaded as a GH Actions artifact and later ingested into Kusto (WI-03).
//
// Run-level metadata (commit SHA, PR number) is NOT duplicated here —
// it lives in CloudMine's WorkflowRun table and is joined via RunID at query time.
// Branch is the exception: stored directly to enable efficient filtering without
// cross-cluster joins (used by the flaky-test-notifier workflow).
package results

import "time"

// Status represents the outcome of a single test.
type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

// Outcome is a single test result. One Outcome is recorded per test function.
// Run-level metadata (commit, PR, URL) comes from CloudMine WorkflowRun via
// the RunID join key. Branch is stored directly to enable efficient filtering
// without cross-cluster joins (e.g. flaky-test-notifier workflow).
type Outcome struct {
	RunID       int64     `json:"run_id"`       // GH Actions run ID — join key to CloudMine WorkflowRun
	RunAttempt  int64     `json:"run_attempt"`  // GH Actions attempt number (1 for first run, increments on re-run); defaults to 1 for local runs
	TestName    string    `json:"test_name"`    // e.g. "TestGangSchedulingAdmitted"
	Suite       string    `json:"suite"`        // derived from directory: "kueue", "kuberay", "gpu-monitoring", "stack"
	Status      Status    `json:"status"`       // pass, fail, or skip
	DurationSec float64   `json:"duration_sec"` // wall-clock seconds
	Timestamp   time.Time `json:"timestamp"`    // UTC start time
	Branch      string    `json:"branch"`       // Source branch: PR head branch (GITHUB_HEAD_REF) or push target (GITHUB_REF_NAME); empty for local runs
}
