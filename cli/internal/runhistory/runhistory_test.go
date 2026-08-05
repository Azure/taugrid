package runhistory

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/experiment"
	portalruns "github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type fakeSource struct {
	jobs        []Job
	rayJobs     []RayJob
	workloads   []Workload
	rayErr      error
	workloadErr error
}

func (s *fakeSource) ListJobs(context.Context, string) ([]Job, error) { return s.jobs, nil }
func (s *fakeSource) ListRayJobs(context.Context, string) ([]RayJob, error) {
	return s.rayJobs, s.rayErr
}
func (s *fakeSource) ListWorkloads(context.Context, string) ([]Workload, error) {
	return s.workloads, s.workloadErr
}

type fakeWriter struct {
	mu        sync.Mutex
	records   []Record
	failures  int
	err       error
	writeCall int
}

type lifecycleStore struct {
	mu      sync.Mutex
	records []Record
}

func (s *lifecycleStore) Write(_ context.Context, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, records...)
	return nil
}

func (s *lifecycleStore) ListHistory(_ context.Context, scope portalruns.HistoryScope) ([]portalruns.Run, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := map[string]Record{}
	for _, record := range s.records {
		if scope.Cluster != "" && record.Cluster != scope.Cluster {
			continue
		}
		if scope.Namespace != "" && record.Namespace != scope.Namespace {
			continue
		}
		if scope.LocalQueue != "" && record.LocalQueue != scope.LocalQueue {
			continue
		}
		if scope.WorkspaceID != "" && record.WorkspaceID != scope.WorkspaceID {
			continue
		}
		latest[record.DurableID] = record
	}
	out := make([]portalruns.Run, 0, len(latest))
	for _, record := range latest {
		out = append(out, portalruns.Run{
			Name: record.OwnerName, Kind: record.OwnerKind, Status: record.State,
			Created: record.CreatedAt, RunID: record.RunID, Queue: record.LocalQueue,
			Namespace: record.Namespace, Cluster: record.Cluster, ResourceUID: record.ResourceUID,
			DurableID: record.DurableID, ExperimentTracking: record.ExperimentTracking,
		})
	}
	return out, nil
}

type emptyRunsReader struct{}

func (emptyRunsReader) ListJobs(context.Context, string) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

func (emptyRunsReader) ListRayJobs(context.Context, string) ([]byte, error) {
	return []byte(`{"items":[]}`), nil
}

func (w *fakeWriter) Write(_ context.Context, records []Record) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeCall++
	if w.failures > 0 {
		w.failures--
		return w.err
	}
	w.records = append(w.records, records...)
	return nil
}

func testMetadata(name string) Metadata {
	return Metadata{
		Name: name, Namespace: "ray", UID: "uid-" + name, ResourceVersion: "7", Generation: 3,
		CreatedAt: time.Date(2026, 7, 16, 10, 0, 0, 0, time.UTC),
		Labels:    map[string]string{experiment.LabelRunID: name, experiment.LabelWorkloadKind: experiment.WorkloadKindJob},
		Annotations: map[string]string{
			experiment.AnnotationImage:       "mcr.example/tau@sha256:deadbeef",
			experiment.AnnotationImageDigest: "sha256:deadbeef",
			experiment.AnnotationConfigHash:  "config",
			experiment.AnnotationCodeSHA:     "commit",
			experiment.AnnotationTauCommand:  "tau run train",
		},
	}
}

func newTestReconciler(source Source, writer Writer) *Reconciler {
	return &Reconciler{Source: source, Writer: writer, Cluster: "cluster-a", Now: func() time.Time {
		return time.Date(2026, 7, 16, 10, 1, 0, 0, time.UTC)
	}}
}

func TestHasTauLabelSupportsCanonicalMetadata(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"current": {workloadmeta.LabelWorkspace: "workspace"},
		"taugrid": {workloadmeta.LabelRunID: "run"},
	} {
		if !hasTauLabel(labels) {
			t.Errorf("%s labels were not recognized as Tau metadata", name)
		}
	}
	if hasTauLabel(map[string]string{"app": "other"}) {
		t.Fatal("unrelated labels were recognized as Tau metadata")
	}
}

func TestRayJobOwnedJobsAreExcluded(t *testing.T) {
	if !isRayJobOwned(Metadata{OwnerKind: "RayJob", OwnerName: "train"}) {
		t.Fatal("RayJob-owned Job was not recognized")
	}
	if isRayJobOwned(Metadata{OwnerKind: "CronJob", OwnerName: "schedule"}) {
		t.Fatal("non-RayJob owner was excluded")
	}
}

func TestJobInitialAndTerminalTransitions(t *testing.T) {
	metadata := testMetadata("train")
	metadata.Labels["kueue.x-k8s.io/queue-name"] = "jobqueue"
	source := &fakeSource{jobs: []Job{{Metadata: metadata, Suspended: true}}}
	writer := &fakeWriter{}
	reconciler := newTestReconciler(source, writer)
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if got := states(writer.records); strings.Join(got, ",") != "submitted,queued" {
		t.Fatalf("initial states = %v", got)
	}
	if writer.records[0].LocalQueue != "jobqueue" {
		t.Fatalf("initial receipt queue = %q", writer.records[0].LocalQueue)
	}
	source.jobs[0].Suspended = false
	source.jobs[0].Failed = 1
	source.jobs[0].Conditions = []Condition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}}
	source.jobs[0].CompletionTime = time.Date(2026, 7, 16, 10, 2, 0, 0, time.UTC)
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if got := writer.records[len(writer.records)-1].State; got != StateFailed {
		t.Fatalf("terminal state = %q", got)
	}
}

func TestDeletingNonTerminalJobIsCancelled(t *testing.T) {
	metadata := testMetadata("cancelled")
	metadata.Deleting = true
	source := &fakeSource{jobs: []Job{{Metadata: metadata, Active: 1}}}
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if record := writer.records[len(writer.records)-1]; record.State != StateCancelled || record.Reason != "Deleting" {
		t.Fatalf("cancelled record = %+v", record)
	}
}

func TestDeletingCompletedJobRemainsSucceeded(t *testing.T) {
	metadata := testMetadata("completed")
	metadata.Deleting = true
	source := &fakeSource{jobs: []Job{{
		Metadata: metadata, Succeeded: 1,
		Conditions: []Condition{{Type: "Complete", Status: "True"}},
	}}}
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if record := writer.records[len(writer.records)-1]; record.State != StateSucceeded {
		t.Fatalf("completed deleting record = %+v", record)
	}
}

func TestRetriedJobWithCompleteConditionSucceeds(t *testing.T) {
	source := &fakeSource{jobs: []Job{{
		Metadata: testMetadata("retried"), Failed: 1, Succeeded: 1,
		Conditions: []Condition{{Type: "Complete", Status: "True", Reason: "Completed"}},
	}}}
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if record := writer.records[len(writer.records)-1]; record.State != StateSucceeded {
		t.Fatalf("retried Job record = %+v", record)
	}
}

func TestFailedJobUsesConditionTransitionTime(t *testing.T) {
	failedAt := time.Date(2026, 7, 16, 10, 9, 0, 0, time.UTC)
	source := &fakeSource{jobs: []Job{{
		Metadata: testMetadata("failed-time"),
		Conditions: []Condition{{
			Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded",
			LastTransitionTime: failedAt,
		}},
	}}}
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	record := writer.records[len(writer.records)-1]
	if !record.CompletionAt.Equal(failedAt) || !record.ObservedAt.Equal(failedAt) {
		t.Fatalf("failed Job timestamps = completion %s observed %s", record.CompletionAt, record.ObservedAt)
	}
}

func TestRayJobShapeAndKueueAdmission(t *testing.T) {
	metadata := testMetadata("ray-train")
	metadata.Labels[experiment.LabelWorkloadKind] = experiment.WorkloadKindRayJobEval
	metadata.Annotations[experiment.AnnotationStellarProject] = "research"
	metadata.Annotations[experiment.AnnotationExperimentSource] = "stellar"
	metadata.Annotations[experiment.AnnotationStellarQuestion] = "q-42"
	metadata.Annotations[experiment.AnnotationStellarGroup] = "baseline"
	metadata.Annotations[experiment.AnnotationStellarTags] = `{"dataset":"fineweb"}`
	metadata.Annotations[experiment.AnnotationResultScope] = "az://results/research"
	metadata.Labels[workloadmeta.LabelWorkspace] = "sample"
	admitted := time.Date(2026, 7, 16, 10, 3, 0, 0, time.UTC)
	source := &fakeSource{
		rayJobs: []RayJob{{Metadata: metadata, DeploymentStatus: "Running", StartTime: admitted}},
		workloads: []Workload{{Metadata: Metadata{
			Name: "ray-train-workload", Namespace: "ray", Labels: map[string]string{experiment.LabelRunID: "ray-train"},
		}, Queue: "jobqueue", ClusterQueue: "taugrid-cq", Admitted: true, AdmittedAt: admitted}},
	}
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	record := writer.records[len(writer.records)-1]
	if record.WorkloadKind != experiment.WorkloadKindRayJobEval || record.State != StateRunning {
		t.Fatalf("RayJob record = %+v", record)
	}
	if record.LocalQueue != "jobqueue" || record.ClusterQueue != "taugrid-cq" || !record.AdmittedAt.Equal(admitted) {
		t.Fatalf("Kueue data = %+v", record)
	}
	if record.ExperimentTracking != "tracked" || record.ExperimentSource != "stellar" || record.WorkspaceID != "sample" || record.ResultScope != "az://results/research" || record.Tags["dataset"] != "fineweb" {
		t.Fatalf("Stellar metadata = %+v", record)
	}
}

func TestKueueAdmissionTransitionsQueuedJob(t *testing.T) {
	metadata := testMetadata("queued")
	admitted := time.Date(2026, 7, 16, 10, 3, 0, 0, time.UTC)
	source := &fakeSource{
		jobs: []Job{{Metadata: metadata, Suspended: true}},
		workloads: []Workload{{Metadata: Metadata{
			Name: "queued-workload", Namespace: "ray", Labels: map[string]string{experiment.LabelRunID: "queued"},
		}, Queue: "jobqueue", ClusterQueue: "taugrid-cq", Admitted: true, AdmittedAt: admitted}},
	}
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	record := writer.records[len(writer.records)-1]
	if record.State != StateAdmitted || record.LocalQueue != "jobqueue" || !record.AdmittedAt.Equal(admitted) {
		t.Fatalf("admitted Kueue record = %+v", record)
	}
}

func TestUntrackedAndTrackedMetadata(t *testing.T) {
	untracked := testMetadata("untracked")
	declaredOnly := testMetadata("declared-only")
	declaredOnly.Annotations[experiment.AnnotationStellarProject] = "project-without-producer"
	tracked := testMetadata("tracked")
	tracked.Annotations[experiment.AnnotationStellarProject] = "project-a"
	tracked.Annotations[experiment.AnnotationExperimentSource] = "stellar"
	source := &fakeSource{jobs: []Job{{Metadata: untracked}, {Metadata: declaredOnly}, {Metadata: tracked}}}
	writer := &fakeWriter{}
	if _, err := newTestReconciler(source, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	found := map[string]Record{}
	for _, record := range writer.records {
		if record.State == StateQueued {
			found[record.RunID] = record
		}
	}
	if found["untracked"].ExperimentTracking != "untracked" || found["untracked"].ExperimentSource != "" {
		t.Fatalf("untracked record = %+v", found["untracked"])
	}
	if found["declared-only"].ExperimentTracking != "untracked" || found["declared-only"].ExperimentSource != "" {
		t.Fatalf("declared-only record = %+v", found["declared-only"])
	}
	if found["tracked"].ExperimentTracking != "tracked" || found["tracked"].ExperimentSource != "stellar" {
		t.Fatalf("tracked record = %+v", found["tracked"])
	}
}

func TestRecorderScopeDefaults(t *testing.T) {
	source := &fakeSource{jobs: []Job{{Metadata: testMetadata("legacy")}}}
	writer := &fakeWriter{}
	reconciler := newTestReconciler(source, writer)
	reconciler.WorkspaceID = "workspace-a"
	reconciler.ResultScope = "scope-a"
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	record := writer.records[len(writer.records)-1]
	if record.WorkspaceID != "workspace-a" || record.ResultScope != "scope-a" {
		t.Fatalf("scope defaults not recorded: %+v", record)
	}
}

func TestWorkspaceScopedHistorySurvivesLiveObjectDeletion(t *testing.T) {
	store := &lifecycleStore{}
	for _, marker := range []struct {
		name, namespace, workspace, queue string
	}{
		{name: "marker-a", namespace: "team-a", workspace: "workspace-a", queue: "queue-a"},
		{name: "marker-b", namespace: "team-b", workspace: "workspace-b", queue: "queue-b"},
	} {
		metadata := testMetadata(marker.name)
		metadata.Namespace = marker.namespace
		metadata.Labels[workloadmeta.LabelWorkspace] = marker.workspace
		source := &fakeSource{
			jobs: []Job{{
				Metadata: metadata, Succeeded: 1,
				CompletionTime: time.Date(2026, 7, 16, 10, 2, 0, 0, time.UTC),
				Conditions:     []Condition{{Type: "Complete", Status: "True"}},
			}},
			workloads: []Workload{{
				Metadata: Metadata{Name: marker.name + "-workload", Namespace: marker.namespace, Labels: map[string]string{experiment.LabelRunID: marker.name}},
				Queue:    marker.queue, ClusterQueue: "shared-cq", Admitted: true,
			}},
		}
		reconciler := newTestReconciler(source, store)
		if _, err := reconciler.Reconcile(context.Background(), marker.namespace); err != nil {
			t.Fatalf("record %s: %v", marker.name, err)
		}
		// A second relist before deletion must not append duplicate transitions.
		before := len(store.records)
		if _, err := reconciler.Reconcile(context.Background(), marker.namespace); err != nil {
			t.Fatalf("relist %s: %v", marker.name, err)
		}
		if len(store.records) != before {
			t.Fatalf("relist appended duplicate transition for %s", marker.name)
		}
		source.jobs = nil
		source.workloads = nil
	}

	for _, authorized := range []struct {
		namespace, workspace, queue, want string
	}{
		{namespace: "team-a", workspace: "workspace-a", queue: "queue-a", want: "marker-a"},
		{namespace: "team-b", workspace: "workspace-b", queue: "queue-b", want: "marker-b"},
	} {
		snapshot, err := portalruns.Board(context.Background(), emptyRunsReader{}, portalruns.Options{
			Namespace: authorized.namespace, Queue: authorized.queue, History: store,
			HistoryScope: portalruns.HistoryScope{
				Cluster: "cluster-a", Namespace: authorized.namespace,
				LocalQueue: authorized.queue, WorkspaceID: authorized.workspace,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Runs) != 1 || snapshot.Runs[0].Name != authorized.want {
			t.Fatalf("%s visible rows = %+v", authorized.workspace, snapshot.Runs)
		}
		if snapshot.Runs[0].ExperimentTracking != "untracked" {
			t.Fatalf("%s experiment tracking = %q", authorized.want, snapshot.Runs[0].ExperimentTracking)
		}
	}
}

func TestExactDuplicateSuppressionAndOutOfOrderObservations(t *testing.T) {
	source := &fakeSource{jobs: []Job{{
		Metadata: testMetadata("train"), Failed: 1,
		Conditions: []Condition{{Type: "Failed", Status: "True"}},
	}}}
	writer := &fakeWriter{}
	reconciler := newTestReconciler(source, writer)
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	firstCount := len(writer.records)
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if len(writer.records) != firstCount {
		t.Fatalf("duplicate records emitted: %d -> %d", firstCount, len(writer.records))
	}
	// An older queued observation can arrive after terminal status. It is safe to
	// append because downstream readers order observations by observed_at/state.
	source.jobs[0].Failed = 0
	source.jobs[0].Conditions = nil
	source.jobs[0].Suspended = true
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if got := writer.records[len(writer.records)-1].State; got != StateQueued {
		t.Fatalf("out-of-order state was not emitted: %q", got)
	}
}

func TestResourceVersionOnlyUpdateIsNotATransition(t *testing.T) {
	source := &fakeSource{jobs: []Job{{Metadata: testMetadata("stable"), Active: 1}}}
	writer := &fakeWriter{}
	reconciler := newTestReconciler(source, writer)
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	firstCount := len(writer.records)
	source.jobs[0].ResourceVersion = "8"
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if len(writer.records) != firstCount {
		t.Fatalf("resourceVersion-only update emitted lifecycle records: %d -> %d", firstCount, len(writer.records))
	}
}

func TestRecordJSONMatchesLifecycleSchema(t *testing.T) {
	record := Record{
		Group: "group", OwnerKind: "Job", OwnerName: "run",
		SubmittedAt: time.Unix(1, 0).UTC(), CreatedAt: time.Unix(2, 0).UTC(),
		AdmittedAt: time.Unix(3, 0).UTC(), PodStartedAt: time.Unix(4, 0).UTC(),
		CompletionAt: time.Unix(5, 0).UTC(),
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(data)
	for _, field := range []string{
		`"run_group_id"`, `"owning_resource_kind"`,
		`"owning_resource_name"`, `"submit_time"`, `"created_time"`,
		`"kueue_admitted_time"`, `"pod_start_time"`, `"completion_time"`,
	} {
		if !strings.Contains(encoded, field) {
			t.Fatalf("lifecycle JSON missing %s: %s", field, encoded)
		}
	}
}

func TestLifecycleMappingReferencesRecordFields(t *testing.T) {
	record := Record{
		ObservedAt: time.Unix(1, 0).UTC(), ObservationID: "observation", DurableID: "durable", RunID: "run",
		WorkspaceID: "workspace", ResultScope: "scope", Project: "project",
		Group: "group", Tags: map[string]string{"key": "value"},
		OwnerKind: "Job", OwnerName: "job", Namespace: "ray", Cluster: "cluster",
		ResourceUID: "uid", ResourceVersion: "1", Generation: 1,
		SubmittedAt: time.Unix(2, 0).UTC(), CreatedAt: time.Unix(3, 0).UTC(),
		AdmittedAt: time.Unix(4, 0).UTC(), PodStartedAt: time.Unix(5, 0).UTC(),
		CompletionAt: time.Unix(6, 0).UTC(), State: StateSucceeded, Reason: "Complete",
		Message: "done", LocalQueue: "queue", ClusterQueue: "cluster-queue",
		WorkloadKind: "job", Image: "mcr/image@sha256:digest", ImageDigest: "sha256:digest",
		ConfigHash: "config", CodeSHA: "code", TauCommand: "tau run",
		ResultPath: "/results", ResultPVC: "results", ArtifactURI: "az://artifact",
		CheckpointURI: "az://checkpoint", ControllerVersion: "version",
		ExperimentTracking: "tracked", ExperimentSource: "stellar",
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, mapping := range lifecycleMapping {
		path := strings.TrimPrefix(mapping["path"], "$.")
		if _, ok := fields[path]; !ok {
			t.Fatalf("ingestion mapping column %q references missing record field %q", mapping["column"], path)
		}
	}
}

func TestRecordJSONOmitsMissingTimestamps(t *testing.T) {
	data, err := json.Marshal(Record{
		ObservedAt: time.Unix(1, 0).UTC(), ObservationID: "observation",
		DurableID: "durable", RunID: "run", ExperimentTracking: "untracked",
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"submit_time", "created_time", "kueue_admitted_time", "pod_start_time", "completion_time"} {
		if _, ok := fields[name]; ok {
			t.Errorf("zero timestamp %q was serialized: %s", name, data)
		}
	}
	if strings.Contains(string(data), "0001-01-01") {
		t.Fatalf("zero timestamp leaked into JSON: %s", data)
	}
}

func TestRestartRelistAndMissingOptionalCRDs(t *testing.T) {
	source := &fakeSource{
		jobs: []Job{{
			Metadata: testMetadata("terminal"), Succeeded: 1,
			Conditions: []Condition{{Type: "Complete", Status: "True"}},
		}},
		rayErr: ErrRayJobCRDMissing, workloadErr: ErrWorkloadCRDMissing,
	}
	firstWriter := &fakeWriter{}
	first := newTestReconciler(source, firstWriter)
	result, err := first.Reconcile(context.Background(), "ray")
	if err != nil {
		t.Fatal(err)
	}
	if result.RayJobsStatus != "missing-crd" {
		t.Fatalf("RayJob status = %q", result.RayJobsStatus)
	}
	if result.WorkloadsStatus != "missing-crd" {
		t.Fatalf("Workload status = %q", result.WorkloadsStatus)
	}
	secondWriter := &fakeWriter{}
	second := newTestReconciler(source, secondWriter)
	second.Now = func() time.Time { return time.Date(2026, 7, 17, 10, 1, 0, 0, time.UTC) }
	if _, err := second.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if len(secondWriter.records) == 0 || secondWriter.records[len(secondWriter.records)-1].State != StateSucceeded {
		t.Fatalf("restart relist did not emit current terminal state: %+v", secondWriter.records)
	}
	if firstWriter.records[len(firstWriter.records)-1].ObservationID != secondWriter.records[len(secondWriter.records)-1].ObservationID {
		t.Fatalf("restart changed deterministic observation identity")
	}
	if !firstWriter.records[len(firstWriter.records)-1].ObservedAt.Equal(secondWriter.records[len(secondWriter.records)-1].ObservedAt) {
		t.Fatalf("restart changed lifecycle event time")
	}
}

func TestWriterRetryAndErrorPropagation(t *testing.T) {
	source := &fakeSource{jobs: []Job{{Metadata: testMetadata("retry")}}}
	writer := &fakeWriter{failures: 2, err: errors.New("temporary ingest failure")}
	reconciler := newTestReconciler(source, writer)
	reconciler.WriterRetries = 2
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	if writer.writeCall != 3 {
		t.Fatalf("write calls = %d, want 3", writer.writeCall)
	}
	writer = &fakeWriter{failures: 3, err: errors.New("ingest unavailable")}
	reconciler = newTestReconciler(source, writer)
	reconciler.WriterRetries = 1
	if _, err := reconciler.Reconcile(context.Background(), "ray"); err == nil || !strings.Contains(err.Error(), "ingest unavailable") {
		t.Fatalf("write error = %v", err)
	}
}

func TestRecordNeverIncludesSecretsLogsOrMetricValues(t *testing.T) {
	metadata := testMetadata("safe")
	metadata.Annotations["example.com/secret"] = "do-not-record"
	metadata.Annotations["example.com/pod-log"] = "do-not-record"
	metadata.Annotations["example.com/metric-value"] = "42"
	writer := &fakeWriter{}
	if _, err := newTestReconciler(&fakeSource{jobs: []Job{{Metadata: metadata}}}, writer).Reconcile(context.Background(), "ray"); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(writer.records)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"do-not-record", "secret", "pod-log", "metric-value"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("record leaked %q: %s", forbidden, data)
		}
	}
}

func TestHealthStateIsRaceSafe(t *testing.T) {
	health := &Health{}
	server := httptest.NewServer(health.Handler())
	defer server.Close()
	var wait sync.WaitGroup
	for i := 0; i < 20; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			health.MarkSuccess(Result{RayJobsStatus: "available"}, time.Now().UTC().Format(time.RFC3339Nano))
			response, err := http.Get(server.URL + "/readyz")
			if err == nil {
				_ = response.Body.Close()
			}
		}()
	}
	wait.Wait()
	response, err := http.Get(server.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("ready status = %d", response.StatusCode)
	}
}

func states(records []Record) []string {
	out := make([]string, 0, len(records))
	for _, record := range records {
		out = append(out, record.State)
	}
	return out
}
