// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runhistory

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"
)

// A failed batch Job's own condition only ever carries "BackoffLimitExceeded",
// which says nothing about why the run died. The real cause lives on the pods,
// and Kubernetes deletes those pods along with the Job once
// ttlSecondsAfterFinished elapses — so unless the recorder folds pod evidence
// into the durable lifecycle row before that happens, the failure becomes
// permanently undiagnosable. These tests pin that enrichment.

func failedJobSource(pods []Pod) *fakeSource {
	metadata := testMetadata("train")
	return &fakeSource{
		jobs: []Job{{
			Metadata:   metadata,
			Failed:     1,
			Conditions: []Condition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}},
		}},
		pods: pods,
	}
}

func jobPod(name string, containers ...ContainerState) Pod {
	metadata := testMetadata(name)
	metadata.OwnerKind = "Job"
	metadata.OwnerName = "train"
	return Pod{Metadata: metadata, Phase: "Failed", Containers: containers}
}

func terminalRecord(t *testing.T, source *fakeSource) Record {
	t.Helper()
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if len(writer.records) == 0 {
		t.Fatal("no records written")
	}
	return writer.records[len(writer.records)-1]
}

func TestFailedJobRecordsPodExitCode(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{
		jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 1, Reason: "Error"}),
	}))

	if record.State != StateFailed {
		t.Fatalf("state = %q, want %q", record.State, StateFailed)
	}
	// The Job's own reason is kept so nothing that used to be recorded is lost.
	if !strings.Contains(record.Reason, "BackoffLimitExceeded") {
		t.Errorf("reason dropped the Job condition: %q", record.Reason)
	}
	if !strings.Contains(record.Message, "train-abc") || !strings.Contains(record.Message, "trainer") {
		t.Errorf("message did not identify the failing pod/container: %q", record.Message)
	}
	if !strings.Contains(record.Message, "exit code 1") {
		t.Errorf("message did not record the exit code: %q", record.Message)
	}
}

func TestFailedJobRecordsOOMKill(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{
		jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 137, Reason: "OOMKilled", OOMKilled: true}),
	}))

	if !strings.Contains(record.Reason, "OOMKilled") {
		t.Errorf("reason did not surface the OOM kill: %q", record.Reason)
	}
	if !strings.Contains(record.Message, "exit code 137") {
		t.Errorf("message did not record the exit code: %q", record.Message)
	}
}

// A container stuck waiting never terminates, so there is no exit code — but
// the waiting reason is exactly the diagnosis (bad image, bad config).
func TestFailedJobRecordsWaitingReason(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{
		jobPod("train-abc", ContainerState{Name: "trainer", Reason: "ImagePullBackOff", Message: "manifest unknown"}),
	}))

	if !strings.Contains(record.Reason, "ImagePullBackOff") {
		t.Errorf("reason did not surface the waiting reason: %q", record.Reason)
	}
	if strings.Contains(record.Message, "exit code") {
		t.Errorf("a container that never terminated must not report an exit code: %q", record.Message)
	}
	if !strings.Contains(record.Message, "manifest unknown") {
		t.Errorf("message dropped the container detail: %q", record.Message)
	}
}

// A multi-pod Job must not write an unbounded row: one cause plus a count.
func TestFailedJobSummaryIsBounded(t *testing.T) {
	pods := []Pod{
		jobPod("train-a", ContainerState{Name: "trainer", Terminated: true, ExitCode: 2, Reason: "Error", Message: strings.Repeat("x", 4000)}),
		jobPod("train-b", ContainerState{Name: "trainer", Terminated: true, ExitCode: 3, Reason: "Error"}),
		jobPod("train-c", ContainerState{Name: "trainer", Terminated: true, ExitCode: 4, Reason: "Error"}),
	}
	record := terminalRecord(t, failedJobSource(pods))

	if len(record.Message) > maxFailureMessageLength {
		t.Fatalf("message length %d exceeds cap %d", len(record.Message), maxFailureMessageLength)
	}
	if !strings.Contains(record.Message, "train-a") {
		t.Errorf("summary did not report the first failing pod: %q", record.Message)
	}
}

func TestFailedJobCountsAdditionalFailures(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{
		jobPod("train-a", ContainerState{Name: "trainer", Terminated: true, ExitCode: 2, Reason: "Error"}),
		jobPod("train-b", ContainerState{Name: "trainer", Terminated: true, ExitCode: 3, Reason: "Error"}),
	}))

	if !strings.Contains(record.Message, "+1 more failed container") {
		t.Errorf("summary did not count the other failures: %q", record.Message)
	}
}

// Succeeded containers are not failures; a Job that failed for another reason
// must not be described by a container that exited 0.
func TestSucceededContainersAreNotTreatedAsFailures(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{
		jobPod("train-abc", ContainerState{Name: "sidecar", Terminated: true, ExitCode: 0, Reason: "Completed"}),
	}))

	if record.Reason != "BackoffLimitExceeded" {
		t.Errorf("reason was enriched from a successful container: %q", record.Reason)
	}
}

// Pods belonging to a different Job must never be attributed to this one.
func TestPodCorrelationIgnoresForeignPods(t *testing.T) {
	foreign := jobPod("other-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 9, Reason: "Error"})
	foreign.OwnerName = "other"
	record := terminalRecord(t, failedJobSource([]Pod{foreign}))

	if strings.Contains(record.Message, "other-abc") {
		t.Errorf("a foreign pod was attributed to this Job: %q", record.Message)
	}
	if record.Reason != "BackoffLimitExceeded" {
		t.Errorf("reason = %q, want the unenriched Job condition", record.Reason)
	}
}

// Correlation must also work off the Job-name label, since that is what older
// clusters set and what survives an ownerReference rewrite.
func TestPodCorrelationMatchesJobNameLabel(t *testing.T) {
	pod := jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 5, Reason: "Error"})
	pod.OwnerKind = ""
	pod.OwnerName = ""
	pod.Labels["batch.kubernetes.io/job-name"] = "train"
	record := terminalRecord(t, failedJobSource([]Pod{pod}))

	if !strings.Contains(record.Message, "exit code 5") {
		t.Errorf("label-correlated pod was not used: %q", record.Message)
	}
}

// The lifecycle row is the durable artifact. Losing pod reads must degrade the
// summary, never the record.
func TestPodListFailureDegradesGracefully(t *testing.T) {
	source := failedJobSource(nil)
	source.podErr = context.DeadlineExceeded

	writer := &fakeWriter{}
	result, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray")
	if err != nil {
		t.Fatalf("pod read failure must not fail the pass: %v", err)
	}
	if result.PodsStatus != "unavailable" {
		t.Errorf("PodsStatus = %q, want unavailable", result.PodsStatus)
	}
	if record := writer.records[len(writer.records)-1]; record.State != StateFailed || record.Reason != "BackoffLimitExceeded" {
		t.Errorf("record = %+v, want the unenriched terminal record", record)
	}
}

// The recorder de-duplicates by fingerprint, so a summary that churns between
// passes would re-ingest forever.
func TestFailureSummaryIsStableAcrossPasses(t *testing.T) {
	pods := []Pod{
		jobPod("train-b", ContainerState{Name: "trainer", Terminated: true, ExitCode: 3, Reason: "Error"}),
		jobPod("train-a", ContainerState{Name: "trainer", Terminated: true, ExitCode: 2, Reason: "Error"}),
	}
	first := terminalRecord(t, failedJobSource(pods))

	// Same pods, reported in the opposite order by the API server.
	reordered := []Pod{pods[1], pods[0]}
	second := terminalRecord(t, failedJobSource(reordered))

	if first.Message != second.Message || first.Reason != second.Reason {
		t.Errorf("summary changed with pod ordering:\n first=%q/%q\nsecond=%q/%q",
			first.Reason, first.Message, second.Reason, second.Message)
	}
}

// Container messages are arbitrary user text. A byte-wise truncation would cut
// a multi-byte rune in half and write invalid UTF-8 into the durable record.
func TestFailureSummaryTruncationPreservesValidUTF8(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{
		jobPod("train-abc", ContainerState{
			Name:       "trainer",
			Terminated: true,
			ExitCode:   1,
			Reason:     "Error",
			Message:    strings.Repeat("→", 4000),
		}),
	}))

	if len(record.Message) > maxFailureMessageLength {
		t.Fatalf("message length %d exceeds cap %d", len(record.Message), maxFailureMessageLength)
	}
	if !utf8.ValidString(record.Message) {
		t.Errorf("truncation produced invalid UTF-8: %q", record.Message)
	}
}
