package expkusto

import (
	"fmt"
	"strings"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const (
	defaultRemoteWriteMetricName         = exptelemetry.RemoteWriteMetricName
	DefaultRemoteWriteTable              = exptelemetry.RemoteWriteTable
	defaultRemoteWriteDashboardFunction  = exptelemetry.RemoteWriteDashboardFunction
	DefaultProjectionTable               = exptelemetry.ProjectionTable
	defaultProjectionDashboardFunction   = exptelemetry.ProjectionDashboardFunction
	DefaultRunLifecycleTable             = exptelemetry.RunLifecycleTable
	defaultRunLifecycleDashboardFunction = exptelemetry.RunLifecycleDashboardFunction
	DefaultLifecycleStaleAfter           = "15m"
	RunStatusMetricName                  = exptelemetry.RunStatusMetricName
	RunStatusStateTag                    = exptelemetry.RunStatusStateTag
	RunStatusReasonTag                   = exptelemetry.RunStatusReasonTag
	RunStatusMessageTag                  = exptelemetry.RunStatusMessageTag
	RunStatusArtifactURITag              = exptelemetry.RunStatusArtifactURITag
	RunStatusCheckpointURITag            = exptelemetry.RunStatusCheckpointURITag
)

type SchemaOptions struct {
	Ingestion         string
	RemoteWriteTable  string
	RunLifecycleTable string
}

func BuildRemoteWriteSchemaKQL(opts SchemaOptions) (string, error) {
	table := strings.TrimSpace(opts.RemoteWriteTable)
	if table == "" {
		table = DefaultRemoteWriteTable
	}
	if !isKQLIdentifier(table) {
		return "", fmt.Errorf("--remote-write-table must be a Kusto identifier")
	}
	return fmt.Sprintf(`// Tau exp adx-mon remote-write contract.
// adx-mon creates this table from the Prometheus metric %s.
// Dashboard queries expect the adx-mon normalized metric schema plus Labels:dynamic with Tau labels.
// The special metric_name %q is a Stellar-only run terminal-state marker.
.create-merge table %s (Timestamp: datetime, SeriesId: long, Labels: dynamic, Value: real, Container: string, Namespace: string, Pod: string, Cluster: string, Host: string)

.create-or-alter function with (folder = 'Tau/experiments', docstring = 'Normalize adx-mon %s rows to the Tau dashboard metric contract.', skipvalidation = 'true') %s()
{
%s
| extend workspace_id=tostring(Labels.workspace_id), cluster=tostring(Cluster), source_store_id=tostring(Labels.source_store_id), ['project']=tostring(Labels['project']), experiment_id=coalesce(tostring(Labels.experiment_id), tostring(Labels.question_id), ''), run_group_id=tostring(Labels.run_group_id), run_id=tostring(Labels.run_id), metric_name=tostring(Labels.metric_name), source=tostring(Labels.source), unit=tostring(Labels.unit), split=tostring(Labels.split), metric_file_id=tostring(Labels.metric_file_id), metric_file_path=tostring(Labels.metric_file_path), tags=tostring(Labels.tags), step=tolong(Labels.step), wall_time=Timestamp, value=todouble(Value)
| project exported_at=Timestamp, workspace_id, cluster, source_store_id, metric_file_id, metric_file_path, ['project'], experiment_id, run_group_id, run_id, metric_name, step, wall_time, value, unit, source, split, tags
}
`, defaultRemoteWriteMetricName, RunStatusMetricName, table, defaultRemoteWriteMetricName, defaultRemoteWriteDashboardFunction, table), nil
}

func BuildRunLifecycleSchemaKQL(opts SchemaOptions) (string, error) {
	table := strings.TrimSpace(opts.RunLifecycleTable)
	if table == "" {
		table = DefaultRunLifecycleTable
	}
	if !isKQLIdentifier(table) {
		return "", fmt.Errorf("--run-lifecycle-table must be a Kusto identifier")
	}
	return fmt.Sprintf(`// Tau/Stellar run lifecycle contract.
// A Tau-owned controller/operator appends or upserts one row per observed run state change.
// Metrics alone cannot answer queued/running/failed/stale states; Stellar joins this index with ExperimentMetrics rows.
// State values: submitted, queued, admitted, running, succeeded, failed, cancelled, stale.
.create-merge table %s (
    observed_at: datetime,
    observation_id: string,
    run_id: string,
    durable_id: string,
    workspace_id: string,
    result_scope: string,
    ['project']: string,
    run_group_id: string,
    tags: dynamic,
    owning_resource_kind: string,
    owning_resource_name: string,
    namespace: string,
    cluster: string,
    local_queue: string,
    cluster_queue: string,
    workload_kind: string,
    resource_uid: string,
    resource_version: string,
    generation: long,
    submit_time: datetime,
    created_time: datetime,
    kueue_admitted_time: datetime,
    pod_start_time: datetime,
    first_metric_time: datetime,
    latest_metric_time: datetime,
    completion_time: datetime,
    state: string,
    reason: string,
    message: string,
    artifact_uri: string,
    checkpoint_uri: string,
    image: string,
    image_digest: string,
    config_hash: string,
    code_sha: string,
    tau_command: string,
    result_path: string,
    result_pvc: string,
    experiment_tracking: string,
    experiment_source: string,
    controller_version: string
)

.create-or-alter function with (folder = 'Tau/experiments', docstring = 'Return the latest Tau/Stellar lifecycle row per scoped durable run; terminal states remain monotonic.', skipvalidation = 'true') %s()
{
%s
| extend durable_identity=iff(isnotempty(durable_id), durable_id, iff(isnotempty(resource_uid), resource_uid, run_id))
| where isnotempty(durable_identity)
| extend observation_identity=iff(isnotempty(observation_id), observation_id, strcat(durable_identity, ':', state, ':', tostring(observed_at)))
| summarize arg_max(observed_at, *) by cluster, namespace, durable_identity, observation_identity
| extend is_terminal=tolower(state) in ('succeeded', 'failed', 'cancelled')
| summarize arg_max(observed_at, *) by cluster, namespace, durable_identity, is_terminal
| extend terminal_rank=iff(is_terminal, 1, 0)
| summarize arg_max(terminal_rank, *) by cluster, namespace, durable_identity
| project observed_at, observation_id, run_id, durable_id=durable_identity, workspace_id, result_scope, ['project'], run_group_id, tags, owning_resource_kind, owning_resource_name, namespace, cluster, local_queue, cluster_queue, workload_kind, resource_uid, resource_version, generation, submit_time, created_time, kueue_admitted_time, pod_start_time, first_metric_time, latest_metric_time, completion_time, state, reason, message, artifact_uri, checkpoint_uri, image, image_digest, config_hash, code_sha, tau_command, result_path, result_pvc, experiment_tracking, experiment_source, controller_version
}
`, table, defaultRunLifecycleDashboardFunction, table), nil
}

func BuildProjectionDashboardSchemaKQL() string {
	return fmt.Sprintf(`// Tau exp dashboard row contract for the explicit %s projection table.
.create-or-alter function with (folder = 'Tau/experiments', docstring = 'Normalize %s projection rows to the Tau dashboard metric contract.', skipvalidation = 'true') %s()
{
%s
| extend workspace_id=tostring(parse_json(tostring(tags))['tau_workspace'])
| extend experimentIdOf=iff(isempty(column_ifexists('experiment_id', '')), column_ifexists('question_id', ''), column_ifexists('experiment_id', ''))
| project exported_at, workspace_id, source_store_id, metric_file_id, metric_file_path, ['project'], experiment_id=experimentIdOf, run_group_id, run_id, metric_name, step=tolong(step), wall_time=todatetime(wall_time), value=todouble(value), unit, source, split, tags
}
`, DefaultProjectionTable, DefaultProjectionTable, defaultProjectionDashboardFunction, DefaultProjectionTable)
}

func isKQLIdentifier(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if r == '_' || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}
