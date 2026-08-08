// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/status"
)

// A snapshot that succeeded but located neither workload kind: the run does not
// exist. Historically this fell through to `kubectl logs -l job-name=<name>`,
// which matches nothing for an unknown name and returns empty output with a nil
// error, so `tau run logs <typo>` exited 0 in silence.
func TestRunLogsReportsMissingRunInsteadOfSilentSuccess(t *testing.T) {
	var out bytes.Buffer
	jobLogsCalled := false

	err := runLogsCommandWithHooks(context.Background(), &out, nil, "typo-run-name", runLogsOptions{
		Namespace: "pre-training-document",
		Tail:      200,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			// Snapshot itself succeeds; it simply found nothing.
			return status.Snapshot{
				Name:      "typo-run-name",
				Namespace: "pre-training-document",
			}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", errors.New("not a rayjob")
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			// This is the empty-match path that produced the silent success:
			// `kubectl logs -l job-name=<unknown>` returns "" with a nil error.
			jobLogsCalled = true
			return "", nil
		},
	})

	if err == nil {
		t.Fatal("expected an error for a run that does not exist, got nil (silent success)")
	}
	if !jobLogsCalled {
		t.Error("the job-name path should still be attempted; the decision must come from its result, not from a prediction")
	}
	for _, want := range []string{"typo-run-name", "pre-training-document", "tau run list"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q so the user can act on it; got: %v", want, err)
		}
	}
	if out.Len() != 0 {
		t.Errorf("expected no stdout for a missing run, got %q", out.String())
	}
}

// Negative control for the guard above: when the snapshot DOES find a Job, the
// batch/v1 path must still run normally. Without this, a guard that fired too
// eagerly would break every plain Job run and the test above would still pass.
func TestRunLogsStillReadsJobLogsWhenJobExists(t *testing.T) {
	var out bytes.Buffer
	jobLogsCalled := false

	err := runLogsCommandWithHooks(context.Background(), &out, nil, "real-job", runLogsOptions{
		Namespace: "pre-training-document",
		Tail:      200,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "real-job",
				Namespace: "pre-training-document",
				JobFound:  true,
			}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", context.Canceled // force the fallthrough to jobLogs
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			jobLogsCalled = true
			return "real job output\n", nil
		},
	})
	if err != nil {
		t.Fatalf("expected success for an existing Job, got: %v", err)
	}
	if !jobLogsCalled {
		t.Error("jobLogs should have been consulted for an existing Job")
	}
	if !strings.Contains(out.String(), "real job output") {
		t.Errorf("expected job output on stdout, got %q", out.String())
	}
}
