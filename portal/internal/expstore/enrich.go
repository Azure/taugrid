// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (s *Store) EnrichRunData(ctx context.Context, opts EnrichRunDataOptions) (EnrichRunDataResult, error) {
	var result EnrichRunDataResult
	err := s.withWriteLock(ctx, func() error {
		var err error
		result, err = s.enrichRunDataLocked(ctx, opts)
		return err
	})
	return result, err
}

func (s *Store) enrichRunDataLocked(ctx context.Context, opts EnrichRunDataOptions) (EnrichRunDataResult, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if opts.Command == "" {
		opts.Command = "exp autocapture"
	}
	opts.Run = s.normalizeRunRecord(opts.Run, now)
	if err := validateRunRecord(opts.Run); err != nil {
		return EnrichRunDataResult{}, err
	}
	if err := validateRecordCollections(RecordRunDataOptions{Run: opts.Run, RunContext: opts.RunContext, Tags: opts.Tags}); err != nil {
		return EnrichRunDataResult{}, err
	}
	if err := validateEventRecords(opts.Run.RunID, opts.Events); err != nil {
		return EnrichRunDataResult{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EnrichRunDataResult{}, err
	}
	defer tx.Rollback()

	mergedRun, runCreated, runUpdated, err := upsertRunEnrichment(ctx, tx, opts.Run)
	if err != nil {
		return EnrichRunDataResult{}, err
	}
	mergedContext, contextCreated, contextUpdated, err := upsertRunContextEnrichment(ctx, tx, opts.RunContext)
	if err != nil {
		return EnrichRunDataResult{}, err
	}
	createdTags, err := ensureTagRecords(ctx, tx, opts.Tags)
	if err != nil {
		return EnrichRunDataResult{}, err
	}
	createdEvents, err := ensureEventRecords(ctx, tx, opts.Events)
	if err != nil {
		return EnrichRunDataResult{}, err
	}

	cleanupMirrors, err := s.appendEnrichmentMirrors(mergedRun, runCreated || runUpdated, mergedContext, contextCreated || contextUpdated, createdTags, createdEvents)
	if err != nil {
		return EnrichRunDataResult{}, err
	}
	if err := tx.Commit(); err != nil {
		if cleanupErr := cleanupMirrors(); cleanupErr != nil {
			return EnrichRunDataResult{}, fmt.Errorf("commit experiment enrichment: %w; cleanup JSONL mirrors: %v", err, cleanupErr)
		}
		return EnrichRunDataResult{}, err
	}

	return EnrichRunDataResult{
		RunID:             opts.Run.RunID,
		CreatedRun:        runCreated,
		UpdatedRun:        runUpdated,
		CreatedRunContext: contextCreated,
		UpdatedRunContext: contextUpdated,
		Events:            len(createdEvents),
		Tags:              len(createdTags),
		Reused:            !(runCreated || runUpdated || contextCreated || contextUpdated || len(createdEvents) > 0 || len(createdTags) > 0),
	}, nil
}

func validateEventRecords(runID string, events []EventRecord) error {
	for _, event := range events {
		if err := validateID("event", event.EventID); err != nil {
			return err
		}
		if event.RunID != runID {
			return fmt.Errorf("event %q run_id %q does not match run %q", event.EventID, event.RunID, runID)
		}
		if event.Time == "" || event.Type == "" || event.Source == "" || event.Severity == "" || event.Message == "" {
			return fmt.Errorf("event %q time, type, source, severity, and message are required", event.EventID)
		}
	}
	return nil
}

func upsertRunEnrichment(ctx context.Context, tx *sql.Tx, incoming RunRecord) (RunRecord, bool, bool, error) {
	existing, ok, err := runRecordByID(ctx, tx, incoming.RunID)
	if err != nil {
		return RunRecord{}, false, false, err
	}
	if !ok {
		created, err := ensureRunRecord(ctx, tx, incoming)
		return incoming, created, false, err
	}
	merged, err := mergeRunRecordForEnrichment(existing, incoming)
	if err != nil {
		return RunRecord{}, false, false, err
	}
	if runRecordExactEqual(existing, merged) {
		return existing, false, false, nil
	}
	_, err = tx.ExecContext(ctx, `
UPDATE runs
SET state = ?, owner = nullif(?, ''), created_at = ?, started_at = nullif(?, ''),
    completed_at = nullif(?, ''), config_hash = nullif(?, ''), code_sha = nullif(?, ''),
    image_digest = nullif(?, ''), tau_command = nullif(?, ''), result_uri = nullif(?, ''),
    index_version = ?
WHERE run_id = ?
`, merged.State, merged.Owner, merged.CreatedAt, merged.StartedAt, merged.CompletedAt, merged.ConfigHash, merged.CodeSHA,
		merged.ImageDigest, merged.TauCommand, merged.ResultURI, merged.IndexVersion, merged.RunID)
	if err != nil {
		return RunRecord{}, false, false, err
	}
	return merged, false, true, nil
}

func runRecordByID(ctx context.Context, tx *sql.Tx, runID string) (RunRecord, bool, error) {
	var run RunRecord
	err := tx.QueryRowContext(ctx, `
SELECT run_id, project, run_group_id,
       coalesce(parent_run_id, ''), state, coalesce(owner, ''), created_at, coalesce(started_at, ''),
       coalesce(completed_at, ''), coalesce(config_hash, ''), coalesce(code_sha, ''),
       coalesce(image_digest, ''), coalesce(tau_command, ''), coalesce(result_uri, ''), index_version
FROM runs WHERE run_id = ?`, runID).Scan(
		&run.RunID,
		&run.Project,
		&run.RunGroupID,
		&run.ParentRunID,
		&run.State,
		&run.Owner,
		&run.CreatedAt,
		&run.StartedAt,
		&run.CompletedAt,
		&run.ConfigHash,
		&run.CodeSHA,
		&run.ImageDigest,
		&run.TauCommand,
		&run.ResultURI,
		&run.IndexVersion,
	)
	if err == sql.ErrNoRows {
		return RunRecord{}, false, nil
	}
	if err != nil {
		return RunRecord{}, false, err
	}
	return run, true, nil
}

func mergeRunRecordForEnrichment(existing, incoming RunRecord) (RunRecord, error) {
	merged := existing
	for _, field := range []struct {
		name     string
		existing string
		incoming string
	}{
		{"project", existing.Project, incoming.Project},
		{"run_group_id", existing.RunGroupID, incoming.RunGroupID},
		{"parent_run_id", existing.ParentRunID, incoming.ParentRunID},
	} {
		if field.existing != "" && field.incoming != "" && field.existing != field.incoming {
			return RunRecord{}, fmt.Errorf("%w: run %q %s is %q, controller observed %q", ErrConflict, existing.RunID, field.name, field.existing, field.incoming)
		}
	}
	fillString(&merged.ParentRunID, incoming.ParentRunID)
	fillString(&merged.Owner, incoming.Owner)
	fillString(&merged.StartedAt, incoming.StartedAt)
	fillString(&merged.CompletedAt, incoming.CompletedAt)
	fillString(&merged.ConfigHash, incoming.ConfigHash)
	fillString(&merged.CodeSHA, incoming.CodeSHA)
	fillString(&merged.ImageDigest, incoming.ImageDigest)
	fillString(&merged.TauCommand, incoming.TauCommand)
	fillString(&merged.ResultURI, incoming.ResultURI)
	fillString(&merged.IndexVersion, incoming.IndexVersion)
	if incoming.State != "" {
		next, err := mergeRunState(existing.State, incoming.State)
		if err != nil {
			return RunRecord{}, err
		}
		merged.State = next
	}
	return merged, nil
}

func mergeRunState(existing, incoming string) (string, error) {
	if existing == "" || stateRank(incoming) > stateRank(existing) {
		return incoming, nil
	}
	if stateRank(existing) == stateRank(incoming) && existing != incoming {
		return "", fmt.Errorf("%w: terminal run state is %q, controller observed %q", ErrConflict, existing, incoming)
	}
	return existing, nil
}

func stateRank(state string) int {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "pending":
		return 1
	case "running":
		return 2
	case "succeeded", "failed":
		return 3
	default:
		return 0
	}
}

func upsertRunContextEnrichment(ctx context.Context, tx *sql.Tx, incoming *RunContextRecord) (*RunContextRecord, bool, bool, error) {
	if incoming == nil {
		return nil, false, false, nil
	}
	existing, ok, err := runContextByRunID(ctx, tx, incoming.RunID)
	if err != nil {
		return nil, false, false, err
	}
	if !ok {
		created, err := ensureRunContextRecord(ctx, tx, incoming)
		return incoming, created, false, err
	}
	merged := mergeRunContextForEnrichment(existing, *incoming)
	if runContextRecordEqual(existing, merged) {
		return &existing, false, false, nil
	}
	_, err = tx.ExecContext(ctx, `
UPDATE run_context
SET cluster = nullif(?, ''), namespace = nullif(?, ''), team = nullif(?, ''),
    profile = nullif(?, ''), lane = nullif(?, ''), local_queue = nullif(?, ''),
    cluster_queue = nullif(?, ''), kueue_workload = nullif(?, ''), pod_uid = nullif(?, ''),
    ray_job = nullif(?, ''), resource_claims = nullif(?, ''), gpu_class = nullif(?, ''),
    gpu_count = ?, node_names = nullif(?, ''), mounts = nullif(?, ''),
    queue_wait_seconds = ?, gpu_hours = ?, estimated_cost = ?,
    runtime = nullif(?, ''), dependencies = nullif(?, ''), log_uri = nullif(?, '')
WHERE run_id = ?
`, merged.Cluster, merged.Namespace, merged.Team, merged.Profile, merged.Lane, merged.LocalQueue,
		merged.ClusterQueue, merged.KueueWorkload, merged.PodUID, merged.RayJob, merged.ResourceClaims,
		merged.GPUClass, nullableInt64(merged.GPUCount), merged.NodeNames, merged.Mounts,
		nullableFloat64(merged.QueueWaitSeconds), nullableFloat64(merged.GPUHours), nullableFloat64(merged.EstimatedCost),
		merged.Runtime, merged.Dependencies, merged.LogURI, merged.RunID)
	if err != nil {
		return nil, false, false, err
	}
	return &merged, false, true, nil
}

func runContextByRunID(ctx context.Context, tx *sql.Tx, runID string) (RunContextRecord, bool, error) {
	var rc RunContextRecord
	var gpuCount sql.NullInt64
	var queueWait, gpuHours, estimatedCost sql.NullFloat64
	err := tx.QueryRowContext(ctx, `
SELECT run_id, coalesce(cluster, ''), coalesce(namespace, ''), coalesce(team, ''), coalesce(profile, ''),
       coalesce(lane, ''), coalesce(local_queue, ''), coalesce(cluster_queue, ''), coalesce(kueue_workload, ''),
       coalesce(pod_uid, ''), coalesce(ray_job, ''), coalesce(resource_claims, ''), coalesce(gpu_class, ''),
       gpu_count, coalesce(node_names, ''), coalesce(mounts, ''), queue_wait_seconds, gpu_hours, estimated_cost,
       coalesce(runtime, ''), coalesce(dependencies, ''), coalesce(log_uri, '')
FROM run_context WHERE run_id = ?`, runID).Scan(
		&rc.RunID,
		&rc.Cluster,
		&rc.Namespace,
		&rc.Team,
		&rc.Profile,
		&rc.Lane,
		&rc.LocalQueue,
		&rc.ClusterQueue,
		&rc.KueueWorkload,
		&rc.PodUID,
		&rc.RayJob,
		&rc.ResourceClaims,
		&rc.GPUClass,
		&gpuCount,
		&rc.NodeNames,
		&rc.Mounts,
		&queueWait,
		&gpuHours,
		&estimatedCost,
		&rc.Runtime,
		&rc.Dependencies,
		&rc.LogURI,
	)
	if err == sql.ErrNoRows {
		return RunContextRecord{}, false, nil
	}
	if err != nil {
		return RunContextRecord{}, false, err
	}
	if gpuCount.Valid {
		rc.GPUCount = &gpuCount.Int64
	}
	if queueWait.Valid {
		rc.QueueWaitSeconds = &queueWait.Float64
	}
	if gpuHours.Valid {
		rc.GPUHours = &gpuHours.Float64
	}
	if estimatedCost.Valid {
		rc.EstimatedCost = &estimatedCost.Float64
	}
	return rc, true, nil
}

func mergeRunContextForEnrichment(existing, incoming RunContextRecord) RunContextRecord {
	merged := existing
	fillString(&merged.Cluster, incoming.Cluster)
	fillString(&merged.Namespace, incoming.Namespace)
	fillString(&merged.Team, incoming.Team)
	fillString(&merged.Profile, incoming.Profile)
	fillString(&merged.Lane, incoming.Lane)
	fillString(&merged.LocalQueue, incoming.LocalQueue)
	fillString(&merged.ClusterQueue, incoming.ClusterQueue)
	merged.KueueWorkload = mergeCSV(merged.KueueWorkload, incoming.KueueWorkload)
	merged.PodUID = mergeCSV(merged.PodUID, incoming.PodUID)
	fillString(&merged.RayJob, incoming.RayJob)
	merged.ResourceClaims = mergeCSV(merged.ResourceClaims, incoming.ResourceClaims)
	fillString(&merged.GPUClass, incoming.GPUClass)
	if merged.GPUCount == nil && incoming.GPUCount != nil {
		merged.GPUCount = incoming.GPUCount
	}
	merged.NodeNames = mergeCSV(merged.NodeNames, incoming.NodeNames)
	fillString(&merged.Mounts, incoming.Mounts)
	updateFloat64(&merged.QueueWaitSeconds, incoming.QueueWaitSeconds)
	updateFloat64(&merged.GPUHours, incoming.GPUHours)
	updateFloat64(&merged.EstimatedCost, incoming.EstimatedCost)
	fillString(&merged.Runtime, incoming.Runtime)
	fillString(&merged.Dependencies, incoming.Dependencies)
	fillString(&merged.LogURI, incoming.LogURI)
	return merged
}

func ensureEventRecords(ctx context.Context, tx *sql.Tx, events []EventRecord) ([]EventRecord, error) {
	created := []EventRecord{}
	for _, event := range events {
		var existing EventRecord
		err := tx.QueryRowContext(ctx, `
SELECT event_id, run_id, time, type, source, severity, message, coalesce(payload, '')
FROM events WHERE event_id = ?`, event.EventID).Scan(
			&existing.EventID,
			&existing.RunID,
			&existing.Time,
			&existing.Type,
			&existing.Source,
			&existing.Severity,
			&existing.Message,
			&existing.Payload,
		)
		if err == nil {
			if !eventRecordEqual(existing, event) {
				return nil, fmt.Errorf("%w: event %q already exists with different metadata", ErrConflict, event.EventID)
			}
			continue
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO events(event_id, run_id, time, type, source, severity, message, payload)
VALUES (?, ?, ?, ?, ?, ?, ?, nullif(?, ''))
`, event.EventID, event.RunID, event.Time, event.Type, event.Source, event.Severity, event.Message, event.Payload); err != nil {
			return nil, err
		}
		created = append(created, event)
	}
	return created, nil
}

func (s *Store) appendEnrichmentMirrors(run RunRecord, runChanged bool, runContext *RunContextRecord, contextChanged bool, tags []TagRecord, events []EventRecord) (func() error, error) {
	var cleanups []func() error
	add := func(name string, record any) error {
		cleanup, err := s.appendJSONLWithRollback(name, record)
		if err != nil {
			if cleanupErr := cleanupJSONL(cleanups); cleanupErr != nil {
				return fmt.Errorf("%w; cleanup JSONL mirrors: %v", err, cleanupErr)
			}
			return err
		}
		cleanups = append(cleanups, cleanup)
		return nil
	}
	if runChanged {
		if err := add("runs.jsonl", run); err != nil {
			return nil, err
		}
	}
	if contextChanged && runContext != nil {
		if err := add("run_context.jsonl", runContext); err != nil {
			return nil, err
		}
	}
	for _, tag := range tags {
		if err := add("tags.jsonl", tag); err != nil {
			return nil, err
		}
	}
	for _, event := range events {
		if err := add("events.jsonl", event); err != nil {
			return nil, err
		}
	}
	return func() error {
		return cleanupJSONL(cleanups)
	}, nil
}

func eventRecordEqual(a, b EventRecord) bool {
	return a.EventID == b.EventID &&
		a.RunID == b.RunID &&
		a.Time == b.Time &&
		a.Type == b.Type &&
		a.Source == b.Source &&
		a.Severity == b.Severity &&
		a.Message == b.Message
}

func runRecordExactEqual(a, b RunRecord) bool {
	return a.RunID == b.RunID &&
		a.Project == b.Project &&
		a.RunGroupID == b.RunGroupID &&
		a.ParentRunID == b.ParentRunID &&
		a.State == b.State &&
		a.Owner == b.Owner &&
		a.CreatedAt == b.CreatedAt &&
		a.StartedAt == b.StartedAt &&
		a.CompletedAt == b.CompletedAt &&
		a.ConfigHash == b.ConfigHash &&
		a.CodeSHA == b.CodeSHA &&
		a.ImageDigest == b.ImageDigest &&
		a.TauCommand == b.TauCommand &&
		a.ResultURI == b.ResultURI &&
		a.IndexVersion == b.IndexVersion
}

func fillString(dst *string, value string) {
	if *dst == "" && value != "" {
		*dst = value
	}
}

func updateFloat64(dst **float64, value *float64) {
	if value == nil {
		return
	}
	if *dst == nil || **dst != *value {
		*dst = value
	}
}

func mergeCSV(a, b string) string {
	seen := map[string]bool{}
	var out []string
	for _, value := range strings.Split(a+","+b, ",") {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}
