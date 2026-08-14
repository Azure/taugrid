// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestHasTauLabel(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{"matching label", map[string]string{workloadmeta.LabelJob: "train"}, true},
		{"non-matching label", map[string]string{"app": "foo"}, false},
		{"empty map", map[string]string{}, false},
		{"nil map", nil, false},
		{"mixed labels", map[string]string{"app": "foo", workloadmeta.LabelRun: "r1"}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := hasTauLabel(tt.labels); got != tt.want {
				t.Fatalf("hasTauLabel(%v) = %v, want %v", tt.labels, got, tt.want)
			}
		})
	}
}

func TestBoardIncludesExternalWorkloadsOnlyWhenRequested(t *testing.T) {
	reader := fakeReader{
		jobs: []byte(`{"items":[
			{"metadata":{"name":"tau-job","labels":{"` + workloadmeta.LabelJob + `":"tau-job"}},"status":{"active":1}},
			{"metadata":{"name":"external-job","labels":{"app":"trainer"}},"status":{"active":1}},
			{"metadata":{"name":"ray-owned","ownerReferences":[{"kind":"RayJob"}]},"status":{"active":1}}
		]}`),
		ray: []byte(`{"items":[
			{"metadata":{"name":"external-ray","labels":{"app":"ray"}},"status":{"jobDeploymentStatus":"Running"}}
		]}`),
	}

	defaultSnap, err := Board(context.Background(), reader, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(defaultSnap.Runs) != 1 || defaultSnap.Runs[0].Name != "tau-job" || defaultSnap.Runs[0].Source != "" {
		t.Fatalf("default runs = %+v", defaultSnap.Runs)
	}

	externalSnap, err := Board(context.Background(), reader, Options{IncludeExternal: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(externalSnap.Runs) != 3 {
		t.Fatalf("external runs = %+v", externalSnap.Runs)
	}
	sources := map[string]string{}
	for _, run := range externalSnap.Runs {
		sources[run.Name] = run.Source
	}
	if sources["tau-job"] != "tau" || sources["external-job"] != "external" || sources["external-ray"] != "external" {
		t.Fatalf("sources = %+v", sources)
	}
	if _, ok := sources["ray-owned"]; ok {
		t.Fatalf("RayJob-owned Job was not excluded: %+v", externalSnap.Runs)
	}
}

type fakeHistoryReader struct {
	rows  []Run
	err   error
	scope HistoryScope
	calls int
}

func (f *fakeHistoryReader) ListHistory(_ context.Context, scope HistoryScope) ([]Run, error) {
	f.calls++
	f.scope = scope
	return f.rows, f.err
}

func TestBoardMergesDurableHistoryWithLiveStateWinning(t *testing.T) {
	history := &fakeHistoryReader{rows: []Run{{
		Name: "train", Kind: "Job", Status: "failed",
		Created:   time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
		Namespace: "team-a", Cluster: "cluster-a", ResourceUID: "uid-a",
		RunID: "shared", DurableID: "durable-a", Queue: "queue-a",
	}, {
		Name: "deleted", Kind: "Job", Status: "succeeded",
		Created:   time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC),
		Namespace: "team-a", Cluster: "cluster-a", ResourceUID: "uid-deleted",
		RunID: "deleted", DurableID: "durable-deleted", Queue: "queue-a",
	}}}
	reader := fakeReader{
		jobs: []byte(`{"items":[{"metadata":{"name":"train","namespace":"team-a","uid":"uid-a","creationTimestamp":"2026-07-03T12:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"train","` + workloadmeta.LabelRunID + `":"shared","kueue.x-k8s.io/queue-name":"queue-a"},"annotations":{"` + workloadmeta.AnnotationDurableID + `":"durable-a"}},"status":{"active":1}}]}`),
		ray:  []byte(`{"items":[]}`),
	}
	snap, err := Board(context.Background(), reader, Options{
		Namespace: "team-a", Queue: "queue-a", History: history,
		HistoryScope: HistoryScope{Table: "RunHistory", Cluster: "cluster-a", WorkspaceID: "workspace-a", Limit: 50},
	})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.HistoryState != historyStateAvailable || snap.Total != 2 {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap.Runs[0].Name != "train" || snap.Runs[0].Status != "Running" || snap.Runs[0].DurableID != "durable-a" {
		t.Fatalf("live row did not win: %+v", snap.Runs[0])
	}
	if snap.Runs[1].Name != "deleted" || snap.Runs[1].Status != "succeeded" {
		t.Fatalf("terminal durable row missing: %+v", snap.Runs)
	}
	if history.scope.Namespace != "team-a" || history.scope.LocalQueue != "queue-a" ||
		history.scope.Cluster != "cluster-a" || history.scope.WorkspaceID != "workspace-a" {
		t.Fatalf("history scope = %+v", history.scope)
	}
}

func TestBoardHistoryKeepsSameRunIDInDistinctScopesIsolated(t *testing.T) {
	history := &fakeHistoryReader{rows: []Run{
		{Name: "alpha", Kind: "Job", Status: "succeeded", Created: time.Now(), Cluster: "cluster-a", Namespace: "alpha", RunID: "shared", Queue: "q-alpha"},
		{Name: "beta", Kind: "Job", Status: "failed", Created: time.Now(), Cluster: "cluster-a", Namespace: "beta", RunID: "shared", Queue: "q-beta"},
	}}
	snap, err := Board(context.Background(), fakeReader{jobs: []byte(`{"items":[]}`), ray: []byte(`{"items":[]}`)}, Options{
		Namespace: "alpha", Queue: "q-alpha", History: history,
		HistoryScope: HistoryScope{Cluster: "cluster-a", WorkspaceID: "alpha-workspace"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Runs) != 1 || snap.Runs[0].Name != "alpha" {
		t.Fatalf("scoped rows = %+v", snap.Runs)
	}
}

func TestBoardHistoryUnavailableRetainsLiveRows(t *testing.T) {
	history := &fakeHistoryReader{err: errors.New("credentials=secret")}
	snap, err := Board(context.Background(), fakeReader{
		jobs: []byte(`{"items":[{"metadata":{"name":"live","labels":{"` + workloadmeta.LabelJob + `":"live"}},"status":{"active":1}}]}`),
		ray:  []byte(`{"items":[]}`),
	}, Options{History: history})
	if err != nil {
		t.Fatal(err)
	}
	if snap.HistoryState != historyStateUnavailable || snap.Total != 1 || snap.Runs[0].Name != "live" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if strings.Contains(snap.HistoryDiagnostic, "secret") {
		t.Fatalf("history diagnostic leaked backend error: %q", snap.HistoryDiagnostic)
	}
}

func TestBoardWithoutHistoryIsExplicitlyLiveOnly(t *testing.T) {
	snap, err := Board(context.Background(), fakeReader{jobs: []byte(`{"items":[]}`), ray: []byte(`{"items":[]}`)}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.HistoryState != historyStateLiveOnly {
		t.Fatalf("history state = %q", snap.HistoryState)
	}
}

func TestBoardClassifiesTrackingAgainstExperimentSurface(t *testing.T) {
	tests := []struct {
		name        string
		annotations string
		surface     ExperimentSurfaceState
		want        string
	}{
		{
			name:        "exact workload evidence stays tracked",
			annotations: `,"annotations":{"` + workloadmeta.AnnotationExperimentSource + `":"stellar","` + workloadmeta.AnnotationMetricsSession + `":"session-1"}`,
			surface:     ExperimentSurfaceUnavailable,
			want:        experimentTrackingTracked,
		},
		{
			name:    "configured surface is available without claiming per-run evidence",
			surface: ExperimentSurfaceAvailable,
			want:    experimentTrackingAvailable,
		},
		{
			name:    "no configured surface is truly untracked",
			surface: ExperimentSurfaceUnconfigured,
			want:    experimentTrackingUntracked,
		},
		{
			name:    "configured but unusable surface is unavailable",
			surface: ExperimentSurfaceUnavailable,
			want:    experimentTrackingUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rayJSON := []byte(`{"items":[{"metadata":{"name":"run","namespace":"team-a","labels":{"` + workloadmeta.LabelRunID + `":"run-1"}` +
				tt.annotations + `},"status":{"jobDeploymentStatus":"Running"}}]}`)
			snap, err := Board(context.Background(), fakeReader{
				jobs: []byte(`{"items":[]}`),
				ray:  rayJSON,
			}, Options{Namespace: "team-a", ExperimentSurface: tt.surface})
			if err != nil {
				t.Fatal(err)
			}
			if len(snap.Runs) != 1 || snap.Runs[0].ExperimentTracking != tt.want {
				t.Fatalf("runs = %+v, want experimentTracking=%q", snap.Runs, tt.want)
			}
		})
	}
}

func TestMergeHistoryPromotesExactTrackedEvidence(t *testing.T) {
	live := []Run{{
		Name: "train", Kind: "RayJob", Cluster: "cluster-a", Namespace: "team-a", ResourceUID: "uid-1", RunID: "run-1",
		ExperimentTracking: experimentTrackingAvailable,
	}}
	history := []Run{{
		Name: "train", Kind: "RayJob", Cluster: "cluster-a", Namespace: "team-a", ResourceUID: "uid-1", RunID: "run-1",
		ExperimentTracking: experimentTrackingTracked,
	}}
	merged := mergeHistory(live, history)
	if len(merged) != 1 || merged[0].ExperimentTracking != experimentTrackingTracked {
		t.Fatalf("merged = %+v, want exact durable tracking evidence to win", merged)
	}
}

func TestMergeHistoryDoesNotPromoteTrackingFromReusedRunID(t *testing.T) {
	live := []Run{{
		Name: "train", Kind: "RayJob", Cluster: "cluster-a", Namespace: "team-a", ResourceUID: "uid-new", RunID: "run-1",
		ExperimentTracking: experimentTrackingAvailable,
	}}
	history := []Run{{
		Name: "train", Kind: "RayJob", Cluster: "cluster-a", Namespace: "team-a", ResourceUID: "uid-old", RunID: "run-1",
		ExperimentTracking: experimentTrackingTracked,
	}}
	merged := mergeHistory(live, history)
	if len(merged) != 1 || merged[0].ExperimentTracking != experimentTrackingAvailable {
		t.Fatalf("merged = %+v, want reused run ID to retain live tracking state", merged)
	}
}

func TestMergeHistoryDoesNotCollapseEmptyOrCollidingRunIDs(t *testing.T) {
	live := []Run{
		{Name: "live-empty", Kind: "Job", Namespace: "team-a", Cluster: "cluster-a"},
		{Name: "live-shared", Kind: "Job", Namespace: "team-a", Cluster: "cluster-a", RunID: "shared"},
	}
	durable := []Run{
		{Name: "terminal-empty", Kind: "Job", Namespace: "team-a", Cluster: "cluster-a"},
		{Name: "other-namespace", Kind: "Job", Namespace: "team-b", Cluster: "cluster-a", RunID: "shared"},
	}
	merged := mergeHistory(live, durable)
	if len(merged) != 4 {
		t.Fatalf("merged = %+v, want all rows retained", merged)
	}
}

func TestMergeHistoryKeepsDeletedRunWhenNameIsReused(t *testing.T) {
	live := []Run{{
		Name: "train", Kind: "Job", Status: "Running",
		Namespace: "team-a", Cluster: "cluster-a", ResourceUID: "uid-new", RunID: "run-new",
	}}
	durable := []Run{{
		Name: "train", Kind: "Job", Status: "Succeeded",
		Namespace: "team-a", Cluster: "cluster-a", ResourceUID: "uid-old", RunID: "run-old",
		DurableID: "cluster-a/team-a/uid-old",
	}}
	merged := mergeHistory(live, durable)
	if len(merged) != 2 {
		t.Fatalf("merged = %+v, want distinct live and deleted runs", merged)
	}
}

type fakeKustoQuerier struct {
	rows []kustoquery.Row
	kql  string
}

func (q *fakeKustoQuerier) Query(_ context.Context, kql string) ([]kustoquery.Row, error) {
	q.kql = kql
	return q.rows, nil
}

func TestKustoHistoryReaderParsesLifecycleRows(t *testing.T) {
	querier := &fakeKustoQuerier{rows: []kustoquery.Row{{
		"owning_resource_name": "terminal", "owning_resource_kind": "Job",
		"state": "succeeded", "created_time": "2026-07-01T12:00:00Z",
		"namespace": "team-a", "cluster": "cluster-a", "resource_uid": "uid-a",
		"run_id": "run-a", "durable_id": "durable-a", "experiment_tracking": "tracked",
	}}}
	reader := KustoHistoryReader{
		Querier: querier,
		QueryBuilder: func(scope HistoryScope) (string, error) {
			if scope.Namespace != "team-a" || scope.WorkspaceID != "workspace-a" {
				t.Fatalf("builder scope = %+v", scope)
			}
			return "RunHistory | take 1", nil
		},
	}
	rows, err := reader.ListHistory(context.Background(), HistoryScope{Namespace: "team-a", WorkspaceID: "workspace-a"})
	if err != nil {
		t.Fatal(err)
	}
	if querier.kql != "RunHistory | take 1" || len(rows) != 1 || rows[0].DurableID != "durable-a" || rows[0].Status != "Succeeded" || rows[0].ExperimentTracking != "tracked" {
		t.Fatalf("rows = %+v kql=%q", rows, querier.kql)
	}
}

func TestNormalizeHistoryStatusMatchesLiveDomain(t *testing.T) {
	for input, want := range map[string]string{
		"submitted": "Pending", "queued": "Pending", "admitted": "Pending",
		"running": "Running", "succeeded": "Succeeded", "failed": "Failed",
		"cancelled": "Cancelled", "stale": "Stale",
	} {
		if got := normalizeHistoryStatus(input); got != want {
			t.Errorf("normalizeHistoryStatus(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestNewKustoHistoryReaderBuildsScopedLifecycleQuery(t *testing.T) {
	querier := &fakeKustoQuerier{}
	reader := NewKustoHistoryReader(querier)
	_, err := reader.ListHistory(context.Background(), HistoryScope{
		Table: "TauExpRunLifecycle", Cluster: "cluster-a", Namespace: "team-a",
		LocalQueue: "team-a-queue", WorkspaceID: "workspace-a", Limit: 25,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"cluster == 'cluster-a'", "namespace == 'team-a'",
		"local_queue == 'team-a-queue'", "workspace_id == 'workspace-a'", "| take 25",
	} {
		if !strings.Contains(querier.kql, want) {
			t.Fatalf("query missing %q:\n%s", want, querier.kql)
		}
	}
}
func TestOwnedByRayJob(t *testing.T) {
	tests := []struct {
		name   string
		owners []ownerRef
		want   bool
	}{
		{"rayjob owner", []ownerRef{{Kind: "RayJob"}}, true},
		{"non-rayjob owner", []ownerRef{{Kind: "CronJob"}}, false},
		{"empty owners", []ownerRef{}, false},
		{"nil owners", nil, false},
		{"multiple owners with rayjob", []ownerRef{{Kind: "ReplicaSet"}, {Kind: "RayJob"}}, true},
		{"multiple owners without rayjob", []ownerRef{{Kind: "ReplicaSet"}, {Kind: "CronJob"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ownedByRayJob(tt.owners); got != tt.want {
				t.Fatalf("ownedByRayJob() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestJobStatus(t *testing.T) {
	cond := func(typ, status string) jobCondition {
		return jobCondition{Type: typ, Status: status}
	}
	tests := []struct {
		name       string
		conditions []jobCondition
		active     int
		succeeded  int
		failed     int
		want       string
	}{
		{"failed condition", []jobCondition{cond("Failed", "True")}, 0, 0, 1, "Failed"},
		{"complete condition", []jobCondition{cond("Complete", "True")}, 0, 1, 0, "Complete"},
		{"suspended condition", []jobCondition{cond("Suspended", "True")}, 0, 0, 0, "Suspended"},
		{"failed takes precedence over complete", []jobCondition{cond("Failed", "True"), cond("Complete", "True")}, 0, 0, 1, "Failed"},
		{"condition not true is ignored", []jobCondition{cond("Complete", "False")}, 1, 0, 0, "Running"},
		{"active pods running", []jobCondition{}, 2, 0, 0, "Running"},
		{"no conditions no active", []jobCondition{}, 0, 0, 0, "Pending"},
		{"nil conditions no active", nil, 0, 0, 0, "Pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jobStatus(tt.conditions, tt.active, tt.succeeded, tt.failed); got != tt.want {
				t.Fatalf("jobStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRayJobStatus(t *testing.T) {
	tests := []struct {
		name             string
		deploymentStatus string
		jobStatus        string
		want             string
	}{
		{"deployment status set", "Running", "", "Running"},
		{"fallback to job status", "", "SUCCEEDED", "SUCCEEDED"},
		{"deployment status takes precedence", "Initializing", "FAILED", "Initializing"},
		{"both empty", "", "", "Pending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rayJobStatus(tt.deploymentStatus, tt.jobStatus); got != tt.want {
				t.Fatalf("rayJobStatus(%q, %q) = %q, want %q", tt.deploymentStatus, tt.jobStatus, got, tt.want)
			}
		})
	}
}

func TestFormatAge(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		created time.Time
		want    string
	}{
		{"zero time", time.Time{}, "<unknown>"},
		{"30 seconds ago", now.Add(-30 * time.Second), "30s"},
		{"59 seconds ago", now.Add(-59 * time.Second), "59s"},
		{"1 minute ago", now.Add(-time.Minute), "1m"},
		{"45 minutes ago", now.Add(-45 * time.Minute), "45m"},
		{"1 hour ago", now.Add(-time.Hour), "1h"},
		{"2 hours 30 minutes ago", now.Add(-150 * time.Minute), "2h"},
		{"23 hours ago", now.Add(-23 * time.Hour), "23h"},
		{"1 day ago", now.Add(-24 * time.Hour), "1d"},
		{"3 days ago", now.Add(-72 * time.Hour), "3d"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := FormatAge(now, tt.created); got != tt.want {
				t.Fatalf("FormatAge() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestAggregate exercises the whole fold: Tau-label filtering, RayJob-owned Job
// exclusion, status mapping across both kinds, and newest-first ordering.
func TestAggregate(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	jobsJSON := []byte(`{"items":[
		{"metadata":{"name":"tau-job","creationTimestamp":"2026-07-02T10:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"train"}},
		 "status":{"active":1}},
		{"metadata":{"name":"not-tau","creationTimestamp":"2026-07-02T11:00:00Z","labels":{"app":"foo"}},
		 "status":{"active":1}},
		{"metadata":{"name":"rayjob-submitter","creationTimestamp":"2026-07-02T11:30:00Z","labels":{"` + workloadmeta.LabelRun + `":"r1"},"ownerReferences":[{"kind":"RayJob"}]},
		 "status":{"active":1}},
		{"metadata":{"name":"tau-complete","creationTimestamp":"2026-07-02T09:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"eval"}},
		 "status":{"conditions":[{"type":"Complete","status":"True"}],"succeeded":1}}
	]}`)
	rayJSON := []byte(`{"items":[
		{"metadata":{"name":"tau-rayjob","creationTimestamp":"2026-07-02T11:45:00Z","labels":{"` + workloadmeta.LabelRun + `":"r2"}},
		 "status":{"jobDeploymentStatus":"Running"}},
		{"metadata":{"name":"not-tau-ray","creationTimestamp":"2026-07-02T11:50:00Z","labels":{}},
		 "status":{"jobDeploymentStatus":"Running"}}
	]}`)

	snap := aggregate(now, jobsJSON, rayJSON)

	// Expect 3 kept: tau-job, tau-complete (Jobs), tau-rayjob (RayJob).
	// Excluded: not-tau (no label), rayjob-submitter (RayJob-owned), not-tau-ray (no label).
	if snap.Total != 3 {
		t.Fatalf("Total = %d, want 3", snap.Total)
	}

	// Newest first: tau-rayjob (11:45) > tau-job (10:00) > tau-complete (09:00).
	wantOrder := []struct{ name, kind, status string }{
		{"tau-rayjob", "RayJob", "Running"},
		{"tau-job", "Job", "Running"},
		{"tau-complete", "Job", "Complete"},
	}
	for i, w := range wantOrder {
		if snap.Runs[i].Name != w.name || snap.Runs[i].Kind != w.kind || snap.Runs[i].Status != w.status {
			t.Fatalf("Runs[%d] = %+v, want name=%s kind=%s status=%s", i, snap.Runs[i], w.name, w.kind, w.status)
		}
	}
	if snap.Runs[0].Age != "15m" {
		t.Fatalf("Runs[0].Age = %q, want 15m", snap.Runs[0].Age)
	}
	for _, run := range snap.Runs {
		if run.ExperimentTracking != experimentTrackingUntracked || run.ExperimentPath != "" {
			t.Fatalf("run tracking = %+v, want explicit untracked state without fabricated Stellar link", run)
		}
	}
	if snap.Runs[0].RunID != "r2" {
		t.Fatalf("RayJob RunID = %q, want legacy run label r2", snap.Runs[0].RunID)
	}
}

func TestAggregateDerivesLiveTrackingFromWorkloadEvidence(t *testing.T) {
	now := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	jobsJSON := []byte(`{"items":[{
		"metadata":{
			"name":"tracked-job",
			"creationTimestamp":"2026-07-21T11:00:00Z",
			"labels":{"` + workloadmeta.LabelRunID + `":"tracked-job"},
			"annotations":{"` + workloadmeta.AnnotationExperimentSource + `":"stellar","` + workloadmeta.AnnotationMetricsSession + `":"job-session"}
		},
		"status":{"active":1}
	}]}`)
	rayJSON := []byte(`{"items":[{
		"metadata":{
			"name":"tracked-ray",
			"creationTimestamp":"2026-07-21T11:30:00Z",
			"labels":{"` + workloadmeta.LabelRunID + `":"tracked-ray"},
			"annotations":{"` + workloadmeta.AnnotationExperimentSource + `":"stellar","` + workloadmeta.AnnotationMetricsSession + `":"ray-session"}
		},
		"status":{"jobDeploymentStatus":"Running"}
	}]}`)
	snap := aggregate(now, jobsJSON, rayJSON)
	if len(snap.Runs) != 2 {
		t.Fatalf("runs = %+v", snap.Runs)
	}
	for _, run := range snap.Runs {
		if run.ExperimentTracking != experimentTrackingTracked {
			t.Fatalf("%s tracking = %q, want tracked", run.Name, run.ExperimentTracking)
		}
	}

	missingSession := []byte(`{"items":[{
		"metadata":{"name":"not-yet-tracked","labels":{"` + workloadmeta.LabelRunID + `":"not-yet-tracked"},"annotations":{"` + workloadmeta.AnnotationExperimentSource + `":"stellar"}},
		"status":{"jobDeploymentStatus":"Running"}
	}]}`)
	untracked := aggregate(now, nil, missingSession)
	if untracked.Runs[0].ExperimentTracking != experimentTrackingUntracked {
		t.Fatalf("source-only RayJob tracking = %q, want untracked without a metrics session", untracked.Runs[0].ExperimentTracking)
	}
}

// TestAggregateToleratesInvalidJSON confirms a source that returns non-JSON
// (e.g. a "CRD not found" message) contributes no rows instead of failing.
func TestAggregateToleratesInvalidJSON(t *testing.T) {
	now := time.Now()
	jobsJSON := []byte(`{"items":[{"metadata":{"name":"tau-job","labels":{"` + workloadmeta.LabelJob + `":"t"}},"status":{"active":1}}]}`)
	snap := aggregate(now, jobsJSON, []byte("error: the server doesn't have a resource type \"rayjobs\""))
	if snap.Total != 1 || snap.Runs[0].Name != "tau-job" {
		t.Fatalf("aggregate with invalid ray JSON = %+v, want just tau-job", snap)
	}
}

// fakeReader drives Board() without a live API.
type fakeReader struct {
	jobs   []byte
	ray    []byte
	jobErr error
	rayErr error
}

func (f fakeReader) ListJobs(context.Context, string) ([]byte, error) {
	return f.jobs, f.jobErr
}

func (f fakeReader) ListRayJobs(context.Context, string) ([]byte, error) {
	return f.ray, f.rayErr
}

func TestBoardSetsNamespaceAndAggregates(t *testing.T) {
	r := fakeReader{
		jobs: []byte(`{"items":[{"metadata":{"name":"j","creationTimestamp":"2026-07-02T10:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"t"}},"status":{"active":1}}]}`),
		ray:  []byte(`{"items":[]}`),
	}
	snap, err := Board(context.Background(), r, Options{Namespace: "ray"})
	if err != nil {
		t.Fatalf("Board() error = %v", err)
	}
	if snap.Namespace != "ray" {
		t.Fatalf("Namespace = %q, want ray", snap.Namespace)
	}
	if snap.Total != 1 || snap.Runs[0].Name != "j" {
		t.Fatalf("snap = %+v, want one run j", snap)
	}
}

// TestBoardDropsFailedSource confirms one failing lister (missing RayJob CRD)
// still yields the other source's rows without an error.
func TestBoardDropsFailedSource(t *testing.T) {
	r := fakeReader{
		jobs:   []byte(`{"items":[{"metadata":{"name":"j","creationTimestamp":"2026-07-02T10:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"t"}},"status":{"active":1}}]}`),
		rayErr: errors.New("rayjobs.ray.io not found"),
	}
	snap, err := Board(context.Background(), r, Options{})
	if err != nil {
		t.Fatalf("Board() error = %v, want nil (one source is enough)", err)
	}
	if snap.Total != 1 || snap.Runs[0].Name != "j" {
		t.Fatalf("snap = %+v, want one run j", snap)
	}
}

// TestBoardErrorsWhenBothSourcesFail confirms a real outage (no Kubernetes)
// surfaces as an error so the handler can return 502.
func TestBoardErrorsWhenBothSourcesFail(t *testing.T) {
	r := fakeReader{
		jobErr: errors.New("connection refused"),
		rayErr: errors.New("connection refused"),
	}
	_, err := Board(context.Background(), r, Options{})
	if err == nil {
		t.Fatal("Board() error = nil, want error when both sources fail")
	}
}

func TestBoardServesDurableHistoryWhenBothLiveSourcesFail(t *testing.T) {
	history := &fakeHistoryReader{rows: []Run{{
		Name: "deleted", Kind: "Job", Status: "succeeded",
		Cluster: "cluster-a", Namespace: "team-a", DurableID: "cluster-a/team-a/uid-deleted",
	}}}
	snap, err := Board(context.Background(), fakeReader{
		jobErr: errors.New("connection refused"),
		rayErr: errors.New("connection refused"),
	}, Options{
		Namespace: "team-a",
		History:   history,
		HistoryScope: HistoryScope{
			Cluster: "cluster-a", Namespace: "team-a",
		},
	})
	if err != nil {
		t.Fatalf("Board() error = %v, want durable history fallback", err)
	}
	if snap.HistoryState != historyStateAvailable || snap.Total != 1 || snap.Runs[0].Name != "deleted" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

func TestBoardFiltersResolvedLocalQueue(t *testing.T) {
	jobsJSON := []byte(`{"items":[
		{"metadata":{"name":"alpha","creationTimestamp":"2026-07-02T10:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"alpha","kueue.x-k8s.io/queue-name":"alpha-queue"}}},
		{"metadata":{"name":"beta","creationTimestamp":"2026-07-02T11:00:00Z","labels":{"` + workloadmeta.LabelJob + `":"beta","kueue.x-k8s.io/queue-name":"beta-queue"}}}
	]}`)
	r := fakeReader{jobs: jobsJSON, ray: []byte(`{"items":[]}`)}
	snap, err := Board(context.Background(), r, Options{Namespace: "shared", Queue: "alpha-queue"})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if snap.Total != 1 || snap.Runs[0].Name != "alpha" || snap.Runs[0].Queue != "alpha-queue" {
		t.Fatalf("queue-filtered snapshot = %+v", snap)
	}
}
