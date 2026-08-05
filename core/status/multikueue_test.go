package status

import (
	"reflect"
	"testing"

	runtopology "github.com/Azure/taugrid/core/topology"
)

// The JSON fixtures below mirror real `kubectl get workloads.kueue.x-k8s.io
// -o json` / `kubectl get job|rayjob -o json` shapes for Kueue v0.18.2 (see
// charts/kueue/templates/crd/kueue.x-k8s.io_workloads.yaml and
// charts/kuberay-operator/crds/ray.io_rayjobs.yaml), trimmed to the fields
// hydrate*() reads.

func TestHydrateWorkloads_SingleCluster_Unchanged(t *testing.T) {
	// An ordinary, non-MultiKueue Workload: admitted, no clusterName,
	// nominatedClusterNames, or admissionChecks at all.
	data := []byte(`{
		"items": [{
			"metadata": {"name": "train-001"},
			"spec": {"queueName": "sample-h100"},
			"status": {
				"conditions": [
					{"type": "QuotaReserved", "status": "True", "reason": "AdmittedByTest", "message": ""},
					{"type": "Admitted", "status": "True", "reason": "Admitted", "message": "The workload is admitted"}
				]
			}
		}]
	}`)
	got := hydrateWorkloads(data)
	if len(got) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(got))
	}
	w := got[0]
	// Note: hydrateWorkloads sets Phase from whichever condition type it
	// sees first that isn't already superseded; with QuotaReserved listed
	// before Admitted (the real transition order Kueue reports), Phase
	// stays "QuotaReserved" even though Admitted is true. That ordering
	// quirk predates this change and is out of scope here — we only
	// assert the (unaffected) Admitted flag and the new MultiKueue
	// fields.
	if w.Name != "train-001" || w.Queue != "sample-h100" || !w.Admitted {
		t.Fatalf("unexpected base workload fields: %+v", w)
	}
	if w.ClusterName != "" {
		t.Errorf("expected empty ClusterName for single-cluster workload, got %q", w.ClusterName)
	}
	if w.NominatedClusterNames != nil {
		t.Errorf("expected nil NominatedClusterNames, got %v", w.NominatedClusterNames)
	}
	if w.AdmissionChecks != nil {
		t.Errorf("expected nil AdmissionChecks, got %+v", w.AdmissionChecks)
	}

	snap := Snapshot{Name: "train-001", Namespace: "ray", JobFound: true, Workloads: got}
	if snap.IsMultiKueue() {
		t.Error("expected IsMultiKueue() false for ordinary single-cluster workload")
	}
	if got := snap.SelectedWorkerCluster(); got != "" {
		t.Errorf("expected empty SelectedWorkerCluster(), got %q", got)
	}
	if got := snap.NominatedWorkerClusters(); len(got) != 0 {
		t.Errorf("expected no NominatedWorkerClusters(), got %v", got)
	}
	if got := snap.AdmissionCheckSummaries(); len(got) != 0 {
		t.Errorf("expected no AdmissionCheckSummaries(), got %+v", got)
	}
}

func TestHydrateWorkloads_MultiKueue_PendingBeforeNomination(t *testing.T) {
	// MultiKueue admission check reported before Kueue has nominated any
	// worker clusters — clusterName and nominatedClusterNames are both
	// absent, so the manager's Job/RayJob managedBy marker is what proves
	// this is a MultiKueue workload.
	data := []byte(`{
		"items": [{
			"metadata": {"name": "train-001"},
			"spec": {"queueName": "sample-h100"},
			"status": {
				"conditions": [
					{"type": "QuotaReserved", "status": "True", "reason": "Pending", "message": ""}
				],
				"admissionChecks": [
					{"name": "multikueue", "state": "Pending", "message": "", "lastTransitionTime": "2026-07-09T10:00:00Z"}
				]
			}
		}]
	}`)
	got := hydrateWorkloads(data)
	if len(got) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(got))
	}
	w := got[0]
	if len(w.AdmissionChecks) != 1 || w.AdmissionChecks[0].Name != "multikueue" || w.AdmissionChecks[0].State != "Pending" {
		t.Fatalf("unexpected admission checks: %+v", w.AdmissionChecks)
	}
	wantTime := mustTime("2026-07-09T10:00:00Z")
	if !w.AdmissionChecks[0].LastTransitionTime.Equal(wantTime) {
		t.Errorf("expected LastTransitionTime %v, got %v", wantTime, w.AdmissionChecks[0].LastTransitionTime)
	}

	snap := Snapshot{Name: "train-001", Namespace: "ray", JobManagedBy: multiKueueManagedBy, Workloads: got}
	if !snap.IsMultiKueue() {
		t.Error("expected IsMultiKueue() true when the manager view marks the job as MultiKueue")
	}
	if got := snap.SelectedWorkerCluster(); got != "" {
		t.Errorf("expected no selected cluster yet, got %q", got)
	}
	if got := snap.NominatedWorkerClusters(); len(got) != 0 {
		t.Errorf("expected no nominated clusters yet, got %v", got)
	}
}

func TestHydrateWorkloads_MultiKueue_Nominated(t *testing.T) {
	// Kueue has nominated candidate worker clusters but not yet selected
	// one. nominatedClusterNames is intentionally unsorted (with a
	// duplicate) in the fixture to verify hydration sorting/dedup.
	data := []byte(`{
		"items": [{
			"metadata": {"name": "train-001"},
			"spec": {"queueName": "sample-h100"},
			"status": {
				"conditions": [
					{"type": "QuotaReserved", "status": "True", "reason": "Pending", "message": ""}
				],
				"nominatedClusterNames": ["worker-b", "worker-a", "worker-b"],
				"admissionChecks": [
					{"name": "multikueue", "state": "Pending", "message": "Waiting for worker clusters", "lastTransitionTime": "2026-07-09T10:01:00Z"}
				]
			}
		}]
	}`)
	got := hydrateWorkloads(data)
	if len(got) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(got))
	}
	w := got[0]
	if want := []string{"worker-a", "worker-b"}; !reflect.DeepEqual(w.NominatedClusterNames, want) {
		t.Fatalf("expected deduped/sorted NominatedClusterNames %v, got %v", want, w.NominatedClusterNames)
	}
	if w.ClusterName != "" {
		t.Errorf("expected empty ClusterName while nominated, got %q", w.ClusterName)
	}

	snap := Snapshot{Name: "train-001", Namespace: "ray", Workloads: got}
	if !snap.IsMultiKueue() {
		t.Error("expected IsMultiKueue() true")
	}
	if got := snap.SelectedWorkerCluster(); got != "" {
		t.Errorf("expected no selected cluster while nominated, got %q", got)
	}
	if want, got := []string{"worker-a", "worker-b"}, snap.NominatedWorkerClusters(); !reflect.DeepEqual(got, want) {
		t.Errorf("expected NominatedWorkerClusters() %v, got %v", want, got)
	}
}

func TestHydrateWorkloads_MultiKueue_Selected(t *testing.T) {
	// Kueue has picked a worker cluster: clusterName is set, and
	// nominatedClusterNames is reset (Kueue resets it once clusterName is
	// set — see the workloads CRD validation rule).
	data := []byte(`{
		"items": [{
			"metadata": {"name": "train-001"},
			"spec": {"queueName": "sample-h100"},
			"status": {
				"conditions": [
					{"type": "QuotaReserved", "status": "True", "reason": "Admitted", "message": ""},
					{"type": "Admitted", "status": "True", "reason": "Admitted", "message": "The workload is admitted"}
				],
				"clusterName": "worker-b",
				"admissionChecks": [
					{"name": "multikueue", "state": "Ready", "message": "The workload got reservation on \"worker-b\"", "lastTransitionTime": "2026-07-09T10:02:00Z"}
				]
			}
		}]
	}`)
	got := hydrateWorkloads(data)
	w := got[0]
	if w.ClusterName != "worker-b" {
		t.Fatalf("expected ClusterName worker-b, got %q", w.ClusterName)
	}
	if w.NominatedClusterNames != nil {
		t.Errorf("expected nil NominatedClusterNames once selected, got %v", w.NominatedClusterNames)
	}

	snap := Snapshot{Name: "train-001", Namespace: "ray", Workloads: got}
	if !snap.IsMultiKueue() {
		t.Error("expected IsMultiKueue() true")
	}
	if got := snap.SelectedWorkerCluster(); got != "worker-b" {
		t.Errorf("expected SelectedWorkerCluster() worker-b, got %q", got)
	}
	if got := snap.NominatedWorkerClusters(); len(got) != 0 {
		t.Errorf("expected no nominated clusters once selected, got %v", got)
	}
	summaries := snap.AdmissionCheckSummaries()
	if len(summaries) != 1 || summaries[0].State != "Ready" || summaries[0].WorkloadName != "train-001" {
		t.Fatalf("unexpected admission check summaries: %+v", summaries)
	}
}

func TestHydrateWorkloads_MultiKueue_RetryAndRejected(t *testing.T) {
	// Multiple admission checks in non-alphabetical order; verify
	// deterministic sort-by-name and that Retry/Rejected states survive
	// hydration untouched (Tau must not editorialize Kueue's state
	// machine).
	data := []byte(`{
		"items": [{
			"metadata": {"name": "train-001"},
			"spec": {"queueName": "sample-h100"},
			"status": {
				"conditions": [
					{"type": "QuotaReserved", "status": "True", "reason": "Pending", "message": ""}
				],
				"admissionChecks": [
					{"name": "prov-worker-b", "state": "Rejected", "message": "quota exceeded", "lastTransitionTime": "2026-07-09T10:03:00Z"},
					{"name": "multikueue", "state": "Retry", "message": "retrying admission", "lastTransitionTime": "2026-07-09T10:03:30Z"}
				]
			}
		}]
	}`)
	got := hydrateWorkloads(data)
	w := got[0]
	if len(w.AdmissionChecks) != 2 {
		t.Fatalf("expected 2 admission checks, got %d", len(w.AdmissionChecks))
	}
	if w.AdmissionChecks[0].Name != "multikueue" || w.AdmissionChecks[0].State != "Retry" {
		t.Errorf("expected first (sorted) check to be multikueue/Retry, got %+v", w.AdmissionChecks[0])
	}
	if w.AdmissionChecks[1].Name != "prov-worker-b" || w.AdmissionChecks[1].State != "Rejected" {
		t.Errorf("expected second (sorted) check to be prov-worker-b/Rejected, got %+v", w.AdmissionChecks[1])
	}
}

func TestHydrateWorkloads_MalformedAndMissingFields(t *testing.T) {
	// Malformed JSON must not panic; hydrateWorkloads soft-fails to nil,
	// matching Fetch's "show what we can" contract.
	if got := hydrateWorkloads([]byte(`not json`)); got != nil {
		t.Errorf("expected nil for malformed JSON, got %+v", got)
	}
	if got := hydrateWorkloads(nil); got != nil {
		t.Errorf("expected nil for empty input, got %+v", got)
	}

	// Missing/null MultiKueue fields (older Kueue API objects, or a
	// Workload that predates the MultiKueue admission check being wired
	// up) must hydrate to zero values without panicking.
	data := []byte(`{
		"items": [{
			"metadata": {"name": "train-001"},
			"spec": {"queueName": "sample-h100"},
			"status": {
				"conditions": null,
				"clusterName": "",
				"nominatedClusterNames": null,
				"admissionChecks": null
			}
		}]
	}`)
	got := hydrateWorkloads(data)
	if len(got) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(got))
	}
	w := got[0]
	if w.ClusterName != "" || w.NominatedClusterNames != nil || w.AdmissionChecks != nil {
		t.Errorf("expected zero-value MultiKueue fields for absent data, got %+v", w)
	}

	// An admissionChecks entry with an empty name (shouldn't happen per
	// the CRD's `required: [name]`, but hydration must not crash or
	// synthesize a phantom check for it).
	dataEmptyName := []byte(`{
		"items": [{
			"metadata": {"name": "train-001"},
			"spec": {"queueName": "sample-h100"},
			"status": {
				"admissionChecks": [{"name": "", "state": "Pending", "message": "", "lastTransitionTime": null}]
			}
		}]
	}`)
	got2 := hydrateWorkloads(dataEmptyName)
	if len(got2) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(got2))
	}
	if len(got2[0].AdmissionChecks) != 0 {
		t.Errorf("expected empty-name admission check to be dropped, got %+v", got2[0].AdmissionChecks)
	}
}

func TestSnapshot_IsMultiKueue_FalseForGenericAdmissionChecksWithoutSignals(t *testing.T) {
	snap := Snapshot{
		Workloads: []Workload{{
			Name: "train-001",
			AdmissionChecks: []AdmissionCheck{
				{Name: "quota-check", State: "Ready", ControllerName: "kueue.x-k8s.io/provisioning"},
			},
		}},
	}
	if snap.IsMultiKueue() {
		t.Fatal("generic workload admission checks must not imply MultiKueue without managedBy or placement fields")
	}
}

func TestSnapshot_IsMultiKueue_TrueForMultiKueueControllerCheck(t *testing.T) {
	snap := Snapshot{
		Workloads: []Workload{{
			Name: "train-001",
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Pending", ControllerName: multiKueueControllerName},
			},
		}},
	}
	if !snap.IsMultiKueue() {
		t.Fatal("known MultiKueue controller checks must identify manager-side MultiKueue even before managedBy or placement fields appear")
	}
}

func TestSnapshot_IsMultiKueue_TrueForFailedLookupExactNameFallback(t *testing.T) {
	snap := Snapshot{
		Workloads: []Workload{{
			Name: "train-001",
			AdmissionChecks: []AdmissionCheck{
				{Name: multiKueueAdmissionCheckName, State: "Pending", ControllerLookupFailed: true},
			},
		}},
	}
	if !snap.IsMultiKueue() {
		t.Fatal("failed controller lookup on the exact multikueue admission-check name should still identify MultiKueue")
	}
}

func TestSnapshot_IsMultiKueue_FalseForFailedLookupGenericName(t *testing.T) {
	snap := Snapshot{
		Workloads: []Workload{{
			Name: "train-001",
			AdmissionChecks: []AdmissionCheck{
				{Name: "quota-check", State: "Pending", ControllerLookupFailed: true},
			},
		}},
	}
	if snap.IsMultiKueue() {
		t.Fatal("failed controller lookup on a generic admission-check name must not imply MultiKueue")
	}
}

func TestSnapshot_IsMultiKueue_FalseForDifferentControllerOnExactName(t *testing.T) {
	snap := Snapshot{
		Workloads: []Workload{{
			Name: "train-001",
			AdmissionChecks: []AdmissionCheck{
				{Name: multiKueueAdmissionCheckName, State: "Pending", ControllerName: "kueue.x-k8s.io/provisioning"},
			},
		}},
	}
	if snap.IsMultiKueue() {
		t.Fatal("successful lookup with a different controller must override exact-name fallback")
	}
}

func TestSnapshot_MultiKueueState_ControllerOnlySignals(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  MultiKueueState
	}{
		{name: "pending", state: "Pending", want: MultiKueueStatePending},
		{name: "retry", state: "Retry", want: MultiKueueStateRetry},
		{name: "rejected", state: "Rejected", want: MultiKueueStateRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := Snapshot{
				Workloads: []Workload{{
					Name: "train-001",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: tt.state, ControllerName: multiKueueControllerName},
					},
				}},
			}
			if got := snap.MultiKueueState(); got != tt.want {
				t.Fatalf("MultiKueueState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSnapshot_MultiKueueState_FailedLookupExactNameFallback(t *testing.T) {
	tests := []struct {
		name  string
		state string
		want  MultiKueueState
	}{
		{name: "pending", state: "Pending", want: MultiKueueStatePending},
		{name: "retry", state: "Retry", want: MultiKueueStateRetry},
		{name: "rejected", state: "Rejected", want: MultiKueueStateRejected},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			snap := Snapshot{
				Workloads: []Workload{{
					Name: "train-001",
					AdmissionChecks: []AdmissionCheck{{
						Name:                   multiKueueAdmissionCheckName,
						State:                  tt.state,
						ControllerLookupFailed: true,
					}},
				}},
			}
			if got := snap.MultiKueueState(); got != tt.want {
				t.Fatalf("MultiKueueState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSnapshot_MultiKueueState_UnknownControllerDoesNotImplyReadyOrRejected(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name: "train-001",
			AdmissionChecks: []AdmissionCheck{
				{Name: "custom-placement", State: "Rejected"},
			},
		}},
	}
	if !snap.IsMultiKueue() {
		t.Fatal("managedBy should identify the workload as MultiKueue even before worker placement fields appear")
	}
	if got := snap.MultiKueueState(); got != MultiKueueStatePending {
		t.Fatalf("expected pending state when controller lookup is unknown, got %q", got)
	}
}

func TestSnapshot_MultiKueueState_DifferentControllerOnExactNameDoesNotFallback(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name: "train-001",
			AdmissionChecks: []AdmissionCheck{{
				Name:           multiKueueAdmissionCheckName,
				State:          "Rejected",
				ControllerName: "kueue.x-k8s.io/provisioning",
			}},
		}},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStatePending {
		t.Fatalf("expected pending state when lookup succeeded with a different controller, got %q", got)
	}
}

func TestHydrateJob_ManagedBy(t *testing.T) {
	data := []byte(`{
		"metadata": {"uid": "abc-123", "creationTimestamp": "2026-07-09T09:00:00Z"},
		"spec": {"suspend": false, "parallelism": 1, "managedBy": "kueue.x-k8s.io/multikueue"},
		"status": {"active": 0, "succeeded": 0, "failed": 0}
	}`)
	var s Snapshot
	hydrateJob(&s, data)
	if s.JobManagedBy != multiKueueManagedBy {
		t.Fatalf("expected JobManagedBy %q, got %q", multiKueueManagedBy, s.JobManagedBy)
	}
	s.JobFound = true
	if !s.IsMultiKueue() {
		t.Error("expected IsMultiKueue() true from spec.managedBy alone, before any Workload MultiKueue status exists")
	}
}

func TestHydrateJob_ManagedBy_AbsentIsSingleCluster(t *testing.T) {
	data := []byte(`{
		"metadata": {"uid": "abc-123", "creationTimestamp": "2026-07-09T09:00:00Z"},
		"spec": {"suspend": false, "parallelism": 1},
		"status": {"active": 1, "succeeded": 0, "failed": 0}
	}`)
	var s Snapshot
	hydrateJob(&s, data)
	if s.JobManagedBy != "" {
		t.Fatalf("expected empty JobManagedBy for ordinary Job, got %q", s.JobManagedBy)
	}
	s.JobFound = true
	if s.IsMultiKueue() {
		t.Error("expected IsMultiKueue() false for ordinary Job")
	}
}

func TestSnapshot_IsKueueManaged_TrueForQueueLabel(t *testing.T) {
	s := Snapshot{
		Labels: map[string]string{runtopology.QueueLabel: "jobqueue"},
	}
	if got := s.ManagerLocalQueue(); got != "jobqueue" {
		t.Fatalf("manager queue = %q, want jobqueue", got)
	}
	if !s.IsKueueManaged() {
		t.Fatal("expected queue label to mark snapshot as Kueue-managed")
	}
}

func TestSnapshot_IsKueueManaged_TrueForTopologyQueueAnnotation(t *testing.T) {
	s := Snapshot{
		Annotations: map[string]string{runtopology.AnnotationTopologyQueue: "jobqueue"},
	}
	if got := s.ManagerLocalQueue(); got != "jobqueue" {
		t.Fatalf("manager queue = %q, want jobqueue", got)
	}
	if !s.IsKueueManaged() {
		t.Fatal("expected topology queue annotation to mark snapshot as Kueue-managed")
	}
}

func TestHydrateRayJob_ManagedBy(t *testing.T) {
	data := []byte(`{
		"metadata": {"uid": "ray-123", "name": "train-001", "creationTimestamp": "2026-07-09T09:00:00Z"},
		"spec": {"managedBy": "kueue.x-k8s.io/multikueue"},
		"status": {
			"rayClusterName": "train-001-raycluster",
			"jobDeploymentStatus": "Running",
			"jobId": "train-001-job"
		}
	}`)
	var s Snapshot
	hydrateRayJob(&s, data)
	if s.RayJob.ManagedBy != multiKueueManagedBy {
		t.Fatalf("expected RayJob.ManagedBy %q, got %q", multiKueueManagedBy, s.RayJob.ManagedBy)
	}
	if !s.IsMultiKueue() {
		t.Error("expected IsMultiKueue() true from RayJob spec.managedBy")
	}
}

func TestHydrateRayJob_ManagedBy_DefaultKuberayOperator(t *testing.T) {
	data := []byte(`{
		"metadata": {"uid": "ray-123", "name": "train-001", "creationTimestamp": "2026-07-09T09:00:00Z"},
		"spec": {"managedBy": "ray.io/kuberay-operator"},
		"status": {"jobDeploymentStatus": "Running"}
	}`)
	var s Snapshot
	hydrateRayJob(&s, data)
	if s.RayJob.ManagedBy != "ray.io/kuberay-operator" {
		t.Fatalf("expected default managedBy, got %q", s.RayJob.ManagedBy)
	}
	if s.IsMultiKueue() {
		t.Error("expected IsMultiKueue() false for the default (non-MultiKueue) managedBy value")
	}
}

func TestSnapshot_NominatedWorkerClusters_DedupesAcrossMultipleWorkloads(t *testing.T) {
	// Fetch tolerates multiple Workloads for one job; verify the
	// aggregate accessor merges and dedupes across all of them.
	snap := Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		Workloads: []Workload{
			{Name: "train-001-a", NominatedClusterNames: []string{"worker-b", "worker-a"}},
			{Name: "train-001-b", NominatedClusterNames: []string{"worker-a", "worker-c"}},
		},
	}
	want := []string{"worker-a", "worker-b", "worker-c"}
	if got := snap.NominatedWorkerClusters(); !reflect.DeepEqual(got, want) {
		t.Fatalf("expected %v, got %v", want, got)
	}
}

func TestSnapshot_AdmissionCheckSummaries_SortedByWorkloadThenName(t *testing.T) {
	snap := Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		Workloads: []Workload{
			{Name: "train-001-b", AdmissionChecks: []AdmissionCheck{{Name: "multikueue", State: "Ready"}}},
			{Name: "train-001-a", AdmissionChecks: []AdmissionCheck{
				{Name: "prov", State: "Pending"},
				{Name: "multikueue", State: "Ready"},
			}},
		},
	}
	got := snap.AdmissionCheckSummaries()
	if len(got) != 3 {
		t.Fatalf("expected 3 summaries, got %d: %+v", len(got), got)
	}
	wantOrder := []struct {
		workload, check string
	}{
		{"train-001-a", "multikueue"},
		{"train-001-a", "prov"},
		{"train-001-b", "multikueue"},
	}
	for i, want := range wantOrder {
		if got[i].WorkloadName != want.workload || got[i].Name != want.check {
			t.Errorf("summary[%d]: expected %s/%s, got %s/%s", i, want.workload, want.check, got[i].WorkloadName, got[i].Name)
		}
	}
}

func TestSnapshot_SelectedWorkerCluster_FirstNonEmptyWins(t *testing.T) {
	snap := Snapshot{
		Workloads: []Workload{
			{Name: "a"},
			{Name: "b", ClusterName: "worker-b"},
			{Name: "c", ClusterName: "worker-c"},
		},
	}
	if got := snap.SelectedWorkerCluster(); got != "worker-b" {
		t.Errorf("expected first non-empty ClusterName worker-b, got %q", got)
	}
}

func TestSnapshot_PlacementWorkerCluster_SelectedWorkloadExact(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{
				Name:        "ready",
				ClusterName: "worker-a",
				AdmissionChecks: []AdmissionCheck{
					{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
				},
			},
			{
				Name:        "selected",
				ClusterName: "worker-b",
			},
		},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStateSelected {
		t.Fatalf("expected selected placement aggregate, got %q", got)
	}
	if got := snap.PlacementWorkerCluster(); got != "worker-b" {
		t.Fatalf("expected PlacementWorkerCluster() worker-b, got %q", got)
	}
}

func TestSnapshot_PlacementWorkerCluster_ConflictingAssignmentsReturnEmpty(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{Name: "a", ClusterName: "worker-a"},
			{Name: "b", ClusterName: "worker-b"},
		},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStateSelected {
		t.Fatalf("expected selected placement aggregate, got %q", got)
	}
	if got := snap.PlacementWorkerCluster(); got != "" {
		t.Fatalf("expected conflicting worker assignments to be ambiguous, got %q", got)
	}
}

func TestSnapshot_MultiKueueState_Precedence(t *testing.T) {
	tests := []struct {
		name string
		snap Snapshot
		want MultiKueueState
	}{
		{
			name: "rejected beats retry across observed checks",
			snap: Snapshot{
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					ClusterName:           "worker-a",
					NominatedClusterNames: []string{"worker-a", "worker-b"},
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Retry", ControllerName: multiKueueControllerName},
						{Name: "worker-a", State: "Ready", ControllerName: multiKueueControllerName},
						{Name: "worker-b", State: "Rejected", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: MultiKueueStateRejected,
		},
		{
			name: "retry beats ready selected nominated",
			snap: Snapshot{
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					ClusterName:           "worker-a",
					NominatedClusterNames: []string{"worker-a", "worker-b"},
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Retry", ControllerName: multiKueueControllerName},
						{Name: "worker-a", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: MultiKueueStateRetry,
		},
		{
			name: "rejected beats pending across observed checks",
			snap: Snapshot{
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Pending", ControllerName: multiKueueControllerName},
						{Name: "worker-b", State: "Rejected", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: MultiKueueStateRejected,
		},
		{
			name: "ready requires at least one observed check and all ready",
			snap: Snapshot{
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
						{Name: "worker-a", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: MultiKueueStateReady,
		},
		{
			name: "selected when cluster chosen and checks absent",
			snap: Snapshot{
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					ClusterName: "worker-a",
				}},
			},
			want: MultiKueueStateSelected,
		},
		{
			name: "nominated when candidates exist but no cluster selected",
			snap: Snapshot{
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					NominatedClusterNames: []string{"worker-a", "worker-b"},
				}},
			},
			want: MultiKueueStateNominated,
		},
		{
			name: "pending when only managed-by marker is present",
			snap: Snapshot{
				JobManagedBy: multiKueueManagedBy,
			},
			want: MultiKueueStatePending,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.snap.MultiKueueState(); got != tt.want {
				t.Fatalf("MultiKueueState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSnapshot_MultiKueueState_AggregatesPrimaryChecksAcrossWorkloads(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{Name: "a", AdmissionChecks: []AdmissionCheck{{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName}}},
			{Name: "b", AdmissionChecks: []AdmissionCheck{{Name: "multikueue", State: "Rejected", ControllerName: multiKueueControllerName}}},
		},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStateRejected {
		t.Fatalf("expected rejected precedence across workloads, got %q", got)
	}
}

func TestSnapshot_MultiKueueState_PendingBeatsReadyAcrossWorkloads(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{Name: "a", AdmissionChecks: []AdmissionCheck{{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName}}},
			{Name: "b", AdmissionChecks: []AdmissionCheck{{Name: "multikueue", State: "Pending", ControllerName: multiKueueControllerName}}},
		},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStatePending {
		t.Fatalf("expected pending precedence across workloads, got %q", got)
	}
}

func TestSnapshot_MultiKueueState_AllWorkloadsMustBeReady(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{
				Name: "a",
				AdmissionChecks: []AdmissionCheck{
					{Name: "worker-a", State: "Ready", ControllerName: multiKueueControllerName},
				},
			},
			{
				Name: "b",
				AdmissionChecks: []AdmissionCheck{
					{Name: "worker-b", State: "Ready", ControllerName: multiKueueControllerName},
				},
			},
		},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStateReady {
		t.Fatalf("expected ready when every workload has observed ready checks, got %q", got)
	}
}

func TestSnapshot_MultiKueueState_ReadyAndEmptyWorkloadStaysPending(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{
				Name: "a",
				AdmissionChecks: []AdmissionCheck{
					{Name: "worker-a", State: "Ready", ControllerName: multiKueueControllerName},
				},
			},
			{Name: "b"},
		},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStatePending {
		t.Fatalf("expected pending when one workload has no checks or placement, got %q", got)
	}
}

func TestSnapshot_MultiKueueState_ReadyAndSelectedFallsBackToSelected(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{
				Name: "a",
				AdmissionChecks: []AdmissionCheck{
					{Name: "worker-a", State: "Ready", ControllerName: multiKueueControllerName},
				},
			},
			{
				Name:        "b",
				ClusterName: "worker-b",
			},
		},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStateSelected {
		t.Fatalf("expected selected when a second workload has selected placement but no ready checks, got %q", got)
	}
}

func TestSnapshot_MultiKueueState_IgnoresGenericChecksForPlacement(t *testing.T) {
	for _, genericState := range []string{"Pending", "Rejected"} {
		t.Run(genericState, func(t *testing.T) {
			snap := Snapshot{
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					Name:        "a",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
						{Name: "quota-check", State: genericState, ControllerName: "kueue.x-k8s.io/provisioning"},
					},
				}},
			}
			if got := snap.MultiKueueState(); got != MultiKueueStateReady {
				t.Fatalf("expected placement ready from MultiKueue checks only, got %q", got)
			}
			if snap.AllAdmissionChecksReady() {
				t.Fatalf("generic %s admission checks must still block overall readiness", genericState)
			}
		})
	}
}

func TestSnapshot_MultiKueueState_DoesNotMutateGenericCheckState(t *testing.T) {
	snap := Snapshot{
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name:        "a",
			ClusterName: "worker-a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "aaa-generic", State: "Pending", ControllerName: "kueue.x-k8s.io/provisioning"},
				{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
			},
		}},
	}
	if got := snap.MultiKueueState(); got != MultiKueueStateReady {
		t.Fatalf("expected placement ready from MultiKueue checks only, got %q", got)
	}
	if snap.AllAdmissionChecksReady() {
		t.Fatal("generic pending admission checks must remain pending after placement evaluation")
	}
	if got := snap.AdmissionCheckSummaries()[0].Name; got != "aaa-generic" {
		t.Fatalf("expected generic admission check to remain intact after placement evaluation, got %q", got)
	}
}
