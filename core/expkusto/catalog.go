// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expkusto

import (
	"fmt"
	"strings"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const (
	maxCatalogQueryLimit = 1000
	kqlProjectColumn     = "['project']"
)

type CatalogQueryOptions struct {
	WorkspaceID       string
	Project           string
	Projects          []string
	Target            string
	TargetType        string
	RunGroupID        string
	RunIDs            []string
	MetricNames       []string
	Since             string
	Limit             int
	AfterAt           string
	AfterProject      string
	AfterRunID        string
	AfterExperimentID string
}

func BuildSeriesCatalogQuery(opts CatalogQueryOptions) (string, error) {
	opts, projects, err := normalizeCatalogQueryOptions(opts)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("let scoped_catalog = materialize(\n")
	b.WriteString(exptelemetry.SeriesCatalogRowsFunction + "()\n")
	writeCatalogFilters(&b, opts, projects, true)
	b.WriteString(");\n")
	b.WriteString("let top_experiments = scoped_catalog\n")
	b.WriteString("| summarize latest_activity_at=max(latest_activity_at) by workspace_id, " + kqlProjectColumn + ", experiment_id\n")
	fmt.Fprintf(&b, "| top %d by latest_activity_at desc;\n", opts.Limit+1)
	b.WriteString("scoped_catalog\n")
	b.WriteString("| join kind=inner top_experiments on workspace_id, " + kqlProjectColumn + ", experiment_id\n")
	b.WriteString("| project catalog_version, workspace_id, cluster, source_store_id, " + kqlProjectColumn + ", experiment_id, run_group_id, run_id, metric_name, first_activity_at, latest_activity_at, min_step, max_step, latest_step, latest_value, unit, source, split, latest_file_id, latest_file_path, step=tolong(latest_step), wall_time=todatetime(latest_activity_at), value=todouble(latest_value), metric_file_id=latest_file_id, metric_file_path=latest_file_path, tags=''\n")
	b.WriteString("| order by latest_activity_at desc, " + kqlProjectColumn + " asc, experiment_id asc, run_group_id asc, run_id asc, metric_name asc\n")
	return b.String(), nil
}

func BuildRunCatalogQuery(opts CatalogQueryOptions) (string, error) {
	opts, projects, err := normalizeCatalogQueryOptions(opts)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	if len(opts.MetricNames) > 0 {
		b.WriteString("let matching_catalog_runs = " + exptelemetry.SeriesCatalogRowsFunction + "()\n")
		fmt.Fprintf(&b, "| where metric_name in (%s)\n", kqlStringList(opts.MetricNames))
		b.WriteString("| distinct workspace_id, cluster, " + kqlProjectColumn + ", experiment_id, run_group_id, run_id;\n")
	}
	b.WriteString(exptelemetry.RunCatalogRowsFunction + "()\n")
	if len(opts.MetricNames) > 0 {
		b.WriteString("| join kind=inner matching_catalog_runs on workspace_id, cluster, " + kqlProjectColumn + ", experiment_id, run_group_id, run_id\n")
	}
	writeCatalogFilters(&b, opts, projects, false)
	b.WriteString("| extend cursor_sort_at=coalesce(created_time, submit_time, first_activity_at, latest_activity_at)\n")
	if opts.AfterAt != "" {
		afterAt := "todatetime(" + kqlString(opts.AfterAt) + ")"
		fmt.Fprintf(&b, "| where cursor_sort_at < %s or (cursor_sort_at == %s and (%s > %s or (%s == %s and run_id > %s)))\n",
			afterAt, afterAt,
			kqlProjectColumn, kqlString(opts.AfterProject),
			kqlProjectColumn, kqlString(opts.AfterProject),
			kqlString(opts.AfterRunID))
	}
	b.WriteString("| project catalog_version, workspace_id, cluster, source_store_id, " + kqlProjectColumn + ", experiment_id, run_group_id, run_id, first_activity_at, latest_activity_at, first_metric_at, latest_metric_at, latest_observation_at, terminal_at, state, reason, message, durable_id, result_scope, owning_resource_kind, owning_resource_name, namespace, local_queue, cluster_queue, workload_kind, resource_uid, submit_time, created_time, kueue_admitted_time, pod_start_time, completion_time, artifact_uri, checkpoint_uri, image, image_digest, config_hash, code_sha, tau_command, result_path, result_pvc, experiment_tracking, experiment_source, controller_version, metric_series_count, has_metrics, has_lifecycle, metric_name='', step=long(null), wall_time=todatetime(latest_activity_at), value=real(null), unit='', source='', split='', metric_file_id='', metric_file_path='', tags=tostring(tags), cursor_sort_at\n")
	b.WriteString("| order by cursor_sort_at desc, " + kqlProjectColumn + " asc, run_id asc\n")
	fmt.Fprintf(&b, "| take %d\n", opts.Limit)
	return b.String(), nil
}

func BuildExperimentCatalogQuery(opts CatalogQueryOptions) (string, error) {
	opts, projects, err := normalizeCatalogQueryOptions(opts)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if len(opts.MetricNames) > 0 {
		b.WriteString("let matching_catalog_runs = " + exptelemetry.SeriesCatalogRowsFunction + "()\n")
		fmt.Fprintf(&b, "| where metric_name in (%s)\n", kqlStringList(opts.MetricNames))
		b.WriteString("| distinct workspace_id, cluster, " + kqlProjectColumn + ", experiment_id, run_group_id, run_id;\n")
	}
	b.WriteString("let scoped_runs = materialize(\n")
	b.WriteString(exptelemetry.RunCatalogRowsFunction + "()\n")
	if len(opts.MetricNames) > 0 {
		b.WriteString("| join kind=inner matching_catalog_runs on workspace_id, cluster, " + kqlProjectColumn + ", experiment_id, run_group_id, run_id\n")
	}
	writeCatalogFilters(&b, opts, projects, false)
	b.WriteString(");\n")
	b.WriteString("let page_experiments = scoped_runs\n")
	b.WriteString("| summarize experiment_cursor_at=max(latest_activity_at) by workspace_id, " + kqlProjectColumn + ", experiment_id\n")
	if opts.AfterAt != "" {
		afterAt := "todatetime(" + kqlString(opts.AfterAt) + ")"
		fmt.Fprintf(&b, "| where experiment_cursor_at < %s or (experiment_cursor_at == %s and (%s > %s or (%s == %s and experiment_id > %s)))\n",
			afterAt, afterAt,
			kqlProjectColumn, kqlString(opts.AfterProject),
			kqlProjectColumn, kqlString(opts.AfterProject),
			kqlString(opts.AfterExperimentID))
	}
	b.WriteString("| order by experiment_cursor_at desc, " + kqlProjectColumn + " asc, experiment_id asc\n")
	fmt.Fprintf(&b, "| take %d;\n", opts.Limit)
	b.WriteString("scoped_runs\n")
	b.WriteString("| join kind=inner page_experiments on workspace_id, " + kqlProjectColumn + ", experiment_id\n")
	b.WriteString("| project catalog_version, workspace_id, cluster, source_store_id, " + kqlProjectColumn + ", experiment_id, run_group_id, run_id, first_activity_at, latest_activity_at, first_metric_at, latest_metric_at, latest_observation_at, terminal_at, state, reason, message, durable_id, result_scope, owning_resource_kind, owning_resource_name, namespace, local_queue, cluster_queue, workload_kind, resource_uid, submit_time, created_time, kueue_admitted_time, pod_start_time, completion_time, artifact_uri, checkpoint_uri, image, image_digest, config_hash, code_sha, tau_command, result_path, result_pvc, experiment_tracking, experiment_source, controller_version, metric_series_count, has_metrics, has_lifecycle, metric_name='', step=long(null), wall_time=todatetime(latest_activity_at), value=real(null), unit='', source='', split='', metric_file_id='', metric_file_path='', tags=tostring(tags), experiment_cursor_at\n")
	b.WriteString("| order by experiment_cursor_at desc, " + kqlProjectColumn + " asc, experiment_id asc, run_id asc\n")
	return b.String(), nil
}

func normalizeCatalogQueryOptions(opts CatalogQueryOptions) (CatalogQueryOptions, []string, error) {
	opts.WorkspaceID = strings.TrimSpace(opts.WorkspaceID)
	opts.Project = strings.TrimSpace(opts.Project)
	opts.Target = strings.TrimSpace(opts.Target)
	opts.TargetType = strings.ToLower(strings.TrimSpace(opts.TargetType))
	opts.RunGroupID = strings.TrimSpace(opts.RunGroupID)
	opts.Since = strings.TrimSpace(opts.Since)
	opts.AfterAt = strings.TrimSpace(opts.AfterAt)
	opts.AfterProject = strings.TrimSpace(opts.AfterProject)
	opts.AfterRunID = strings.TrimSpace(opts.AfterRunID)
	opts.AfterExperimentID = strings.TrimSpace(opts.AfterExperimentID)
	if opts.TargetType == "" {
		opts.TargetType = "auto"
	}
	switch opts.TargetType {
	case "auto", "experiment", "run_group", "run":
	default:
		return CatalogQueryOptions{}, nil, fmt.Errorf("--target-type must be auto, experiment, run_group, or run")
	}
	if opts.Since == "" {
		opts.Since = "7d"
	}
	if opts.Limit == 0 {
		opts.Limit = 200
	}
	if opts.Limit < 0 {
		return CatalogQueryOptions{}, nil, fmt.Errorf("--limit must be non-negative")
	}
	opts.Limit = min(opts.Limit, maxCatalogQueryLimit)
	return opts, normalizedProjects(opts.Project, opts.Projects), nil
}

func writeCatalogFilters(b *strings.Builder, opts CatalogQueryOptions, projects []string, includeMetrics bool) {
	if opts.Since != "" {
		fmt.Fprintf(b, "| where latest_activity_at > ago(%s)\n", kqlDuration(opts.Since))
	}
	if opts.WorkspaceID != "" {
		fmt.Fprintf(b, "| where workspace_id == %s\n", kqlString(opts.WorkspaceID))
	}
	writeProjectFilter(b, kqlProjectColumn, projects)
	if opts.Target != "" {
		target := kqlString(opts.Target)
		switch opts.TargetType {
		case "experiment":
			fmt.Fprintf(b, "| where experiment_id == %s\n", target)
		case "run_group":
			fmt.Fprintf(b, "| where run_group_id == %s\n", target)
		case "run":
			fmt.Fprintf(b, "| where run_id == %s\n", target)
		default:
			fmt.Fprintf(b, "| where experiment_id == %s or run_group_id == %s or run_id == %s\n", target, target, target)
		}
	}
	if opts.RunGroupID != "" {
		fmt.Fprintf(b, "| where run_group_id == %s\n", kqlString(opts.RunGroupID))
	}
	if len(opts.RunIDs) > 0 {
		fmt.Fprintf(b, "| where run_id in (%s)\n", kqlStringList(opts.RunIDs))
	}
	if includeMetrics && len(opts.MetricNames) > 0 {
		fmt.Fprintf(b, "| where metric_name in (%s)\n", kqlStringList(opts.MetricNames))
	}
}
