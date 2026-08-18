// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runhistory

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Azure/taugrid/core/workloadmeta"
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
	pod := jobPod("train-abc", ContainerState{Name: "sidecar", Terminated: true, ExitCode: 0, Reason: "Completed"})
	// A pod whose only container exited 0 reports phase Succeeded. Leaving the
	// fixture's Failed phase here would assert against a state Kubernetes does
	// not produce, and would mask the pod-level fallback rather than test it.
	pod.Phase = "Succeeded"
	record := terminalRecord(t, failedJobSource([]Pod{pod}))

	if record.Reason != "BackoffLimitExceeded" {
		t.Errorf("reason was enriched from a successful container: %q", record.Reason)
	}
}

// A pod can fail with no container status at all — evicted, or rejected by
// kubelet admission. The pod-level reason is then the only evidence, and
// dropping it leaves the durable record at "BackoffLimitExceeded" despite the
// cause being known and already captured by ListPods.
func TestFailedJobRecordsPodLevelReasonWhenNoContainerFailed(t *testing.T) {
	pod := jobPod("train-abc")
	pod.Reason = "Evicted"
	record := terminalRecord(t, failedJobSource([]Pod{pod}))

	if !strings.Contains(record.Reason, "Evicted") {
		t.Errorf("reason did not surface the pod-level cause: %q", record.Reason)
	}
	if !strings.Contains(record.Message, "train-abc") {
		t.Errorf("message did not identify the pod: %q", record.Message)
	}
	if strings.Contains(record.Message, "container") {
		t.Errorf("no container failed; message should not claim one: %q", record.Message)
	}
}

// A pod with no reason at all still carries its phase, which is weak evidence
// but strictly better than restating the Job condition.
func TestFailedPodWithoutReasonStillRecorded(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{jobPod("train-abc")}))

	if !strings.Contains(record.Reason, "PodFailed") {
		t.Errorf("reason = %q, want the pod-level fallback", record.Reason)
	}
}

// Evidence strength, not pod name, must decide what the summary reports. A pod
// sorting first alphabetically while merely waiting must not mask an OOM kill
// on a later-named pod: only the strongest candidate is summarised, and the
// pods are deleted with the Job.
func TestStrongestEvidenceWinsOverLexicalOrder(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{
		jobPod("train-aaa", ContainerState{Name: "trainer", Reason: "ImagePullBackOff"}),
		jobPod("train-zzz", ContainerState{Name: "trainer", Terminated: true, ExitCode: 137, Reason: "OOMKilled", OOMKilled: true}),
	}))

	if !strings.Contains(record.Reason, "OOMKilled") {
		t.Errorf("reason = %q, want the OOM kill to win over the waiting pod", record.Reason)
	}
	if !strings.Contains(record.Message, "train-zzz") {
		t.Errorf("message = %q, want the OOM-killed pod", record.Message)
	}
}

// A non-zero exit outranks a container that terminated cleanly with a reason.
func TestNonZeroExitOutranksCleanTermination(t *testing.T) {
	record := terminalRecord(t, failedJobSource([]Pod{
		jobPod("train-aaa", ContainerState{Name: "trainer", Terminated: true, ExitCode: 0, Reason: "Completed"}),
		jobPod("train-zzz", ContainerState{Name: "trainer", Terminated: true, ExitCode: 9, Reason: "Error"}),
	}))

	if !strings.Contains(record.Message, "exit code 9") {
		t.Errorf("message = %q, want the non-zero exit", record.Message)
	}
}

// Container-level evidence outranks a pod-level reason on another pod.
func TestContainerEvidenceOutranksPodLevelReason(t *testing.T) {
	evicted := jobPod("train-aaa")
	evicted.Reason = "Evicted"
	record := terminalRecord(t, failedJobSource([]Pod{
		evicted,
		jobPod("train-zzz", ContainerState{Name: "trainer", Terminated: true, ExitCode: 3, Reason: "Error"}),
	}))

	if !strings.Contains(record.Message, "exit code 3") {
		t.Errorf("message = %q, want the container exit over the pod-level reason", record.Message)
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

// controller_version is a durable audit column in TauExpRunLifecycle. It used
// to fall back to app.kubernetes.io/version, which nothing in TauGrid writes
// and which researchers set to their own git SHA — so the column silently
// recorded a plausible-looking value that was not a TauGrid version. Empty is
// the only honest answer until tau stamps its own version (Azure/taugrid#113).
func TestControllerVersionNeverBorrowsResearcherLabel(t *testing.T) {
	metadata := testMetadata("train")
	metadata.Annotations["app.kubernetes.io/version"] = "87703b5"

	source := &fakeSource{jobs: []Job{{Metadata: metadata, Active: 1}}}
	record := terminalRecord(t, source)

	if record.ControllerVersion != "" {
		t.Errorf("ControllerVersion = %q, want empty; a researcher label must never populate the audit column", record.ControllerVersion)
	}
}

// The tau-owned annotation is still the real source, once something writes it.
func TestControllerVersionUsesTauAnnotation(t *testing.T) {
	metadata := testMetadata("train")
	metadata.Annotations[workloadmeta.AnnotationControllerVerion] = "v0.3.0"
	metadata.Annotations["app.kubernetes.io/version"] = "87703b5"

	source := &fakeSource{jobs: []Job{{Metadata: metadata, Active: 1}}}
	record := terminalRecord(t, source)

	if record.ControllerVersion != "v0.3.0" {
		t.Errorf("ControllerVersion = %q, want v0.3.0", record.ControllerVersion)
	}
}

// A transient pod-read failure must never overwrite evidence already recorded.
//
// Before this guard, a second pass without pod visibility re-emitted the same
// terminal failure with the degraded "BackoffLimitExceeded" text. Both rows
// carry an identical observed_at — it derives from the Job's terminal
// condition, which does not move between passes — and the dashboards collapse
// with arg_max(observed_at, *), whose tie-break on equal values is arbitrary.
// The degraded row could therefore win and hide the exit code, which is the
// exact evidence loss the failure summary exists to prevent.
func TestTransientPodFailureDoesNotOverwriteRecordedEvidence(t *testing.T) {
	source := failedJobSource([]Pod{
		jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 42, Reason: "Error"}),
	})
	writer := &fakeWriter{}
	reconciler := newTestReconciler(source, writer)

	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	enriched := writer.records[len(writer.records)-1]
	if !strings.Contains(enriched.Message, "exit code 42") {
		t.Fatalf("first pass did not enrich: %q", enriched.Message)
	}
	before := len(writer.records)

	// Pod visibility is lost; the Job is unchanged and still terminal.
	source.podErr = context.DeadlineExceeded
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	for _, record := range writer.records[before:] {
		if record.State == StateFailed {
			t.Errorf("degraded failure record re-emitted after losing pod visibility: %q / %q", record.Reason, record.Message)
		}
	}
}

// The guard must not suppress a first observation: if the failure was never
// recorded, a degraded record is better than no record at all.
func TestFirstFailureIsRecordedEvenWithoutPods(t *testing.T) {
	source := failedJobSource(nil)
	source.podErr = context.DeadlineExceeded

	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	var failed bool
	for _, record := range writer.records {
		if record.State == StateFailed {
			failed = true
		}
	}
	if !failed {
		t.Error("no terminal failure recorded; a degraded record is better than none")
	}
}

// Once pod reads recover, a genuinely richer record is still allowed through.
func TestRecoveredPodsStillEnrichAnUnrecordedFailure(t *testing.T) {
	source := failedJobSource(nil)
	source.podErr = context.DeadlineExceeded
	writer := &fakeWriter{}
	reconciler := newTestReconciler(source, writer)

	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	before := len(writer.records)

	source.podErr = nil
	source.pods = []Pod{jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 7, Reason: "Error"})}
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	var enriched bool
	for _, record := range writer.records[before:] {
		if strings.Contains(record.Message, "exit code 7") {
			enriched = true
		}
	}
	if !enriched {
		t.Error("recovered pod reads did not produce an enriched record")
	}
}

// The rollout sequence: a recorder deployed without the pods read verb records
// degraded failures, then the Role is fixed and enriched records begin. Both
// describe the same lifecycle event, so both would otherwise carry the same
// observed_at, and arg_max(observed_at, *) would pick between them arbitrarily
// — meaning the fix could appear not to have worked.
//
// The superseding observation must be strictly newer so the real cause wins.
func TestEnrichedFailureSupersedesAnEarlierDegradedOne(t *testing.T) {
	source := failedJobSource(nil)
	source.podErr = context.DeadlineExceeded
	writer := &fakeWriter{}
	reconciler := newTestReconciler(source, writer)

	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	source.podErr = nil
	source.pods = []Pod{jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 7, Reason: "Error"})}
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	var failed []Record
	for _, record := range writer.records {
		if record.State == StateFailed {
			failed = append(failed, record)
		}
	}
	if len(failed) != 2 {
		t.Fatalf("expected a degraded then an enriched record, got %d", len(failed))
	}
	degraded, enriched := failed[0], failed[1]
	if !strings.Contains(enriched.Message, "exit code 7") {
		t.Fatalf("second record was not the enriched one: %q", enriched.Message)
	}
	if !enriched.ObservedAt.After(degraded.ObservedAt) {
		t.Errorf("enriched observed_at %s is not strictly after degraded %s; arg_max could pick the degraded row",
			enriched.ObservedAt, degraded.ObservedAt)
	}
}

// A first observation must keep lifecycle-event semantics: its observed_at is
// when the failure happened, not when the recorder noticed.
func TestFirstFailureKeepsLifecycleEventTime(t *testing.T) {
	source := failedJobSource([]Pod{
		jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 3, Reason: "Error"}),
	})
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	for _, record := range writer.records {
		if record.State != StateFailed {
			continue
		}
		// newTestReconciler pins Now to 10:01; the fixture's lifecycle time is 10:00.
		if record.ObservedAt.Equal(time.Date(2026, 7, 16, 10, 1, 0, 0, time.UTC)) {
			t.Errorf("first observation used poll time %s instead of the lifecycle event time", record.ObservedAt)
		}
	}
}

// Job names are reused. With background deletion an old pod can outlive its
// Job while an identically named Job is created immediately after, and
// name-only correlation would persist the dead Job's failure under the new
// Job's identity — a durable record that is not merely weak but wrong.
func TestPodFromAnEarlierSameNameJobIsNotAdopted(t *testing.T) {
	stale := jobPod("train-old", ContainerState{Name: "trainer", Terminated: true, ExitCode: 99, Reason: "Error"})
	stale.OwnerUID = "uid-previous-job"

	source := failedJobSource([]Pod{stale})
	// The live Job carries its own identity; testMetadata sets UID uid-train.
	record := terminalRecord(t, source)

	if strings.Contains(record.Message, "exit code 99") {
		t.Errorf("a pod from a previous Job instance was adopted: %q", record.Message)
	}
}

func TestPodMatchingThisJobUIDIsAdopted(t *testing.T) {
	pod := jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 11, Reason: "Error"})
	pod.OwnerUID = "uid-train"

	record := terminalRecord(t, failedJobSource([]Pod{pod}))
	if !strings.Contains(record.Message, "exit code 11") {
		t.Errorf("pod with matching controller UID was not adopted: %q", record.Message)
	}
}

// The controller-uid label carries the same identity for pods whose
// ownerReference has been stripped.
func TestControllerUIDLabelIsHonoured(t *testing.T) {
	pod := jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 5, Reason: "Error"})
	pod.OwnerKind = ""
	pod.OwnerName = ""
	pod.Labels["batch.kubernetes.io/controller-uid"] = "uid-someone-else"

	record := terminalRecord(t, failedJobSource([]Pod{pod}))
	if strings.Contains(record.Message, "exit code 5") {
		t.Errorf("a pod whose controller-uid names another Job was adopted: %q", record.Message)
	}
}

// The recorder is restartable. A scheme that remembers what it already wrote
// in process memory silently stops working after a restart — exactly when a
// degraded record is most likely to be the one already durable. Ordering must
// therefore be a pure function of the record's own evidence.
func TestEvidenceOrderingSurvivesRecorderRestart(t *testing.T) {
	source := failedJobSource(nil)
	source.podErr = context.DeadlineExceeded

	first := &fakeWriter{}
	if _, err := newTestReconciler(source, first).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	// A brand new Reconciler stands in for a restarted recorder: no memory of
	// the degraded row it is about to supersede.
	source.podErr = nil
	source.pods = []Pod{jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 7, Reason: "Error"})}
	second := &fakeWriter{}
	if _, err := newTestReconciler(source, second).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	degraded := terminalFailureRecord(t, first.records)
	enriched := terminalFailureRecord(t, second.records)
	if !strings.Contains(enriched.Message, "exit code 7") {
		t.Fatalf("second recorder did not enrich: %q", enriched.Message)
	}
	if !enriched.ObservedAt.After(degraded.ObservedAt) {
		t.Errorf("after a restart the enriched row (%s) does not outrank the degraded one (%s); arg_max would tie",
			enriched.ObservedAt, degraded.ObservedAt)
	}
}

// A successful pod list that correlates no pods is not enrichment. Such a
// record must never displace evidence already recorded — the earlier fix
// retimestamped it as newer, which made arg_max replace an exit code with the
// generic Job reason deterministically.
func TestEmptyPodListDoesNotDisplaceRecordedEvidence(t *testing.T) {
	source := failedJobSource([]Pod{
		jobPod("train-abc", ContainerState{Name: "trainer", Terminated: true, ExitCode: 42, Reason: "Error"}),
	})
	writer := &fakeWriter{}
	reconciler := newTestReconciler(source, writer)
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	enriched := terminalFailureRecord(t, writer.records)
	before := len(writer.records)

	// Pods are gone — listed successfully, but the Job's pods have been
	// collected. podErr stays nil, so this is the "successful but empty" path.
	source.pods = nil
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}

	for _, record := range writer.records[before:] {
		if record.State != StateFailed {
			continue
		}
		if record.ObservedAt.After(enriched.ObservedAt) {
			t.Errorf("an evidence-free record was timestamped newer than the enriched one and would win arg_max: %q at %s",
				record.Reason, record.ObservedAt)
		}
	}
}

func terminalFailureRecord(t *testing.T, records []Record) Record {
	t.Helper()
	for i := len(records) - 1; i >= 0; i-- {
		if records[i].State == StateFailed {
			return records[i]
		}
	}
	t.Fatal("no terminal failure record written")
	return Record{}
}
