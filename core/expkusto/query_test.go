// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expkusto

import (
	"strings"
	"testing"
)

func TestBuildMetricsQueryScopesAndDownsamples(t *testing.T) {
	query, err := BuildMetricsQuery(MetricsQueryOptions{
		WorkspaceID:  "sample",
		Project:      "sample-project",
		RunGroupID:   "reference-group",
		RunIDs:       []string{"seed-1", "seed-2"},
		MetricNames:  []string{"train/return"},
		Since:        "24h",
		TargetPoints: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"TauExpMetrics",
		"let target_points = 10000",
		"| where exported_at > ago(24h)",
		"| where workspace_id == 'sample'",
		"| where ['project'] == 'sample-project'",
		"| where run_group_id == 'reference-group'",
		"| where run_id in ('seed-1', 'seed-2')",
		"| where metric_name in ('train/return')",
		"arg_max(exported_at, *)",
		"source_point_count=count()",
		"step_bucket=bin(step, step_bin)",
		"summarize arg_min(value, *)",
		"summarize arg_max(value, *)",
		"union endpoints, minima, maxima",
		"| project ['project'], experiment_id, run_group_id, run_id, metric_name, step, wall_time, value, unit, source, split, metric_file_id, metric_file_path",
		"order by run_group_id asc, run_id asc",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("query missing %q:\n%s", want, query)
		}
	}
}

func TestBuildMetricsQuerySupportsAdxMonRemoteWrite(t *testing.T) {
	query, err := BuildMetricsQuery(MetricsQueryOptions{
		WorkspaceID:  "sample",
		Project:      "sample-project",
		RunGroupID:   "reference-group",
		RunIDs:       []string{"seed-1"},
		MetricNames:  []string{"train/return"},
		Ingestion:    "remote-write",
		TargetPoints: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ExperimentMetrics",
		"| where Timestamp > ago(7d)",
		"cluster=tostring(Cluster)",
		"Labels.source_store_id",
		"Labels['project']",
		"Labels.metric_name",
		"Labels.metric_file_id",
		"Labels.metric_file_path",
		"Labels.workspace_id",
		"Value",
		"| where ['project'] == 'sample-project'",
		"| where workspace_id == 'sample'",
		"| where run_group_id == 'reference-group'",
		"| where run_id in ('seed-1')",
		"arg_max(exported_at, *)",
		"step_bucket=bin(step, step_bin)",
		"summarize arg_min(value, *)",
		"summarize arg_max(value, *)",
		"| project ['project'], experiment_id, run_group_id, run_id, metric_name, step, wall_time, value, unit, source, split, metric_file_id, metric_file_path, source_store_id, cluster",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("remote-write query missing %q:\n%s", want, query)
		}
	}
}

func TestBuildMetricsQueryScopesRangeBeforeExtremaPreselection(t *testing.T) {
	start, end := int64(100), int64(900)
	query, err := BuildMetricsQuery(MetricsQueryOptions{
		Project:                     "sample-project",
		Target:                      "sample-project-wandb-migration",
		MetricNames:                 []string{"train/return"},
		StartStep:                   &start,
		EndStep:                     &end,
		TargetPoints:                1200,
		IncludeValidationMilestones: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| where step >= 100",
		"| where step <= 900",
		"let milestone_steps = scoped",
		"metric_name startswith 'eval/'",
		"union endpoints, minima, maxima, milestones",
		"validation_milestone=coalesce(validation_milestone, false)",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("range/milestone query missing %q:\n%s", want, query)
		}
	}
	if strings.Index(query, "| where step <= 900") > strings.Index(query, "let bucketed = deduped") {
		t.Fatalf("step range must be applied before extrema preselection:\n%s", query)
	}
}

func TestBuildMetricsQueryScopesMilestonesToFullSeriesIdentity(t *testing.T) {
	query, err := BuildMetricsQuery(MetricsQueryOptions{
		Projects:                    []string{"project-a", "project-b"},
		MetricNames:                 []string{"train/loss"},
		TargetPoints:                1200,
		IncludeValidationMilestones: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	identity := "source_store_id, metric_file_id, ['project'], experiment_id, run_group_id, run_id, workspace_id, step"
	for _, want := range []string{
		"| distinct " + identity,
		"| join kind=inner milestone_steps on " + identity,
		"| join kind=leftouter milestone_steps on " + identity,
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("milestone query must preserve full series identity %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "milestone_steps on run_id, step") {
		t.Fatalf("milestone query must not join reused run IDs across project/workspace scopes:\n%s", query)
	}
}

func TestBuildMetricsQueryRawRowsSkipDisplayAggregation(t *testing.T) {
	query, err := BuildMetricsQuery(MetricsQueryOptions{
		Project:      "sample-project",
		RunIDs:       []string{"seed-1"},
		MetricNames:  []string{"train/return"},
		TargetPoints: 1200,
		Raw:          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"deduped", "point_count=1", "source_point_count=1"} {
		if !strings.Contains(query, want) {
			t.Fatalf("raw query missing %q:\n%s", want, query)
		}
	}
	for _, unwanted := range []string{"let bucketed", "arg_min(value", "arg_max(value"} {
		if strings.Contains(query, unwanted) {
			t.Fatalf("raw query must not contain display aggregation %q:\n%s", unwanted, query)
		}
	}
}

func TestBuildMetricsQueryRejectsReversedRange(t *testing.T) {
	start, end := int64(10), int64(9)
	if _, err := BuildMetricsQuery(MetricsQueryOptions{StartStep: &start, EndStep: &end}); err == nil {
		t.Fatal("expected reversed step range to fail")
	}
}

func TestBuildMetricsQuerySupportsAutoTargetFilter(t *testing.T) {
	query, err := BuildMetricsQuery(MetricsQueryOptions{
		Project:      "sample-project",
		Target:       "sample-project-wandb-migration",
		TargetType:   "auto",
		MetricNames:  []string{"train/return"},
		TargetPoints: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The filter runs before the projection that aliases the experiment column,
	// so it must match the raw column. Both spellings are matched: experiment_id
	// is current, question_id is what stores written before the rename carry.
	// Matching only one silently returns nothing rather than erroring.
	want := "| where (column_ifexists('experiment_id', '') == 'sample-project-wandb-migration' or column_ifexists('question_id', '') == 'sample-project-wandb-migration') or run_group_id == 'sample-project-wandb-migration' or run_id == 'sample-project-wandb-migration'"
	if !strings.Contains(query, want) {
		t.Fatalf("query missing auto target filter %q:\n%s", want, query)
	}
}

// The experiment filter is the one that silently degrades: an unmatched column
// yields an empty chart, not an error, so pin the experiment-scoped form too.
func TestBuildMetricsQueryExperimentFilterMatchesBothSpellings(t *testing.T) {
	query, err := BuildMetricsQuery(MetricsQueryOptions{
		Project:      "sample-project",
		Target:       "experiment-alpha",
		TargetType:   "experiment",
		MetricNames:  []string{"train/return"},
		TargetPoints: 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "| where (column_ifexists('experiment_id', '') == 'experiment-alpha' or column_ifexists('question_id', '') == 'experiment-alpha')"
	if !strings.Contains(query, want) {
		t.Fatalf("query missing experiment target filter %q:\n%s", want, query)
	}
	// The projection must prefer the current column, or a store carrying both
	// would resolve to the stale value.
	if !strings.Contains(query, "experiment_id=experimentIdOf") {
		t.Fatalf("query does not alias the resolved experiment column:\n%s", query)
	}
}

func TestBuildExperimentSearchQueryRollsUpMetricRows(t *testing.T) {
	query, err := BuildExperimentSearchQuery(MetricsQueryOptions{
		Project:     "sample-project",
		MetricNames: []string{"train/return"},
		Since:       "30d",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"TauExpMetrics",
		"| where wall_time > ago(30d)",
		"| where project_id == 'sample-project'",
		"| where metric_name in ('train/return')",
		"| summarize latest_wall_time=max(wall_time) by project_id, experiment_id, workspace_id",
		"| top 201 by latest_wall_time desc",
		"arg_max(wall_time, *) by project_id, experiment_id, run_group_id, run_id, metric_name",
		"| project ['project']=project_id, experiment_id, run_group_id, run_id, metric_name, step, wall_time, value, unit, source, split, metric_file_id, metric_file_path, source_store_id, cluster, tags",
		"order by wall_time desc, project_id asc,",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("experiment search query missing %q:\n%s", want, query)
		}
	}
	for _, unwanted := range []string{"let target_points", "step=bin(step, step_bin)", "point_count=count()"} {
		if strings.Contains(query, unwanted) {
			t.Fatalf("experiment search query should not include dashboard rollup %q:\n%s", unwanted, query)
		}
	}
}

func TestBuildExperimentSearchQuerySupportsRemoteWrite(t *testing.T) {
	query, err := BuildExperimentSearchQuery(MetricsQueryOptions{
		Project:   "sample-project",
		Ingestion: "remote-write",
		Since:     "24h",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ExperimentMetrics",
		"| where Timestamp > ago(24h)",
		"cluster=tostring(Cluster)",
		"Labels['project']",
		"experiment_id=coalesce(tostring(Labels.experiment_id), tostring(Labels.question_id), '')",
		"| where project_id == 'sample-project'",
		"| top 201 by latest_wall_time desc",
		"arg_max(wall_time, *) by project_id, experiment_id, run_group_id, run_id, metric_name",
		"| project ['project']=project_id, experiment_id, run_group_id, run_id, metric_name",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("remote-write experiment search query missing %q:\n%s", want, query)
		}
	}
}

func TestBuildRunLifecycleQueryJoinsLifecycleWithRemoteWriteMetrics(t *testing.T) {
	query, err := BuildRunLifecycleQuery(RunLifecycleQueryOptions{
		WorkspaceID: "sample",
		Project:     "sample-project",
		Target:      "seed-1",
		TargetType:  "run",
		Ingestion:   "remote-write",
		Since:       "24h",
		StaleAfter:  "20m",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"let stale_after = 20m",
		"TauExpRunLifecycleDashboardRows()",
		"| where observed_at > ago(24h)",
		"| where ['project'] == 'sample-project'",
		"| where workspace_id == 'sample'",
		"| where run_id == 'seed-1'",
		"ExperimentMetrics",
		"| where Timestamp > ago(24h)",
		"Labels['project']",
		"Labels.workspace_id",
		"on project_id, run_group_id, run_id, workspace_id",
		"metric_latest_time=max(wall_time)",
		"join kind=leftouter metric_rows",
		"latest_activity_time=coalesce(latest_metric_time, pod_start_time, kueue_admitted_time, created_time, observed_at)",
		"effective_state=case",
		"'stale'",
		"artifact_uri, checkpoint_uri",
		"outcome_state=iff(state in ('succeeded', 'failed', 'cancelled')",
		"liveness_state=case",
		"'not_responding'",
		"latest_evidence_time=latest_activity_time",
		"workload_absence_confirmed=false",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("run lifecycle query missing %q:\n%s", want, query)
		}
	}
}

func TestBuildRunHistoryQueryScopesBeforeDedupe(t *testing.T) {
	query, err := BuildRunHistoryQuery(RunHistoryQueryOptions{
		Table:       "TauExpRunLifecycle",
		Cluster:     "west'us",
		Namespace:   "research",
		LocalQueue:  "gpu-priority",
		WorkspaceID: "sample",
		Kind:        "RayJob",
		Limit:       25,
	})
	if err != nil {
		t.Fatal(err)
	}
	const want = `let scoped = materialize(
TauExpRunLifecycle
| where cluster == 'west''us'
| where namespace == 'research'
| where local_queue == 'gpu-priority'
| where workspace_id == 'sample'
| where tolower(owning_resource_kind) == 'rayjob'
| extend durable_identity=iff(isnotempty(durable_id), durable_id, iff(isnotempty(resource_uid), resource_uid, run_id))
| where isnotempty(durable_identity)
| extend observation_identity=iff(isnotempty(observation_id), observation_id, strcat(durable_identity, ':', state, ':', tostring(observed_at)))
| summarize arg_max(observed_at, *) by cluster, namespace, durable_identity, observation_identity
);
let latest_by_state = scoped
| extend is_terminal=tolower(state) in ('succeeded', 'failed', 'cancelled')
| summarize arg_max(observed_at, *) by cluster, namespace, durable_identity, is_terminal;
latest_by_state
| extend terminal_rank=iff(is_terminal, 1, 0)
| summarize arg_max(terminal_rank, *) by cluster, namespace, durable_identity
| project observed_at, observation_id, run_id, durable_id=durable_identity, workspace_id, result_scope, ['project'], run_group_id, tags, owning_resource_kind, owning_resource_name, namespace, cluster, local_queue, cluster_queue, workload_kind, resource_uid, resource_version, generation, submit_time, created_time, kueue_admitted_time, pod_start_time, first_metric_time, latest_metric_time, completion_time, state, reason, message, artifact_uri, checkpoint_uri, image, image_digest, config_hash, code_sha, tau_command, result_path, result_pvc, experiment_tracking, experiment_source, controller_version
| order by observed_at desc, cluster asc, namespace asc, durable_id asc
| take 25
`
	if query != want {
		t.Fatalf("run history query changed:\nwant:\n%s\ngot:\n%s", want, query)
	}
}

func TestBuildRunHistoryTimelineQueryLimitsNewestEventsThenRestoresDisplayOrder(t *testing.T) {
	query, err := BuildRunHistoryTimelineQuery(RunHistoryQueryOptions{Kind: "RayJob", Limit: 25}, "uid-1")
	if err != nil {
		t.Fatal(err)
	}
	kind := strings.Index(query, "| where tolower(owning_resource_kind) == 'rayjob'")
	resource := strings.Index(query, "| where resource_uid == 'uid-1'")
	desc := strings.Index(query, "| order by observed_at desc")
	take := strings.Index(query, "| take 25")
	asc := strings.LastIndex(query, "| order by observed_at asc")
	if kind < 0 || resource < 0 || desc < 0 || take < 0 || asc < 0 || !(kind < resource && resource < desc && desc < take && take < asc) {
		t.Fatalf("timeline query must filter kind before limiting newest events, then restore ascending order:\n%s", query)
	}
}

func TestBuildRunHistoryQueryKeepsIdenticalRunIDsDistinctAcrossScopes(t *testing.T) {
	query, err := BuildRunHistoryQuery(RunHistoryQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"durable_identity=iff(isnotempty(durable_id), durable_id, iff(isnotempty(resource_uid), resource_uid, run_id))",
		"| where isnotempty(durable_identity)",
		"| summarize arg_max(observed_at, *) by cluster, namespace, durable_identity, is_terminal",
		"durable_id=durable_identity",
		"| take 200",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("run history query missing %q:\n%s", want, query)
		}
	}
	if strings.Contains(query, "by run_id") {
		t.Fatalf("run history must not globally dedupe run_id:\n%s", query)
	}
}

func TestBuildRunHistoryQueryFallsBackWhenRunIDIsEmpty(t *testing.T) {
	query, err := BuildRunHistoryQuery(RunHistoryQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "durable_identity=iff(isnotempty(durable_id), durable_id, iff(isnotempty(resource_uid), resource_uid, run_id))"
	if !strings.Contains(query, want) {
		t.Fatalf("run history query must use durable_id and resource_uid before run_id:\n%s", query)
	}
}

func TestBuildRunHistoryQueryKeepsTerminalStateMonotonic(t *testing.T) {
	query, err := BuildRunHistoryQuery(RunHistoryQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"tolower(state) in ('succeeded', 'failed', 'cancelled')",
		"| summarize arg_max(observed_at, *) by cluster, namespace, durable_identity, is_terminal",
		"| extend terminal_rank=iff(is_terminal, 1, 0)",
		"| summarize arg_max(terminal_rank, *) by cluster, namespace, durable_identity",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("run history terminal monotonicity missing %q:\n%s", want, query)
		}
	}
}

func TestBuildRunHistoryQueryProjectsDurableMetadata(t *testing.T) {
	query, err := BuildRunHistoryQuery(RunHistoryQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		"workspace_id",
		"result_scope",
		"local_queue",
		"cluster_queue",
		"workload_kind",
		"image",
		"image_digest",
		"config_hash",
		"code_sha",
		"tau_command",
		"result_path",
		"result_pvc",
		"experiment_tracking",
		"experiment_source",
	} {
		if !strings.Contains(query, field) {
			t.Fatalf("run history query missing durable metadata field %q:\n%s", field, query)
		}
	}
}

func TestBuildRunHistoryQueryValidatesTableAndLimits(t *testing.T) {
	if _, err := BuildRunHistoryQuery(RunHistoryQueryOptions{Table: "bad; table"}); err == nil {
		t.Fatal("expected invalid table error")
	}
	if _, err := BuildRunHistoryQuery(RunHistoryQueryOptions{Limit: -1}); err == nil {
		t.Fatal("expected invalid limit error")
	}
	query, err := BuildRunHistoryQuery(RunHistoryQueryOptions{Limit: 1001})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, "| take 1000") {
		t.Fatalf("run history limit must be capped:\n%s", query)
	}
}

func TestBuildMetricsQueryAllowsUnscopedProject(t *testing.T) {
	query, err := BuildMetricsQuery(MetricsQueryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{
		"| where ['project'] ==",
		"| where project_id ==",
		"| where project_id in",
	} {
		if strings.Contains(query, unwanted) {
			t.Fatalf("unscoped query should not include project filter %q:\n%s", unwanted, query)
		}
	}
}

func TestBuildExperimentSearchQuerySupportsMultipleAllowedProjects(t *testing.T) {
	query, err := BuildExperimentSearchQuery(MetricsQueryOptions{
		Projects: []string{"vit-enc-vision", "tau-submit"},
		Limit:    50,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| where project_id in ('vit-enc-vision', 'tau-submit')",
		"| top 51 by latest_wall_time desc",
		"| summarize latest_wall_time=max(wall_time) by project_id",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("multi-project discovery query missing %q:\n%s", want, query)
		}
	}
}

func TestBuildExperimentSearchQueryWithEmptyProjectIsBounded(t *testing.T) {
	query, err := BuildExperimentSearchQuery(MetricsQueryOptions{Since: "90d", Limit: 25})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"| where wall_time > ago(90d)",
		"| summarize latest_wall_time=max(wall_time) by project_id",
		"| top 26 by latest_wall_time desc",
		"| join kind=inner (top_experiments) on project_id",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("unscoped discovery query missing %q:\n%s", want, query)
		}
	}
	for _, unwanted := range []string{
		"| where project_id ==",
		"| where ['project'] ==",
	} {
		if strings.Contains(query, unwanted) {
			t.Fatalf("unscoped discovery query should not include project filter %q:\n%s", unwanted, query)
		}
	}
}

func TestBuildSchemaKQLDocumentsDashboardContracts(t *testing.T) {
	projection := BuildProjectionDashboardSchemaKQL()
	for _, want := range []string{
		defaultProjectionDashboardFunction,
		DefaultProjectionTable,
		"metric_file_id",
		"experiment_id=experimentIdOf",
		"['project'], experiment_id=experimentIdOf, run_group_id, run_id, metric_name",
	} {
		if !strings.Contains(projection, want) {
			t.Fatalf("projection schema KQL missing %q:\n%s", want, projection)
		}
	}

	remoteWrite, err := BuildRemoteWriteSchemaKQL(SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		defaultRemoteWriteDashboardFunction,
		defaultRemoteWriteMetricName,
		DefaultRemoteWriteTable,
		"Timestamp: datetime, SeriesId: long, Labels: dynamic, Value: real",
		"Cluster: string",
		"source_store_id=tostring(Labels.source_store_id)",
		"metric_file_id=tostring(Labels.metric_file_id)",
		"experiment_id=coalesce(tostring(Labels.experiment_id), tostring(Labels.question_id), '')",
		"['project'], experiment_id, run_group_id, run_id, metric_name",
	} {
		if !strings.Contains(remoteWrite, want) {
			t.Fatalf("remote-write schema KQL missing %q:\n%s", want, remoteWrite)
		}
	}

	lifecycle, err := BuildRunLifecycleSchemaKQL(SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		DefaultRunLifecycleTable,
		defaultRunLifecycleDashboardFunction,
		RunLifecycleIngestionMappingName,
		".create-or-alter table TauExpRunLifecycle ingestion json mapping",
		"Metrics alone cannot answer queued/running/failed/stale states",
		"owning_resource_kind",
		"kueue_admitted_time",
		"first_metric_time",
		"latest_metric_time",
		"artifact_uri",
		"checkpoint_uri",
		"State values: submitted, queued, admitted, running, succeeded, failed, cancelled, stale",
		"durable_id: string",
		"workspace_id: string",
		"result_scope: string",
		"local_queue: string",
		"cluster_queue: string",
		"workload_kind: string",
		"image: string",
		"image_digest: string",
		"config_hash: string",
		"code_sha: string",
		"tau_command: string",
		"result_path: string",
		"result_pvc: string",
		"experiment_tracking: string",
		"experiment_source: string",
		"by cluster, namespace, durable_identity, is_terminal",
		"arg_max(terminal_rank, *) by cluster, namespace, durable_identity",
	} {
		if !strings.Contains(lifecycle, want) {
			t.Fatalf("run lifecycle schema KQL missing %q:\n%s", want, lifecycle)
		}
	}
}

func TestRunLifecycleIngestionMappingKQLIsSchemaOwnedAndIdempotent(t *testing.T) {
	mapping, err := BuildRunLifecycleIngestionMappingKQL(SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		".create-or-alter table TauExpRunLifecycle ingestion json mapping",
		RunLifecycleIngestionMappingName,
		`"column":"observed_at","datatype":"datetime","path":"$.observed_at"`,
		`"column":"tags","datatype":"dynamic","path":"$.tags"`,
		`"column":"generation","datatype":"long","path":"$.generation"`,
	} {
		if !strings.Contains(mapping, want) {
			t.Fatalf("lifecycle ingestion mapping missing %q:\n%s", want, mapping)
		}
	}
	if got := RunLifecycleIngestionMapping(); len(got) == 0 {
		t.Fatal("lifecycle ingestion mapping must not be empty")
	}

	if _, err := BuildRunLifecycleIngestionMappingKQL(SchemaOptions{RunLifecycleTable: "not-valid-name"}); err == nil {
		t.Fatal("invalid lifecycle table name was accepted")
	}
}

func TestKQLStringLiteralEscapesSingleQuotes(t *testing.T) {
	if got, want := kqlStringLiteral(`a'b`), `'a''b'`; got != want {
		t.Fatalf("KQL string literal = %q, want %q", got, want)
	}
}

func TestTelemetryNameConstantsPreserveKustoContracts(t *testing.T) {
	if DefaultDatabase != "Metrics" || DefaultRemoteWriteTable != "ExperimentMetrics" || defaultRemoteWriteDashboardFunction != "ExperimentMetricsDashboardRows" {
		t.Fatalf("hosted fleet contract = %s.%s -> %s(), want Metrics.ExperimentMetrics -> ExperimentMetricsDashboardRows()", DefaultDatabase, DefaultRemoteWriteTable, defaultRemoteWriteDashboardFunction)
	}
	if defaultRemoteWriteMetricName != "experiment_metrics" {
		t.Fatalf("remote-write metric name = %q, want experiment_metrics", defaultRemoteWriteMetricName)
	}
	if DefaultProjectionTable != "TauExpMetrics" || defaultProjectionDashboardFunction != "TauExpMetricsDashboardRows" {
		t.Fatalf("projection compatibility contract = %s -> %s()", DefaultProjectionTable, defaultProjectionDashboardFunction)
	}

	remoteWrite, err := BuildRemoteWriteSchemaKQL(SchemaOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{defaultRemoteWriteMetricName, DefaultRemoteWriteTable, defaultRemoteWriteDashboardFunction + "()"} {
		if !strings.Contains(remoteWrite, want) {
			t.Fatalf("remote-write schema missing %q:\n%s", want, remoteWrite)
		}
	}
	if strings.Contains(remoteWrite, DefaultProjectionTable) || strings.Contains(remoteWrite, defaultProjectionDashboardFunction) {
		t.Fatalf("remote-write schema must not depend on projection compatibility names:\n%s", remoteWrite)
	}

	projection := BuildProjectionDashboardSchemaKQL()
	for _, want := range []string{DefaultProjectionTable, defaultProjectionDashboardFunction + "()"} {
		if !strings.Contains(projection, want) {
			t.Fatalf("projection schema missing %q:\n%s", want, projection)
		}
	}
	if strings.Contains(projection, defaultRemoteWriteMetricName) {
		t.Fatalf("projection schema must not name hosted remote-write metric %q:\n%s", defaultRemoteWriteMetricName, projection)
	}
}
