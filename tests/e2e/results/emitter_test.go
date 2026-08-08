// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJSONLSink_WritesOneLinePerOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.jsonl")
	sink, err := NewJSONLSink(path)
	if err != nil {
		t.Fatalf("NewJSONLSink: %v", err)
	}

	outcomes := []Outcome{
		{TestName: "TestA", Suite: "kueue", Status: StatusPass, DurationSec: 1.5},
		{TestName: "TestB", Suite: "kuberay", Status: StatusFail, DurationSec: 0.3},
		{TestName: "TestC", Suite: "stack", Status: StatusSkip},
	}
	for _, o := range outcomes {
		if err := sink.Record(context.Background(), o); err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	if err := sink.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Read back and verify.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var decoded []Outcome
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var o Outcome
		if err := dec.Decode(&o); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		decoded = append(decoded, o)
	}

	if len(decoded) != 3 {
		t.Fatalf("expected 3 outcomes, got %d", len(decoded))
	}
	if decoded[0].TestName != "TestA" || decoded[0].Status != StatusPass {
		t.Errorf("outcome[0] = %+v", decoded[0])
	}
	if decoded[1].Status != StatusFail {
		t.Errorf("outcome[1].Status = %q, want %q", decoded[1].Status, StatusFail)
	}
	if decoded[2].Status != StatusSkip {
		t.Errorf("outcome[2].Status = %q, want %q", decoded[2].Status, StatusSkip)
	}
}

func TestMulti_FansOutToAll(t *testing.T) {
	path1 := filepath.Join(t.TempDir(), "a.jsonl")
	path2 := filepath.Join(t.TempDir(), "b.jsonl")

	s1, err := NewJSONLSink(path1)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewJSONLSink(path2)
	if err != nil {
		t.Fatal(err)
	}

	m := &Multi{Emitters: []ResultEmitter{s1, s2}}
	o := Outcome{TestName: "TestFanOut", Suite: "kueue", Status: StatusPass}
	if err := m.Record(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if err := m.Flush(); err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{path1, path2} {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("ReadFile(%s): %v", p, err)
		}
		if len(data) == 0 {
			t.Errorf("file %s is empty after Multi.Record", p)
		}
	}
}

func TestRunID_DefaultsToZero(t *testing.T) {
	// Clear both the GHA and ADO provenance vars so the result is deterministic
	// even when the test itself runs inside an ADO job (where BUILD_BUILDID is set).
	t.Setenv("GITHUB_RUN_ID", "")
	t.Setenv("BUILD_BUILDID", "")
	if id := RunID(); id != 0 {
		t.Errorf("RunID() = %d, want 0 when GITHUB_RUN_ID is unset", id)
	}
}

func TestRunID_ParsesEnvVar(t *testing.T) {
	t.Setenv("GITHUB_RUN_ID", "12345678")
	t.Setenv("BUILD_BUILDID", "99999") // GHA must win over ADO fallback
	if id := RunID(); id != 12345678 {
		t.Errorf("RunID() = %d, want 12345678", id)
	}
}

func TestRunAttempt_DefaultsToOneWhenUnset(t *testing.T) {
	// GitHub guarantees GITHUB_RUN_ATTEMPT on hosted runners, but we default to
	// 1 for local runs and old self-hosted images that may not set it.
	// Clear the ADO fallback too so the test is hermetic on an ADO agent.
	t.Setenv("GITHUB_RUN_ATTEMPT", "")
	t.Setenv("SYSTEM_JOBATTEMPT", "")
	if n := RunAttempt(); n != 1 {
		t.Errorf("RunAttempt() = %d, want 1 when GITHUB_RUN_ATTEMPT is unset", n)
	}
}

func TestRunAttempt_ParsesEnvVar(t *testing.T) {
	t.Setenv("GITHUB_RUN_ATTEMPT", "3")
	if n := RunAttempt(); n != 3 {
		t.Errorf("RunAttempt() = %d, want 3", n)
	}
}

func TestRunAttempt_DefaultsToOneOnGarbage(t *testing.T) {
	t.Setenv("GITHUB_RUN_ATTEMPT", "not-a-number")
	if n := RunAttempt(); n != 1 {
		t.Errorf("RunAttempt() = %d, want 1 on unparseable input", n)
	}
}

func TestRunAttempt_DefaultsToOneOnZeroOrNegative(t *testing.T) {
	t.Setenv("GITHUB_RUN_ATTEMPT", "0")
	if n := RunAttempt(); n != 1 {
		t.Errorf("RunAttempt() = %d, want 1 when env is 0", n)
	}
	t.Setenv("GITHUB_RUN_ATTEMPT", "-5")
	if n := RunAttempt(); n != 1 {
		t.Errorf("RunAttempt() = %d, want 1 when env is negative", n)
	}
}

func TestBranch_PullRequestUsesHeadRef(t *testing.T) {
	// On pull_request events, GITHUB_HEAD_REF is the PR head branch.
	// GITHUB_REF_NAME is "<pr_number>/merge" — should be ignored.
	t.Setenv("GITHUB_HEAD_REF", "contributor/fix-race")
	t.Setenv("GITHUB_REF_NAME", "123/merge")
	t.Setenv("SYSTEM_PULLREQUEST_SOURCEBRANCH", "")
	t.Setenv("BUILD_SOURCEBRANCHNAME", "")
	if b := Branch(); b != "contributor/fix-race" {
		t.Errorf("Branch() = %q, want %q", b, "contributor/fix-race")
	}
}

func TestBranch_PushUsesRefName(t *testing.T) {
	// On push events, GITHUB_HEAD_REF is empty.
	t.Setenv("GITHUB_HEAD_REF", "")
	t.Setenv("GITHUB_REF_NAME", "main")
	t.Setenv("SYSTEM_PULLREQUEST_SOURCEBRANCH", "")
	t.Setenv("BUILD_SOURCEBRANCHNAME", "")
	if b := Branch(); b != "main" {
		t.Errorf("Branch() = %q, want %q", b, "main")
	}
}

func TestBranch_LocalRunReturnsEmpty(t *testing.T) {
	// Clear GHA and ADO branch vars so "local run" is deterministic on any agent.
	t.Setenv("GITHUB_HEAD_REF", "")
	t.Setenv("GITHUB_REF_NAME", "")
	t.Setenv("SYSTEM_PULLREQUEST_SOURCEBRANCH", "")
	t.Setenv("BUILD_SOURCEBRANCHNAME", "")
	if b := Branch(); b != "" {
		t.Errorf("Branch() = %q, want empty string for local runs", b)
	}
}

// --- Azure DevOps provenance fallback coverage (added per review on PR #1184) ---

func TestRunID_FallsBackToBuildBuildID(t *testing.T) {
	// No GHA var, ADO var present → resolves BUILD_BUILDID.
	t.Setenv("GITHUB_RUN_ID", "")
	t.Setenv("BUILD_BUILDID", "174002598")
	if id := RunID(); id != 174002598 {
		t.Errorf("RunID() = %d, want 174002598 from BUILD_BUILDID fallback", id)
	}
}

func TestRunID_GitHubWinsOverADO(t *testing.T) {
	t.Setenv("GITHUB_RUN_ID", "555")
	t.Setenv("BUILD_BUILDID", "174002598")
	if id := RunID(); id != 555 {
		t.Errorf("RunID() = %d, want 555 (GITHUB_RUN_ID must take precedence)", id)
	}
}

func TestRunAttempt_FallsBackToSystemJobAttempt(t *testing.T) {
	t.Setenv("GITHUB_RUN_ATTEMPT", "")
	t.Setenv("SYSTEM_JOBATTEMPT", "3")
	if n := RunAttempt(); n != 3 {
		t.Errorf("RunAttempt() = %d, want 3 from SYSTEM_JOBATTEMPT fallback", n)
	}
}

func TestRunAttempt_GitHubWinsOverADO(t *testing.T) {
	t.Setenv("GITHUB_RUN_ATTEMPT", "2")
	t.Setenv("SYSTEM_JOBATTEMPT", "9")
	if n := RunAttempt(); n != 2 {
		t.Errorf("RunAttempt() = %d, want 2 (GITHUB_RUN_ATTEMPT must take precedence)", n)
	}
}

func TestBranch_FallsBackToADOPullRequestSourceBranch(t *testing.T) {
	// No GHA branch vars; ADO PR build → strip refs/heads/ prefix.
	t.Setenv("GITHUB_HEAD_REF", "")
	t.Setenv("GITHUB_REF_NAME", "")
	t.Setenv("SYSTEM_PULLREQUEST_SOURCEBRANCH", "refs/heads/contributor/fix-race")
	t.Setenv("BUILD_SOURCEBRANCHNAME", "should-be-ignored")
	if b := Branch(); b != "contributor/fix-race" {
		t.Errorf("Branch() = %q, want %q from ADO PR fallback", b, "contributor/fix-race")
	}
}

func TestBranch_FallsBackToADOBuildSourceBranchName(t *testing.T) {
	// No GHA branch vars, no ADO PR var → ADO branch/manual build.
	t.Setenv("GITHUB_HEAD_REF", "")
	t.Setenv("GITHUB_REF_NAME", "")
	t.Setenv("SYSTEM_PULLREQUEST_SOURCEBRANCH", "")
	t.Setenv("BUILD_SOURCEBRANCHNAME", "main")
	if b := Branch(); b != "main" {
		t.Errorf("Branch() = %q, want %q from BUILD_SOURCEBRANCHNAME fallback", b, "main")
	}
}

func TestBranch_GitHubWinsOverADO(t *testing.T) {
	t.Setenv("GITHUB_HEAD_REF", "contributor/gha-branch")
	t.Setenv("SYSTEM_PULLREQUEST_SOURCEBRANCH", "refs/heads/ado-branch")
	if b := Branch(); b != "contributor/gha-branch" {
		t.Errorf("Branch() = %q, want GHA head ref to win over ADO", b)
	}
}
