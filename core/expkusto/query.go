// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expkusto

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const (
	DefaultEndpoint     = "https://example.kusto.windows.net"
	DefaultDatabase     = exptelemetry.RemoteWriteDatabase
	DefaultTargetPoints = 12000
	MinTargetPoints     = 100
)

type MetricsQueryOptions struct {
	WorkspaceID                 string
	Project                     string
	Projects                    []string
	Target                      string
	TargetType                  string
	RunGroupID                  string
	RunIDs                      []string
	MetricNames                 []string
	StartStep                   *int64
	EndStep                     *int64
	Since                       string
	Ingestion                   string
	TargetPoints                int
	Limit                       int
	Raw                         bool
	IncludeValidationMilestones bool
}

type MetricsQueryResult struct {
	Endpoint     string `json:"endpoint"`
	Database     string `json:"database"`
	Query        string `json:"query"`
	TargetPoints int    `json:"target_points"`
}

type RunLifecycleQueryOptions struct {
	WorkspaceID string
	Project     string
	Projects    []string
	Target      string
	TargetType  string
	RunGroupID  string
	RunIDs      []string
	Since       string
	Ingestion   string
	StaleAfter  string
}

type RunLifecycleQueryResult struct {
	Endpoint   string `json:"endpoint"`
	Database   string `json:"database"`
	Query      string `json:"query"`
	StaleAfter string `json:"stale_after"`
}

// RunHistoryQueryOptions scopes durable lifecycle rows for Portal and CLI readers.
// Empty scope fields intentionally do not filter that dimension.
type RunHistoryQueryOptions struct {
	Table       string
	Cluster     string
	Namespace   string
	LocalQueue  string
	WorkspaceID string
	Kind        string
	Limit       int
}

func BuildMetricsQuery(opts MetricsQueryOptions) (string, error) {
	opts.WorkspaceID = strings.TrimSpace(opts.WorkspaceID)
	opts.Project = strings.TrimSpace(opts.Project)
	projects := normalizedProjects(opts.Project, opts.Projects)
	opts.Target = strings.TrimSpace(opts.Target)
	opts.TargetType = strings.ToLower(strings.TrimSpace(opts.TargetType))
	opts.RunGroupID = strings.TrimSpace(opts.RunGroupID)
	opts.Since = strings.TrimSpace(opts.Since)
	opts.Ingestion = strings.ToLower(strings.TrimSpace(opts.Ingestion))
	if opts.TargetType == "" {
		opts.TargetType = "auto"
	}
	switch opts.TargetType {
	case "auto", "experiment", "run_group", "run":
	default:
		return "", fmt.Errorf("--target-type must be auto, experiment, run_group, or run")
	}
	if opts.Ingestion == "" {
		opts.Ingestion = "projection"
	}
	if opts.Since == "" {
		opts.Since = "7d"
	}
	if opts.Ingestion != "projection" && opts.Ingestion != "remote-write" {
		return "", fmt.Errorf("--ingestion must be projection or remote-write")
	}
	if opts.TargetPoints == 0 {
		opts.TargetPoints = DefaultTargetPoints
	}
	if opts.TargetPoints < MinTargetPoints {
		return "", fmt.Errorf("--target-points must be at least %d", MinTargetPoints)
	}
	if opts.StartStep != nil && opts.EndStep != nil && *opts.StartStep > *opts.EndStep {
		return "", fmt.Errorf("--start-step must be less than or equal to --end-step")
	}
	if opts.Ingestion == "remote-write" {
		return buildRemoteWriteMetricsQuery(opts), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "let target_points = %d;\n", opts.TargetPoints)
	b.WriteString("let scoped = materialize(\n")
	b.WriteString(DefaultProjectionTable + "\n")
	if opts.Since != "" {
		fmt.Fprintf(&b, "| where exported_at > ago(%s)\n", kqlDuration(opts.Since))
	}
	b.WriteString("| extend workspace_id=tostring(parse_json(tostring(tags))['tau_workspace'])\n")
	b.WriteString("| extend experimentIdOf=iff(isempty(column_ifexists('experiment_id', '')), column_ifexists('question_id', ''), column_ifexists('experiment_id', ''))\n")
	writeProjectFilter(&b, "['project']", projects)
	writeMetricScopeFilters(&b, opts)
	writeStepRangeFilters(&b, opts)
	b.WriteString("| project exported_at, cluster='', source_store_id, metric_file_id, metric_file_path, ['project'], experiment_id=experimentIdOf, run_group_id, run_id, metric_name, step=tolong(step), wall_time=todatetime(wall_time), value=todouble(value), unit, source, split, tags, workspace_id\n")
	b.WriteString("| where isnotnull(step) and isnotnull(value)\n")
	b.WriteString(");\n")
	writeRequestedAndMilestones(&b, opts)
	b.WriteString("let deduped = requested\n")
	b.WriteString("| summarize arg_max(exported_at, *) by source_store_id, metric_file_id, ['project'], experiment_id, run_group_id, run_id, metric_name, step, wall_time, workspace_id;\n")
	writeMetricsResult(&b, opts)
	writeDashboardProjection(&b)
	b.WriteString("| order by run_group_id asc, run_id asc, metric_name asc, step asc\n")
	return b.String(), nil
}

func BuildExperimentSearchQuery(opts MetricsQueryOptions) (string, error) {
	opts.WorkspaceID = strings.TrimSpace(opts.WorkspaceID)
	opts.Project = strings.TrimSpace(opts.Project)
	projects := normalizedProjects(opts.Project, opts.Projects)
	opts.Target = strings.TrimSpace(opts.Target)
	opts.TargetType = strings.ToLower(strings.TrimSpace(opts.TargetType))
	opts.RunGroupID = strings.TrimSpace(opts.RunGroupID)
	opts.Since = strings.TrimSpace(opts.Since)
	opts.Ingestion = strings.ToLower(strings.TrimSpace(opts.Ingestion))
	if opts.TargetType == "" {
		opts.TargetType = "auto"
	}
	switch opts.TargetType {
	case "auto", "experiment", "run_group", "run":
	default:
		return "", fmt.Errorf("--target-type must be auto, experiment, run_group, or run")
	}
	if opts.Ingestion == "" {
		opts.Ingestion = "projection"
	}
	if opts.Since == "" {
		opts.Since = "7d"
	}
	if opts.Ingestion != "projection" && opts.Ingestion != "remote-write" {
		return "", fmt.Errorf("--ingestion must be projection or remote-write")
	}
	if opts.Limit == 0 {
		opts.Limit = 200
	}
	if opts.Limit < 0 {
		return "", fmt.Errorf("--limit must be non-negative")
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	if opts.Ingestion == "remote-write" {
		return buildRemoteWriteExperimentSearchQuery(opts, projects), nil
	}
	var b strings.Builder
	b.WriteString("let scoped = materialize(\n")
	b.WriteString(DefaultProjectionTable + "\n")
	b.WriteString("| extend workspace_id=tostring(parse_json(tostring(tags))['tau_workspace'])\n")
	b.WriteString("| extend experimentIdOf=iff(isempty(column_ifexists('experiment_id', '')), column_ifexists('question_id', ''), column_ifexists('experiment_id', ''))\n")
	b.WriteString("| project exported_at, cluster='', source_store_id, metric_file_id, metric_file_path, project_id=tostring(['project']), experiment_id=experimentIdOf, run_group_id, run_id, metric_name, step=tolong(step), wall_time=todatetime(wall_time), value=todouble(value), unit, source, split, tags, workspace_id\n")
	writeProjectFilter(&b, "project_id", projects)
	writeMetricFilters(&b, opts)
	if opts.Since != "" {
		fmt.Fprintf(&b, "| where wall_time > ago(%s)\n", kqlDuration(opts.Since))
	}
	b.WriteString("| where isnotnull(step) and isnotnull(value)\n")
	b.WriteString(");\n")
	b.WriteString("let deduped = materialize(scoped\n")
	b.WriteString("| summarize arg_max(exported_at, *) by source_store_id, metric_file_id, project_id, experiment_id, run_group_id, run_id, metric_name, step, wall_time, workspace_id);\n")
	b.WriteString("let top_experiments = deduped\n")
	b.WriteString("| summarize latest_wall_time=max(wall_time) by project_id, experiment_id, workspace_id\n")
	fmt.Fprintf(&b, "| top %d by latest_wall_time desc;\n", opts.Limit+1)
	b.WriteString("deduped\n")
	b.WriteString("| join kind=inner (top_experiments) on project_id, experiment_id, workspace_id\n")
	b.WriteString("| summarize arg_max(wall_time, *) by project_id, experiment_id, run_group_id, run_id, metric_name, workspace_id\n")
	b.WriteString("| order by wall_time desc, project_id asc, run_group_id asc, run_id asc, metric_name asc\n")
	writeExperimentSearchProjection(&b, "project_id")
	return b.String(), nil
}

func BuildRunLifecycleQuery(opts RunLifecycleQueryOptions) (string, error) {
	opts.WorkspaceID = strings.TrimSpace(opts.WorkspaceID)
	opts.Project = strings.TrimSpace(opts.Project)
	projects := normalizedProjects(opts.Project, opts.Projects)
	opts.Target = strings.TrimSpace(opts.Target)
	opts.TargetType = strings.ToLower(strings.TrimSpace(opts.TargetType))
	opts.RunGroupID = strings.TrimSpace(opts.RunGroupID)
	opts.Since = strings.TrimSpace(opts.Since)
	opts.Ingestion = strings.ToLower(strings.TrimSpace(opts.Ingestion))
	opts.StaleAfter = strings.TrimSpace(opts.StaleAfter)
	if opts.TargetType == "" {
		opts.TargetType = "auto"
	}
	switch opts.TargetType {
	case "auto", "experiment", "run_group", "run":
	default:
		return "", fmt.Errorf("--target-type must be auto, experiment, run_group, or run")
	}
	if opts.Ingestion == "" {
		opts.Ingestion = "projection"
	}
	if opts.Since == "" {
		opts.Since = "7d"
	}
	if opts.StaleAfter == "" {
		opts.StaleAfter = DefaultLifecycleStaleAfter
	}
	if opts.Ingestion != "projection" && opts.Ingestion != "remote-write" {
		return "", fmt.Errorf("--ingestion must be projection or remote-write")
	}

	filterOpts := MetricsQueryOptions{
		WorkspaceID: opts.WorkspaceID,
		Target:      opts.Target,
		TargetType:  opts.TargetType,
		RunGroupID:  opts.RunGroupID,
		RunIDs:      opts.RunIDs,
	}
	var b strings.Builder
	fmt.Fprintf(&b, "let stale_after = %s;\n", kqlDuration(opts.StaleAfter))
	b.WriteString("let lifecycle = materialize(\n")
	b.WriteString("TauExpRunLifecycleDashboardRows()\n")
	if opts.Since != "" {
		fmt.Fprintf(&b, "| where observed_at > ago(%s)\n", kqlDuration(opts.Since))
	}
	writeProjectFilter(&b, "['project']", projects)
	if opts.WorkspaceID != "" {
		fmt.Fprintf(&b, "| where workspace_id == %s\n", kqlString(opts.WorkspaceID))
	}
	writeMetricFilters(&b, filterOpts)
	b.WriteString("| extend project_id = ['project']\n")
	b.WriteString(");\n")
	b.WriteString("let metric_rows = materialize(\n")
	writeLifecycleMetricRows(&b, opts, filterOpts)
	b.WriteString("| summarize metric_first_time=min(wall_time), metric_latest_time=max(wall_time) by project_id, run_group_id, run_id, workspace_id\n")
	b.WriteString(");\n")
	b.WriteString("lifecycle\n")
	b.WriteString("| join kind=leftouter metric_rows on project_id, run_group_id, run_id, workspace_id\n")
	b.WriteString("| extend first_metric_time=coalesce(first_metric_time, metric_first_time), latest_metric_time=coalesce(latest_metric_time, metric_latest_time)\n")
	b.WriteString("| extend latest_activity_time=coalesce(latest_metric_time, pod_start_time, kueue_admitted_time, created_time, observed_at)\n")
	b.WriteString("| extend outcome_state=iff(state in ('succeeded', 'failed', 'cancelled'), state, '')\n")
	b.WriteString("| extend liveness_state=case(isnotempty(outcome_state), '', isnull(latest_activity_time), '', latest_activity_time < ago(stale_after), 'not_responding', 'running')\n")
	b.WriteString("| extend lifecycle_source=case(isnotempty(outcome_state), 'lifecycle_record', latest_activity_time == latest_metric_time, 'metrics', isnotnull(latest_activity_time), 'control_plane', 'unavailable')\n")
	b.WriteString("| extend lifecycle_reason=case(isnotempty(outcome_state), coalesce(reason, 'explicit terminal lifecycle record'), liveness_state == 'running', 'run has recent liveness evidence and no terminal outcome', liveness_state == 'not_responding', 'run has no recent liveness evidence and no terminal outcome', 'run has no terminal outcome or current liveness evidence')\n")
	b.WriteString("| extend freshness_seconds=iff(isnull(latest_activity_time), long(null), datetime_diff('second', now(), latest_activity_time))\n")
	b.WriteString("| extend effective_state=case(isnotempty(outcome_state), outcome_state, liveness_state == 'not_responding', 'stale', isempty(state), 'queued', liveness_state)\n")
	b.WriteString("| project run_id, ['project']=project_id, run_group_id, tags, owning_resource_kind, owning_resource_name, namespace, cluster, submit_time, created_time, kueue_admitted_time, pod_start_time, first_metric_time, latest_metric_time, latest_evidence_time=latest_activity_time, freshness_seconds, completion_time, state, effective_state, outcome_state, liveness_state, lifecycle_reason, lifecycle_source, workload_absence_confirmed=false, reason, message, artifact_uri, checkpoint_uri, observed_at, controller_version, workspace_id\n")
	b.WriteString("| order by coalesce(latest_metric_time, pod_start_time, kueue_admitted_time, created_time, observed_at) desc, run_id asc\n")
	return b.String(), nil
}

// BuildRunHistoryQuery returns the latest durable lifecycle row for each
// cluster/namespace/durable identity. Terminal states remain selected even if a
// later non-terminal observation is appended.
func BuildRunHistoryQuery(opts RunHistoryQueryOptions) (string, error) {
	table := strings.TrimSpace(opts.Table)
	if table == "" {
		table = DefaultRunLifecycleTable
	}
	if !isKQLIdentifier(table) {
		return "", fmt.Errorf("--table must be a Kusto identifier")
	}
	opts.Cluster = strings.TrimSpace(opts.Cluster)
	opts.Namespace = strings.TrimSpace(opts.Namespace)
	opts.LocalQueue = strings.TrimSpace(opts.LocalQueue)
	opts.WorkspaceID = strings.TrimSpace(opts.WorkspaceID)
	opts.Kind = strings.TrimSpace(opts.Kind)
	if opts.Limit == 0 {
		opts.Limit = 200
	}
	if opts.Limit < 0 {
		return "", fmt.Errorf("--limit must be non-negative")
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}

	var b strings.Builder
	b.WriteString("let scoped = materialize(\n")
	b.WriteString(table + "\n")
	if opts.Cluster != "" {
		fmt.Fprintf(&b, "| where cluster == %s\n", kqlString(opts.Cluster))
	}
	if opts.Namespace != "" {
		fmt.Fprintf(&b, "| where namespace == %s\n", kqlString(opts.Namespace))
	}
	if opts.LocalQueue != "" {
		fmt.Fprintf(&b, "| where local_queue == %s\n", kqlString(opts.LocalQueue))
	}
	if opts.WorkspaceID != "" {
		fmt.Fprintf(&b, "| where workspace_id == %s\n", kqlString(opts.WorkspaceID))
	}
	if opts.Kind != "" {
		fmt.Fprintf(&b, "| where tolower(owning_resource_kind) == %s\n", kqlString(strings.ToLower(opts.Kind)))
	}
	b.WriteString("| extend durable_identity=iff(isnotempty(durable_id), durable_id, iff(isnotempty(resource_uid), resource_uid, run_id))\n")
	b.WriteString("| where isnotempty(durable_identity)\n")
	b.WriteString("| extend observation_identity=iff(isnotempty(observation_id), observation_id, strcat(durable_identity, ':', state, ':', tostring(observed_at)))\n")
	b.WriteString("| summarize arg_max(observed_at, *) by cluster, namespace, durable_identity, observation_identity\n")
	b.WriteString(");\n")
	b.WriteString("let latest_by_state = scoped\n")
	b.WriteString("| extend is_terminal=tolower(state) in ('succeeded', 'failed', 'cancelled')\n")
	b.WriteString("| summarize arg_max(observed_at, *) by cluster, namespace, durable_identity, is_terminal;\n")
	b.WriteString("latest_by_state\n")
	b.WriteString("| extend terminal_rank=iff(is_terminal, 1, 0)\n")
	b.WriteString("| summarize arg_max(terminal_rank, *) by cluster, namespace, durable_identity\n")
	b.WriteString("| project observed_at, observation_id, run_id, durable_id=durable_identity, workspace_id, result_scope, ['project'], run_group_id, tags, owning_resource_kind, owning_resource_name, namespace, cluster, local_queue, cluster_queue, workload_kind, resource_uid, resource_version, generation, submit_time, created_time, kueue_admitted_time, pod_start_time, first_metric_time, latest_metric_time, completion_time, state, reason, message, artifact_uri, checkpoint_uri, image, image_digest, config_hash, code_sha, tau_command, result_path, result_pvc, experiment_tracking, experiment_source, controller_version\n")
	b.WriteString("| order by observed_at desc, cluster asc, namespace asc, durable_id asc\n")
	fmt.Fprintf(&b, "| take %d\n", opts.Limit)
	return b.String(), nil
}

// BuildRunHistoryTimelineQuery returns every lifecycle observation for one
// Kubernetes resource UID. It is deliberately separate from BuildRunHistoryQuery:
// the latter collapses rows for a board, while this query backs a durable detail
// page after the live RayJob and its Pods have disappeared.
func BuildRunHistoryTimelineQuery(opts RunHistoryQueryOptions, resourceUID string) (string, error) {
	resourceUID = strings.TrimSpace(resourceUID)
	if resourceUID == "" {
		return "", fmt.Errorf("resource UID is required")
	}
	table := strings.TrimSpace(opts.Table)
	if table == "" {
		table = DefaultRunLifecycleTable
	}
	if !isKQLIdentifier(table) {
		return "", fmt.Errorf("--table must be a Kusto identifier")
	}
	if opts.Limit <= 0 {
		opts.Limit = 200
	}
	if opts.Limit > 1000 {
		opts.Limit = 1000
	}
	var b strings.Builder
	b.WriteString(table + "\n")
	if strings.TrimSpace(opts.Cluster) != "" {
		fmt.Fprintf(&b, "| where cluster == %s\n", kqlString(strings.TrimSpace(opts.Cluster)))
	}
	if strings.TrimSpace(opts.Namespace) != "" {
		fmt.Fprintf(&b, "| where namespace == %s\n", kqlString(strings.TrimSpace(opts.Namespace)))
	}
	if strings.TrimSpace(opts.LocalQueue) != "" {
		fmt.Fprintf(&b, "| where local_queue == %s\n", kqlString(strings.TrimSpace(opts.LocalQueue)))
	}
	if strings.TrimSpace(opts.WorkspaceID) != "" {
		fmt.Fprintf(&b, "| where workspace_id == %s\n", kqlString(strings.TrimSpace(opts.WorkspaceID)))
	}
	if kind := strings.TrimSpace(opts.Kind); kind != "" {
		fmt.Fprintf(&b, "| where tolower(owning_resource_kind) == %s\n", kqlString(strings.ToLower(kind)))
	}
	fmt.Fprintf(&b, "| where resource_uid == %s\n", kqlString(resourceUID))
	b.WriteString("| project observed_at, observation_id, run_id, durable_id, workspace_id, owning_resource_kind, owning_resource_name, namespace, cluster, local_queue, cluster_queue, resource_uid, submit_time, created_time, kueue_admitted_time, pod_start_time, completion_time, state, reason, message, artifact_uri, checkpoint_uri, image, image_digest, config_hash, tau_command, result_path, result_pvc\n")
	b.WriteString("| order by observed_at desc\n")
	fmt.Fprintf(&b, "| take %d\n", opts.Limit)
	b.WriteString("| order by observed_at asc\n")
	return b.String(), nil
}

func buildRemoteWriteMetricsQuery(opts MetricsQueryOptions) string {
	projects := normalizedProjects(opts.Project, opts.Projects)
	var b strings.Builder
	fmt.Fprintf(&b, "let target_points = %d;\n", opts.TargetPoints)
	b.WriteString("let scoped = materialize(\n")
	b.WriteString(DefaultRemoteWriteTable + "\n")
	if opts.Since != "" {
		fmt.Fprintf(&b, "| where Timestamp > ago(%s)\n", kqlDuration(opts.Since))
	}
	b.WriteString("| extend workspace_id=tostring(Labels.workspace_id), cluster=tostring(Cluster), source_store_id=tostring(Labels.source_store_id), experiment_id=coalesce(tostring(Labels.experiment_id), tostring(Labels.question_id), ''), ['project']=tostring(Labels['project']), run_group_id=tostring(Labels.run_group_id), run_id=tostring(Labels.run_id), metric_name=tostring(Labels.metric_name), source=tostring(Labels.source), unit=tostring(Labels.unit), split=tostring(Labels.split), metric_file_id=tostring(Labels.metric_file_id), metric_file_path=tostring(Labels.metric_file_path), tags=tostring(Labels.tags), step=tolong(Labels.step), wall_time=Timestamp, value=todouble(Value)\n")
	writeProjectFilter(&b, "['project']", projects)
	writeMetricScopeFilters(&b, opts)
	writeStepRangeFilters(&b, opts)
	b.WriteString("| where isnotnull(step) and isnotnull(value)\n")
	b.WriteString("| project exported_at=Timestamp, cluster, source_store_id, metric_file_id, metric_file_path, ['project'], experiment_id, run_group_id, run_id, metric_name, step, wall_time, value, unit, source, split, tags, workspace_id\n")
	b.WriteString(");\n")
	writeRequestedAndMilestones(&b, opts)
	b.WriteString("let deduped = requested\n")
	b.WriteString("| summarize arg_max(exported_at, *) by source_store_id, metric_file_id, ['project'], experiment_id, run_group_id, run_id, metric_name, step, wall_time, workspace_id;\n")
	writeMetricsResult(&b, opts)
	writeDashboardProjection(&b)
	b.WriteString("| order by run_group_id asc, run_id asc, metric_name asc, step asc\n")
	return b.String()
}

func buildRemoteWriteExperimentSearchQuery(opts MetricsQueryOptions, projects []string) string {
	var b strings.Builder
	b.WriteString("let scoped = materialize(\n")
	b.WriteString(DefaultRemoteWriteTable + "\n")
	if opts.Since != "" {
		fmt.Fprintf(&b, "| where Timestamp > ago(%s)\n", kqlDuration(opts.Since))
	}
	b.WriteString("| extend workspace_id=tostring(Labels.workspace_id), cluster=tostring(Cluster), source_store_id=tostring(Labels.source_store_id), experiment_id=coalesce(tostring(Labels.experiment_id), tostring(Labels.question_id), ''), project_id=tostring(Labels['project']), run_group_id=tostring(Labels.run_group_id), run_id=tostring(Labels.run_id), metric_name=tostring(Labels.metric_name), source=tostring(Labels.source), unit=tostring(Labels.unit), split=tostring(Labels.split), metric_file_id=tostring(Labels.metric_file_id), metric_file_path=tostring(Labels.metric_file_path), tags=tostring(Labels.tags), step=tolong(Labels.step), wall_time=Timestamp, value=todouble(Value)\n")
	writeProjectFilter(&b, "project_id", projects)
	writeMetricFilters(&b, opts)
	b.WriteString("| where isnotnull(step) and isnotnull(value)\n")
	b.WriteString("| project exported_at=Timestamp, cluster, source_store_id, metric_file_id, metric_file_path, project_id, experiment_id, run_group_id, run_id, metric_name, step, wall_time, value, unit, source, split, tags, workspace_id\n")
	b.WriteString(");\n")
	b.WriteString("let deduped = materialize(scoped\n")
	b.WriteString("| summarize arg_max(exported_at, *) by source_store_id, metric_file_id, project_id, experiment_id, run_group_id, run_id, metric_name, step, wall_time, workspace_id);\n")
	b.WriteString("let top_experiments = deduped\n")
	b.WriteString("| summarize latest_wall_time=max(wall_time) by project_id, experiment_id, workspace_id\n")
	fmt.Fprintf(&b, "| top %d by latest_wall_time desc;\n", opts.Limit+1)
	b.WriteString("deduped\n")
	b.WriteString("| join kind=inner (top_experiments) on project_id, experiment_id, workspace_id\n")
	b.WriteString("| summarize arg_max(wall_time, *) by project_id, experiment_id, run_group_id, run_id, metric_name, workspace_id\n")
	b.WriteString("| order by wall_time desc, project_id asc, run_group_id asc, run_id asc, metric_name asc\n")
	writeExperimentSearchProjection(&b, "project_id")
	return b.String()
}

func writeLifecycleMetricRows(b *strings.Builder, opts RunLifecycleQueryOptions, filterOpts MetricsQueryOptions) {
	if opts.Ingestion == "remote-write" {
		b.WriteString(DefaultRemoteWriteTable + "\n")
		if opts.Since != "" {
			fmt.Fprintf(b, "| where Timestamp > ago(%s)\n", kqlDuration(opts.Since))
		}
		b.WriteString("| extend workspace_id=tostring(Labels.workspace_id), project_id=tostring(Labels['project']), run_group_id=tostring(Labels.run_group_id), run_id=tostring(Labels.run_id), wall_time=Timestamp\n")
	} else {
		b.WriteString(DefaultProjectionTable + "\n")
		if opts.Since != "" {
			fmt.Fprintf(b, "| where exported_at > ago(%s)\n", kqlDuration(opts.Since))
		}
		b.WriteString("| extend workspace_id=tostring(parse_json(tostring(tags))['tau_workspace'])\n")
		b.WriteString("| project workspace_id, project_id=tostring(['project']), run_group_id, run_id, wall_time=todatetime(wall_time)\n")
	}
	writeProjectFilter(b, "project_id", normalizedProjects(opts.Project, opts.Projects))
	writeMetricFilters(b, filterOpts)
	b.WriteString("| where isnotempty(run_id) and isnotnull(wall_time)\n")
}

func writeMetricFilters(b *strings.Builder, opts MetricsQueryOptions) {
	writeMetricScopeFilters(b, opts)
	if len(opts.MetricNames) > 0 {
		fmt.Fprintf(b, "| where metric_name in (%s)\n", kqlStringList(opts.MetricNames))
	}
}

func writeMetricScopeFilters(b *strings.Builder, opts MetricsQueryOptions) {
	writeTargetFilter(b, opts)
	if opts.WorkspaceID != "" {
		fmt.Fprintf(b, "| where workspace_id == %s\n", kqlString(opts.WorkspaceID))
	}
	if opts.RunGroupID != "" {
		fmt.Fprintf(b, "| where run_group_id == %s\n", kqlString(opts.RunGroupID))
	}
	if len(opts.RunIDs) > 0 {
		fmt.Fprintf(b, "| where run_id in (%s)\n", kqlStringList(opts.RunIDs))
	}
}

func writeDashboardProjection(b *strings.Builder) {
	b.WriteString("| project ['project'], experiment_id, run_group_id, run_id, metric_name, step, wall_time, value, unit, source, split, metric_file_id, metric_file_path, source_store_id, cluster, exported_at, point_count, source_point_count, min_value, max_value, validation_milestone, tags, workspace_id\n")
}

func writeStepRangeFilters(b *strings.Builder, opts MetricsQueryOptions) {
	if opts.StartStep != nil {
		fmt.Fprintf(b, "| where step >= %d\n", *opts.StartStep)
	}
	if opts.EndStep != nil {
		fmt.Fprintf(b, "| where step <= %d\n", *opts.EndStep)
	}
}

func writeRequestedAndMilestones(b *strings.Builder, opts MetricsQueryOptions) {
	if opts.IncludeValidationMilestones {
		b.WriteString("let milestone_steps = scoped\n")
		b.WriteString("| where metric_name startswith 'eval/' or metric_name startswith 'validation/' or metric_name startswith 'val/' or metric_name startswith 'test/' or metric_name startswith 'final/'\n")
		b.WriteString("| distinct source_store_id, metric_file_id, ['project'], experiment_id, run_group_id, run_id, workspace_id, step\n")
		b.WriteString("| extend validation_milestone=true;\n")
	}
	b.WriteString("let requested = scoped\n")
	if len(opts.MetricNames) > 0 {
		fmt.Fprintf(b, "| where metric_name in (%s)\n", kqlStringList(opts.MetricNames))
	}
	b.WriteString(";\n")
}

func writeMetricsResult(b *strings.Builder, opts MetricsQueryOptions) {
	if opts.Raw {
		b.WriteString("deduped\n")
		b.WriteString("| extend point_count=1, source_point_count=1, min_value=value, max_value=value, validation_milestone=false\n")
		return
	}
	const dimensions = "['project'], experiment_id, run_group_id, run_id, metric_name, workspace_id"
	b.WriteString("let series_bounds = deduped\n")
	fmt.Fprintf(b, "| summarize source_point_count=count(), min_step=min(step), max_step=max(step) by %s;\n", dimensions)
	b.WriteString("let bucketed = deduped\n")
	fmt.Fprintf(b, "| join kind=inner series_bounds on %s\n", dimensions)
	b.WriteString("| extend bucket_count=max_of(1, tolong(target_points / 2)), step_span=max_of(1, max_step - min_step + 1)\n")
	b.WriteString("| extend step_bin=max_of(1, tolong(ceiling(todouble(step_span) / todouble(bucket_count))))\n")
	b.WriteString("| extend step_bucket=bin(step, step_bin);\n")
	b.WriteString("let endpoints = bucketed | where step == min_step or step == max_step;\n")
	b.WriteString("let minima = bucketed | summarize arg_min(value, *) by ")
	b.WriteString(dimensions)
	b.WriteString(", step_bucket;\n")
	b.WriteString("let maxima = bucketed | summarize arg_max(value, *) by ")
	b.WriteString(dimensions)
	b.WriteString(", step_bucket;\n")
	if opts.IncludeValidationMilestones {
		b.WriteString("let milestones = bucketed | join kind=inner milestone_steps on source_store_id, metric_file_id, ['project'], experiment_id, run_group_id, run_id, workspace_id, step;\n")
		b.WriteString("union endpoints, minima, maxima, milestones\n")
	} else {
		b.WriteString("union endpoints, minima, maxima\n")
	}
	fmt.Fprintf(b, "| summarize arg_max(exported_at, *) by source_store_id, metric_file_id, %s, step, wall_time\n", dimensions)
	fmt.Fprintf(b, "| join kind=leftouter series_bounds on %s\n", dimensions)
	if opts.IncludeValidationMilestones {
		b.WriteString("| join kind=leftouter milestone_steps on source_store_id, metric_file_id, ['project'], experiment_id, run_group_id, run_id, workspace_id, step\n")
		b.WriteString("| extend validation_milestone=coalesce(validation_milestone, false)\n")
	} else {
		b.WriteString("| extend validation_milestone=false\n")
	}
	b.WriteString("| extend point_count=source_point_count, min_value=value, max_value=value\n")
}

func writeExperimentSearchProjection(b *strings.Builder, projectExpr string) {
	if projectExpr == "" {
		projectExpr = "['project']"
	}
	fmt.Fprintf(b, "| project ['project']=%s, experiment_id, run_group_id, run_id, metric_name, step, wall_time, value, unit, source, split, metric_file_id, metric_file_path, source_store_id, cluster, tags, workspace_id\n", projectExpr)
}

func normalizedProjects(project string, projects []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, value := range append([]string{project}, projects...) {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func writeProjectFilter(b *strings.Builder, expr string, projects []string) {
	switch len(projects) {
	case 0:
		return
	case 1:
		fmt.Fprintf(b, "| where %s == %s\n", expr, kqlString(projects[0]))
	default:
		fmt.Fprintf(b, "| where %s in (%s)\n", expr, kqlStringList(projects))
	}
}

func writeTargetFilter(b *strings.Builder, opts MetricsQueryOptions) {
	if opts.Target == "" {
		return
	}
	target := kqlString(opts.Target)
	// experiment_id is the durable ADX column. It replaced question_id, which
	// earlier stores wrote for the same axis, so both are matched: filtering on
	// only one spelling silently returns an empty result rather than an error.
	// isfuzzy tolerates the column being absent on either side of the cutover.
	// The question_id half can be dropped once no rows predating the rename
	// remain inside the TauExpMetrics retention window.
	experimentMatch := fmt.Sprintf(
		"(column_ifexists('experiment_id', '') == %s or column_ifexists('question_id', '') == %s)",
		target, target)
	switch opts.TargetType {
	case "experiment":
		fmt.Fprintf(b, "| where %s\n", experimentMatch)
	case "run_group":
		fmt.Fprintf(b, "| where run_group_id == %s\n", target)
	case "run":
		fmt.Fprintf(b, "| where run_id == %s\n", target)
	default:
		fmt.Fprintf(b, "| where %s or run_group_id == %s or run_id == %s\n", experimentMatch, target, target)
	}
}

func kqlString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func kqlStringList(values []string) string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		quoted = append(quoted, kqlString(value))
	}
	return strings.Join(quoted, ", ")
}

func kqlDuration(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "7d"
	}
	for _, r := range value {
		if (r >= '0' && r <= '9') || r == '.' {
			continue
		}
		if strings.ContainsRune("smhdw", r) {
			continue
		}
		return strconv.Quote(value)
	}
	return value
}
