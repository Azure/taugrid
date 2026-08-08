// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/cli/internal/resume"
	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func preemptedSnapshot() status.Snapshot {
	return status.Snapshot{
		Name:      "test-job",
		Namespace: "ray",
		JobFound:  true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{
				Phase: "Failed",
				Conditions: []status.Condition{
					{Type: "DisruptionTarget", Status: "True"},
				},
			},
		},
	}
}

func oomSnapshot() status.Snapshot {
	return status.Snapshot{
		Name:      "test-job",
		Namespace: "ray",
		JobFound:  true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{Phase: "Failed", OOMKilled: true},
		},
	}
}

func successSnapshot() status.Snapshot {
	return status.Snapshot{
		Name:      "test-job",
		Namespace: "ray",
		JobFound:  true,
		JobConditions: []status.Condition{
			{Type: "Complete", Status: "True"},
		},
	}
}

func multiKueueRetryCleanupSnapshot(name string, workloadNames ...string) status.Snapshot {
	workloads := make([]status.Workload, 0, len(workloadNames))
	for _, workloadName := range workloadNames {
		workloads = append(workloads, status.Workload{
			Name:        workloadName,
			ClusterName: "worker-a",
		})
	}
	return status.Snapshot{
		Name:         name,
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: "kueue.x-k8s.io/multikueue",
		Workloads:    workloads,
	}
}

func baseRetryOpts() retryLoopOptions {
	return retryLoopOptions{
		name:           "test-job",
		namespace:      "ray",
		maxRetries:     2,
		retryOn:        []string{"Preempted", "Evicted"},
		backoffInitial: time.Millisecond,
		backoffMax:     10 * time.Millisecond,
	}
}

type recordedRetryEvent struct {
	attempt int
	reason  string
	message string
}

func TestRetryLoop_SuccessFirstAttempt(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return successSnapshot(), terminalSuccess, nil
		},
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if !strings.Contains(buf.String(), "completed successfully") {
		t.Fatalf("expected success message, got:\n%s", buf.String())
	}
}

func TestRetryLoop_RetryableFailureThenSuccess(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	call := 0
	resubmitCount := 0
	var events []recordedRetryEvent
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			call++
			if call == 1 {
				return preemptedSnapshot(), terminalFailed, nil
			}
			return successSnapshot(), terminalSuccess, nil
		},
		deleteWorkload: func() error { return nil },
		resubmit: func(attempt int, reason string) error {
			resubmitCount++
			if reason != "Preempted" {
				t.Errorf("expected Preempted reason, got %s", reason)
			}
			return nil
		},
		emitEvent: func(attempt int, reason string, message string) error {
			events = append(events, recordedRetryEvent{attempt: attempt, reason: reason, message: message})
			return nil
		},
		sleep: func(d time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err != nil {
		t.Fatalf("expected success after retry, got: %v", err)
	}
	if resubmitCount != 1 {
		t.Fatalf("expected 1 resubmit, got %d", resubmitCount)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 retry events, got %d", len(events))
	}
	if events[0].reason != retryEventReasonAttempt {
		t.Fatalf("expected first event reason %s, got %s", retryEventReasonAttempt, events[0].reason)
	}
	if !strings.Contains(events[0].message, "submission_id=test-job") || !strings.Contains(events[0].message, "attempt=1/2") || !strings.Contains(events[0].message, "failure=Preempted") {
		t.Fatalf("unexpected retry attempt event message: %s", events[0].message)
	}
	if events[1].reason != retryEventReasonSucceeded {
		t.Fatalf("expected success event reason %s, got %s", retryEventReasonSucceeded, events[1].reason)
	}
	if !strings.Contains(events[1].message, "recovered_after_attempt=1") || !strings.Contains(events[1].message, "last_failure=Preempted") {
		t.Fatalf("unexpected retry success event message: %s", events[1].message)
	}
}

func TestRetryLoop_NonRetryableFailure(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return oomSnapshot(), terminalFailed, nil
		},
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err == nil {
		t.Fatal("expected error for non-retryable failure")
	}
	if !strings.Contains(err.Error(), "not in retry_on") {
		t.Fatalf("expected 'not in retry_on' error, got: %v", err)
	}
}

func TestRetryLoop_MaxRetriesExhausted(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	opts.maxRetries = 2
	resubmitCount := 0
	var events []recordedRetryEvent
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return preemptedSnapshot(), terminalFailed, nil
		},
		deleteWorkload: func() error { return nil },
		resubmit: func(attempt int, reason string) error {
			resubmitCount++
			return nil
		},
		emitEvent: func(attempt int, reason string, message string) error {
			events = append(events, recordedRetryEvent{attempt: attempt, reason: reason, message: message})
			return nil
		},
		sleep: func(d time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err == nil {
		t.Fatal("expected exhaustion error")
	}
	if !strings.Contains(err.Error(), "exhausted all 2 retries") {
		t.Fatalf("expected exhaustion message, got: %v", err)
	}
	if resubmitCount != 2 {
		t.Fatalf("expected 2 resubmits before exhaustion, got %d", resubmitCount)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 retry events, got %d", len(events))
	}
	last := events[len(events)-1]
	if last.reason != retryEventReasonExhausted {
		t.Fatalf("expected final event reason %s, got %s", retryEventReasonExhausted, last.reason)
	}
	if !strings.Contains(last.message, "retries_exhausted=2") || !strings.Contains(last.message, "final_failure=Preempted") {
		t.Fatalf("unexpected exhausted event message: %s", last.message)
	}
}

func TestRetryLoop_FinalRetrySucceeds(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	opts.maxRetries = 2
	call := 0
	resubmitCount := 0
	var events []recordedRetryEvent
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			call++
			if call <= 2 {
				return preemptedSnapshot(), terminalFailed, nil
			}
			return successSnapshot(), terminalSuccess, nil
		},
		deleteWorkload: func() error { return nil },
		resubmit: func(attempt int, reason string) error {
			resubmitCount++
			return nil
		},
		emitEvent: func(attempt int, reason string, message string) error {
			events = append(events, recordedRetryEvent{attempt: attempt, reason: reason, message: message})
			return nil
		},
		sleep: func(d time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err != nil {
		t.Fatalf("expected success on final retry, got: %v", err)
	}
	if resubmitCount != 2 {
		t.Fatalf("expected exactly 2 resubmits, got %d", resubmitCount)
	}
	if len(events) != 3 {
		t.Fatalf("expected 3 retry events, got %d", len(events))
	}
	if events[2].reason != retryEventReasonSucceeded {
		t.Fatalf("expected final event reason %s, got %s", retryEventReasonSucceeded, events[2].reason)
	}
}

func TestRetryLoop_EmitEventFailureNonFatal(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	call := 0
	resubmitCount := 0
	eventCalls := 0
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			call++
			if call == 1 {
				return preemptedSnapshot(), terminalFailed, nil
			}
			return successSnapshot(), terminalSuccess, nil
		},
		deleteWorkload: func() error { return nil },
		resubmit: func(attempt int, reason string) error {
			resubmitCount++
			return nil
		},
		emitEvent: func(attempt int, reason string, message string) error {
			eventCalls++
			return errors.New("event create failed")
		},
		sleep: func(d time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err != nil {
		t.Fatalf("expected retry loop success, got: %v", err)
	}
	if resubmitCount != 1 {
		t.Fatalf("expected 1 resubmit, got %d", resubmitCount)
	}
	if eventCalls != 2 {
		t.Fatalf("expected 2 event emission attempts, got %d", eventCalls)
	}
	if !strings.Contains(buf.String(), "warning: failed to emit retry event") {
		t.Fatalf("expected warning in output, got:\n%s", buf.String())
	}
}

func TestRetryLoop_ContextCancelled(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return preemptedSnapshot(), terminalFailed, nil
		},
		sleep: func(d time.Duration) error {
			return context.Canceled
		},
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got: %v", err)
	}
}

func TestRetryLoop_DeleteFails(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return preemptedSnapshot(), terminalFailed, nil
		},
		deleteWorkload: func() error {
			return errors.New("kube delete failed")
		},
		sleep: func(d time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err == nil || !strings.Contains(err.Error(), "deleting workload") {
		t.Fatalf("expected delete error, got: %v", err)
	}
}

func TestRetryLoop_DeleteWaitsForMultiKueueCleanup(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	call := 0
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "test-job", "--ignore-not-found"): {{out: "rayjob.ray.io \"test-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "test-job", "--ignore-not-found"):           {{out: "job.batch \"test-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=test-job", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	fetchCalls := 0
	waitCalls := 0
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			call++
			if call == 1 {
				return preemptedSnapshot(), terminalFailed, nil
			}
			return successSnapshot(), terminalSuccess, nil
		},
		deleteWorkload: func() error {
			return deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "test-job", "ray", io.Discard, managerCleanupOptions{
				Timeout:  30 * time.Second,
				Interval: time.Second,
			}, managerCleanupHooks{
				fetchSnapshot: func(context.Context) (status.Snapshot, error) {
					fetchCalls++
					if fetchCalls == 1 {
						return multiKueueRetryCleanupSnapshot("test-job", "wl-a"), nil
					}
					return status.Snapshot{Name: "test-job", Namespace: "ray"}, nil
				},
				wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
				now:  time.Now,
			})
		},
		resubmit: func(int, string) error { return nil },
		sleep:    func(time.Duration) error { return nil },
	}
	if err := retryLoopWithHooks(&buf, opts, hooks); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("expected one exact-name wait plus one selector absence proof wait, got %d", waitCalls)
	}
	for _, call := range runner.joinedCalls() {
		if strings.Contains(call, "worker-a") || strings.Contains(call, "pods") || strings.Contains(call, "events") {
			t.Fatalf("retry cleanup should stay manager-side, got call %q", call)
		}
	}
}

func TestRetryLoop_DeleteGetsFullCleanupBudgetAfterDelete(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	call := 0
	base := time.Date(2026, 7, 10, 14, 30, 0, 0, time.UTC)
	current := base
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "test-job", "--ignore-not-found"): {{out: "rayjob.ray.io \"test-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "test-job", "--ignore-not-found"):           {{out: "job.batch \"test-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=test-job", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	fetchCalls := 0
	waitCalls := 0
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			call++
			if call == 1 {
				return preemptedSnapshot(), terminalFailed, nil
			}
			return successSnapshot(), terminalSuccess, nil
		},
		deleteWorkload: func() error {
			return deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "test-job", "ray", io.Discard, managerCleanupOptions{
				Timeout:  30 * time.Second,
				Interval: time.Second,
			}, managerCleanupHooks{
				fetchSnapshot: func(context.Context) (status.Snapshot, error) {
					fetchCalls++
					if fetchCalls == 1 {
						current = base.Add(31 * time.Second)
						return multiKueueRetryCleanupSnapshot("test-job", "wl-a"), nil
					}
					return status.Snapshot{Name: "test-job", Namespace: "ray"}, nil
				},
				wait: func(_ context.Context, d time.Duration) error {
					waitCalls++
					current = current.Add(d)
					return nil
				},
				now: func() time.Time { return current },
			})
		},
		resubmit: func(int, string) error { return nil },
		sleep:    func(time.Duration) error { return nil },
	}
	if err := retryLoopWithHooks(&buf, opts, hooks); err != nil {
		t.Fatalf("expected retry to succeed with full post-delete cleanup budget, got %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("expected one exact-name wait plus one selector absence proof wait after delete, got %d", waitCalls)
	}
}

func TestRetryLoop_DeleteNamespaceNotFoundPropagates(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return preemptedSnapshot(), terminalFailed, nil
		},
		deleteWorkload: func() error {
			return errors.New("Error from server (NotFound): namespaces \"ray\" not found")
		},
		sleep: func(d time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err == nil || !strings.Contains(err.Error(), "namespaces \"ray\" not found") {
		t.Fatalf("expected namespace not found delete error, got: %v", err)
	}
}

func TestRetryLoop_DeleteTimeoutPropagates(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	base := time.Date(2026, 7, 10, 14, 0, 0, 0, time.UTC)
	nowValues := []time.Time{
		base,
		base,
		base,
		base.Add(31 * time.Second),
	}
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return preemptedSnapshot(), terminalFailed, nil
		},
		deleteWorkload: func() error {
			runner := &scriptedRawRunner{
				steps: map[string][]scriptedRawResponse{
					fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "test-job", "--ignore-not-found"): {{out: "rayjob.ray.io \"test-job\" deleted\n"}},
					fakeRawKey("-n", "ray", "delete", "job", "test-job", "--ignore-not-found"):           {{out: "job.batch \"test-job\" deleted\n"}},
					fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):     {{out: `{"metadata":{"name":"wl-a"}}`}},
				},
			}
			return deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "test-job", "ray", io.Discard, managerCleanupOptions{
				Timeout:  30 * time.Second,
				Interval: time.Second,
			}, managerCleanupHooks{
				fetchSnapshot: func(context.Context) (status.Snapshot, error) {
					return multiKueueRetryCleanupSnapshot("test-job", "wl-a"), nil
				},
				wait: func(context.Context, time.Duration) error { return nil },
				now: func() time.Time {
					if len(nowValues) == 0 {
						return base.Add(time.Hour)
					}
					cur := nowValues[0]
					nowValues = nowValues[1:]
					return cur
				},
			})
		},
		sleep: func(time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err == nil || !strings.Contains(err.Error(), "timed out after 30s") {
		t.Fatalf("expected manager cleanup timeout, got %v", err)
	}
}

func TestRetryLoop_ResubmitFails(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return preemptedSnapshot(), terminalFailed, nil
		},
		deleteWorkload: func() error { return nil },
		resubmit: func(attempt int, reason string) error {
			return errors.New("submission failed")
		},
		sleep: func(d time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err == nil || !strings.Contains(err.Error(), "submitting retry") {
		t.Fatalf("expected submit error, got: %v", err)
	}
}

func TestRetryLoop_WaitForTerminalError(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return status.Snapshot{}, 0, errors.New("kube unreachable")
		},
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err == nil || !strings.Contains(err.Error(), "kube unreachable") {
		t.Fatalf("expected kube error, got: %v", err)
	}
}

func TestRetryLoop_AttemptCountInResubmit(t *testing.T) {
	var buf bytes.Buffer
	opts := baseRetryOpts()
	opts.maxRetries = 3
	call := 0
	var attempts []int
	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			call++
			if call <= 3 {
				return preemptedSnapshot(), terminalFailed, nil
			}
			return successSnapshot(), terminalSuccess, nil
		},
		deleteWorkload: func() error { return nil },
		resubmit: func(attempt int, reason string) error {
			attempts = append(attempts, attempt)
			return nil
		},
		sleep: func(d time.Duration) error { return nil },
	}
	err := retryLoopWithHooks(&buf, opts, hooks)
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if len(attempts) != 3 {
		t.Fatalf("expected 3 attempts, got %v", attempts)
	}
	for i, want := range []int{1, 2, 3} {
		if attempts[i] != want {
			t.Errorf("attempt[%d] = %d, want %d", i, attempts[i], want)
		}
	}
}

func TestRetryOnMatch(t *testing.T) {
	tests := []struct {
		name    string
		retryOn []string
		reason  resume.FailureReason
		want    bool
	}{
		{"preempted in list", []string{"Preempted", "Evicted"}, resume.ReasonPreempted, true},
		{"evicted in list", []string{"Preempted", "Evicted"}, resume.ReasonEvicted, true},
		{"oom not in list", []string{"Preempted", "Evicted"}, resume.ReasonOOMKilled, false},
		{"oom opt-in", []string{"OOMKilled"}, resume.ReasonOOMKilled, true},
		{"unknown not in list", []string{"Preempted"}, resume.ReasonUnknown, false},
		{"case insensitive", []string{"preempted"}, resume.ReasonPreempted, true},
		{"empty list", []string{}, resume.ReasonPreempted, false},
		{"completed never retries", []string{"Preempted", "Evicted", "OOMKilled"}, resume.ReasonCompleted, false},
		{"running never retries", []string{"Preempted", "Evicted", "OOMKilled"}, resume.ReasonRunning, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := retryOnMatch(tt.retryOn, tt.reason)
			if got != tt.want {
				t.Errorf("retryOnMatch(%v, %v) = %v, want %v", tt.retryOn, tt.reason, got, tt.want)
			}
		})
	}
}

func TestRetryBackoff(t *testing.T) {
	initial := 30 * time.Second
	max := 5 * time.Minute

	tests := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 30 * time.Second},
		{4, 240 * time.Second},
		{5, 5 * time.Minute},
		{10, 5 * time.Minute},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("attempt_%d", tt.attempt), func(t *testing.T) {
			got := retryBackoff(initial, max, tt.attempt)
			if got != tt.want {
				t.Errorf("retryBackoff(30s, 5m, %d) = %v, want %v", tt.attempt, got, tt.want)
			}
		})
	}
}

func TestAppendRetryEnv(t *testing.T) {
	base := []string{"FOO=bar", "BAZ=qux"}
	got := appendRetryEnv(base, "/data/checkpoints/finetunes/test-job", 2, 3, "Preempted")

	want := map[string]bool{
		"FOO=bar": true,
		"BAZ=qux": true,
		"TAU_RESUME_FROM=/data/checkpoints/finetunes/test-job": true,
		"TAU_RETRY_ATTEMPT=2":        true,
		"TAU_RETRY_MAX=3":            true,
		"TAU_RETRY_REASON=Preempted": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d env vars, want %d: %v", len(got), len(want), got)
	}
	for _, v := range got {
		if !want[v] {
			t.Errorf("unexpected env var: %s", v)
		}
	}
}

func TestAppendRetryEnvDedup(t *testing.T) {
	base := []string{"TAU_RESUME_FROM=/old/path", "TAU_RETRY_ATTEMPT=1", "OTHER=keep"}
	got := appendRetryEnv(base, "/new/path", 2, 3, "Evicted")

	resumeCount := 0
	attemptCount := 0
	for _, v := range got {
		if v == "TAU_RESUME_FROM=/old/path" {
			t.Error("old TAU_RESUME_FROM should be replaced")
		}
		if v == "TAU_RESUME_FROM=/new/path" {
			resumeCount++
		}
		if v == "TAU_RETRY_ATTEMPT=2" {
			attemptCount++
		}
	}
	if resumeCount != 1 {
		t.Errorf("expected exactly 1 TAU_RESUME_FROM, got %d in %v", resumeCount, got)
	}
	if attemptCount != 1 {
		t.Errorf("expected exactly 1 TAU_RETRY_ATTEMPT, got %d in %v", attemptCount, got)
	}
}
