// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
)

func (s *Store) RecordRunData(ctx context.Context, opts RecordRunDataOptions) (RecordRunDataResult, error) {
	var result RecordRunDataResult
	err := s.withWriteLock(ctx, func() error {
		var err error
		result, err = s.recordRunDataLocked(ctx, opts)
		return err
	})
	return result, err
}

func (s *Store) recordRunDataLocked(ctx context.Context, opts RecordRunDataOptions) (RecordRunDataResult, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	if opts.Command == "" {
		opts.Command = "exp record"
	}
	opts.Run = s.normalizeRunRecord(opts.Run, now)
	if err := validateRunRecord(opts.Run); err != nil {
		return RecordRunDataResult{}, err
	}
	if err := validateRecordCollections(opts); err != nil {
		return RecordRunDataResult{}, err
	}
	requestHash := opts.RequestHash
	if requestHash == "" {
		var err error
		requestHash, err = recordRunDataRequestHash(opts)
		if err != nil {
			return RecordRunDataResult{}, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return RecordRunDataResult{}, err
	}
	defer tx.Rollback()

	if opts.IdempotencyKey != "" {
		existingHash, err := idempotencyHash(ctx, tx, opts.IdempotencyKey)
		if err != nil {
			return RecordRunDataResult{}, err
		}
		if existingHash != "" {
			if existingHash != requestHash {
				return RecordRunDataResult{}, fmt.Errorf("%w: idempotency key %q was used for a different request", ErrConflict, opts.IdempotencyKey)
			}
			return RecordRunDataResult{
				RunID:          opts.Run.RunID,
				Reused:         true,
				IdempotencyKey: opts.IdempotencyKey,
			}, nil
		}
	}

	runCreated, err := ensureRunRecord(ctx, tx, opts.Run)
	if err != nil {
		return RecordRunDataResult{}, err
	}
	runContextCreated, err := ensureRunContextRecord(ctx, tx, opts.RunContext)
	if err != nil {
		return RecordRunDataResult{}, err
	}
	createdConfigs, err := ensureConfigRecords(ctx, tx, opts.Configs)
	if err != nil {
		return RecordRunDataResult{}, err
	}
	createdTags, err := ensureTagRecords(ctx, tx, opts.Tags)
	if err != nil {
		return RecordRunDataResult{}, err
	}
	if _, err := ensureRunExperimentIndexesTx(ctx, tx, opts.Run, opts.Tags, now); err != nil {
		return RecordRunDataResult{}, err
	}
	createdArtifacts, err := ensureArtifactRecords(ctx, tx, opts.Artifacts)
	if err != nil {
		return RecordRunDataResult{}, err
	}
	createdMetricFiles, err := ensureMetricFileRecords(ctx, tx, opts.MetricFiles)
	if err != nil {
		return RecordRunDataResult{}, err
	}
	metricSummaries := metricSummariesForFiles(opts.MetricSummaries, createdMetricFiles)
	createdMetricSummaries, err := ensureMetricSummaryRecordsTx(ctx, tx, metricSummaries, createdMetricFiles)
	if err != nil {
		return RecordRunDataResult{}, err
	}

	idempotencyCreated := false
	if opts.IdempotencyKey != "" {
		_, err := tx.ExecContext(ctx, `
INSERT INTO idempotency_keys(key, command, target_type, target_id, request_hash, created_at)
VALUES (?, ?, 'run', ?, ?, ?)
`, opts.IdempotencyKey, opts.Command, opts.Run.RunID, requestHash, now)
		if err != nil {
			return RecordRunDataResult{}, fmt.Errorf("record idempotency key: %w", err)
		}
		idempotencyCreated = true
	}

	cleanupMirrors, err := s.appendRecordMirrors(opts, runCreated, runContextCreated, createdConfigs, createdTags, createdArtifacts, createdMetricFiles, idempotencyCreated, requestHash, now)
	if err != nil {
		return RecordRunDataResult{}, err
	}
	if err := tx.Commit(); err != nil {
		if cleanupErr := cleanupMirrors(); cleanupErr != nil {
			return RecordRunDataResult{}, fmt.Errorf("commit experiment run data: %w; cleanup JSONL mirrors: %v", err, cleanupErr)
		}
		return RecordRunDataResult{}, err
	}

	return RecordRunDataResult{
		RunID:           opts.Run.RunID,
		CreatedRun:      runCreated,
		Reused:          !(runCreated || runContextCreated || len(createdConfigs) > 0 || len(createdTags) > 0 || len(createdArtifacts) > 0 || len(createdMetricFiles) > 0 || createdMetricSummaries > 0 || idempotencyCreated),
		IdempotencyKey:  opts.IdempotencyKey,
		RunContext:      runContextCreated,
		Configs:         len(createdConfigs),
		MetricFiles:     len(createdMetricFiles),
		MetricSummaries: createdMetricSummaries,
		Artifacts:       len(createdArtifacts),
		Tags:            len(createdTags),
	}, nil
}

func (s *Store) normalizeRunRecord(run RunRecord, now string) RunRecord {
	if run.Project == "" {
		run.Project = s.manifest.Project
	}
	if run.Project == "" {
		run.Project = "default"
	}
	if run.ExperimentID == "" {
		run.ExperimentID = s.manifest.ExperimentID
	}
	if run.RunGroupID == "" {
		run.RunGroupID = "default"
	}
	if run.State == "" {
		run.State = "succeeded"
	}
	if run.CreatedAt == "" {
		run.CreatedAt = now
	}
	if run.IndexVersion == "" {
		run.IndexVersion = SchemaVersion
	}
	return run
}

func validateRunRecord(run RunRecord) error {
	if err := validateID("run", run.RunID); err != nil {
		return err
	}
	if err := validateID("project", run.Project); err != nil {
		return err
	}
	if err := validateID("group", run.RunGroupID); err != nil {
		return err
	}
	if run.ParentRunID != "" {
		if err := validateID("parent run", run.ParentRunID); err != nil {
			return err
		}
	}
	if run.State == "" {
		return fmt.Errorf("run state is required")
	}
	if run.CreatedAt == "" {
		return fmt.Errorf("run created_at is required")
	}
	if run.IndexVersion == "" {
		return fmt.Errorf("run index_version is required")
	}
	return nil
}

func validateRecordCollections(opts RecordRunDataOptions) error {
	if opts.RunContext != nil {
		if opts.RunContext.RunID != opts.Run.RunID {
			return fmt.Errorf("run context run_id %q does not match run %q", opts.RunContext.RunID, opts.Run.RunID)
		}
	}
	for _, tag := range opts.Tags {
		if tag.ScopeType == "" || tag.ScopeID == "" || tag.Key == "" {
			return fmt.Errorf("tag scope_type, scope_id, and key are required")
		}
	}
	for _, config := range opts.Configs {
		if config.ConfigHash == "" || config.RunID == "" || config.Format == "" || config.URI == "" {
			return fmt.Errorf("config config_hash, run_id, format, and uri are required")
		}
		if config.RunID != opts.Run.RunID {
			return fmt.Errorf("config %q run_id %q does not match run %q", config.ConfigHash, config.RunID, opts.Run.RunID)
		}
	}
	for _, artifact := range opts.Artifacts {
		if err := validateID("artifact", artifact.ArtifactID); err != nil {
			return err
		}
		if artifact.RunID != opts.Run.RunID {
			return fmt.Errorf("artifact %q run_id %q does not match run %q", artifact.ArtifactID, artifact.RunID, opts.Run.RunID)
		}
		if artifact.Type == "" || artifact.URI == "" || artifact.Name == "" || artifact.CreatedAt == "" {
			return fmt.Errorf("artifact %q type, uri, name, and created_at are required", artifact.ArtifactID)
		}
		if artifact.Step != nil && *artifact.Step < 0 {
			return fmt.Errorf("artifact %q step must be non-negative", artifact.ArtifactID)
		}
		if artifact.Rank != nil && *artifact.Rank < 0 {
			return fmt.Errorf("artifact %q rank must be non-negative", artifact.ArtifactID)
		}
	}
	for _, metricFile := range opts.MetricFiles {
		if err := validateID("metric file", metricFile.FileID); err != nil {
			return err
		}
		if metricFile.Path == "" || metricFile.Format == "" || metricFile.SchemaVersion == "" || metricFile.CreatedAt == "" {
			return fmt.Errorf("metric file %q path, format, schema_version, and created_at are required", metricFile.FileID)
		}
		if metricFile.RunID != "" && metricFile.RunID != opts.Run.RunID {
			return fmt.Errorf("metric file %q run_id %q does not match run %q", metricFile.FileID, metricFile.RunID, opts.Run.RunID)
		}
	}
	for _, summary := range opts.MetricSummaries {
		if summary.RunID != "" && summary.RunID != opts.Run.RunID {
			return fmt.Errorf("metric summary %q run_id %q does not match run %q", summary.MetricName, summary.RunID, opts.Run.RunID)
		}
	}
	return nil
}

func ensureRunRecord(ctx context.Context, tx *sql.Tx, run RunRecord) (bool, error) {
	var existing RunRecord
	err := tx.QueryRowContext(ctx, `
SELECT run_id, project, run_group_id,
       coalesce(parent_run_id, ''), state, coalesce(owner, ''), created_at, coalesce(started_at, ''),
       coalesce(completed_at, ''), coalesce(config_hash, ''), coalesce(code_sha, ''),
       coalesce(image_digest, ''), coalesce(tau_command, ''), coalesce(result_uri, ''), index_version
FROM runs WHERE run_id = ?`, run.RunID).Scan(
		&existing.RunID,
		&existing.Project,
		&existing.RunGroupID,
		&existing.ParentRunID,
		&existing.State,
		&existing.Owner,
		&existing.CreatedAt,
		&existing.StartedAt,
		&existing.CompletedAt,
		&existing.ConfigHash,
		&existing.CodeSHA,
		&existing.ImageDigest,
		&existing.TauCommand,
		&existing.ResultURI,
		&existing.IndexVersion,
	)
	if err == nil {
		if !runRecordCompatible(existing, run) {
			return false, fmt.Errorf("%w: run %q already exists with different metadata", ErrConflict, run.RunID)
		}
		return false, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO runs(run_id, project, experiment_id, run_group_id, parent_run_id, state, owner,
                 created_at, started_at, completed_at, config_hash, code_sha, image_digest, tau_command,
                 result_uri, index_version)
VALUES (?, ?, ?, ?, nullif(?, ''), ?, nullif(?, ''),
        ?, nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''),
        nullif(?, ''), nullif(?, ''), ?)
`, run.RunID, run.Project, run.ExperimentID, run.RunGroupID, run.ParentRunID, run.State, run.Owner,
		run.CreatedAt, run.StartedAt, run.CompletedAt, run.ConfigHash, run.CodeSHA, run.ImageDigest, run.TauCommand, run.ResultURI, run.IndexVersion)
	if err != nil {
		return false, err
	}
	// runs.experiment_id above is the run -> experiment link, so the experiment
	// row has to exist for it to point at. Doing it here rather than leaving it
	// to the EnsureExperimentIndex backfill matters: that backfill falls back to
	// the run group, which would split an experiment's arms apart.
	if run.ExperimentID != "" {
		if _, err := ensureImplicitExperimentTx(ctx, tx, ExperimentRecord{
			ExperimentID: run.ExperimentID,
			Project:      run.Project,
			Name:         run.ExperimentID,
			Source:       "run",
			CreatedAt:    run.CreatedAt,
			UpdatedAt:    run.CreatedAt,
		}); err != nil {
			return false, err
		}
	}
	return true, nil
}

func ensureRunContextRecord(ctx context.Context, tx *sql.Tx, runContext *RunContextRecord) (bool, error) {
	if runContext == nil {
		return false, nil
	}
	var existing RunContextRecord
	var gpuCount sql.NullInt64
	var queueWait, gpuHours, estimatedCost sql.NullFloat64
	err := tx.QueryRowContext(ctx, `
SELECT run_id, coalesce(cluster, ''), coalesce(namespace, ''), coalesce(team, ''), coalesce(profile, ''),
       coalesce(lane, ''), coalesce(local_queue, ''), coalesce(cluster_queue, ''), coalesce(kueue_workload, ''),
       coalesce(pod_uid, ''), coalesce(ray_job, ''), coalesce(resource_claims, ''), coalesce(gpu_class, ''),
       gpu_count, coalesce(node_names, ''), coalesce(mounts, ''), queue_wait_seconds, gpu_hours, estimated_cost,
       coalesce(runtime, ''), coalesce(dependencies, ''), coalesce(log_uri, '')
FROM run_context WHERE run_id = ?`, runContext.RunID).Scan(
		&existing.RunID,
		&existing.Cluster,
		&existing.Namespace,
		&existing.Team,
		&existing.Profile,
		&existing.Lane,
		&existing.LocalQueue,
		&existing.ClusterQueue,
		&existing.KueueWorkload,
		&existing.PodUID,
		&existing.RayJob,
		&existing.ResourceClaims,
		&existing.GPUClass,
		&gpuCount,
		&existing.NodeNames,
		&existing.Mounts,
		&queueWait,
		&gpuHours,
		&estimatedCost,
		&existing.Runtime,
		&existing.Dependencies,
		&existing.LogURI,
	)
	if gpuCount.Valid {
		existing.GPUCount = &gpuCount.Int64
	}
	if queueWait.Valid {
		existing.QueueWaitSeconds = &queueWait.Float64
	}
	if gpuHours.Valid {
		existing.GPUHours = &gpuHours.Float64
	}
	if estimatedCost.Valid {
		existing.EstimatedCost = &estimatedCost.Float64
	}
	if err == nil {
		merged := mergeRunContextForEnrichment(existing, *runContext)
		if runContextRecordEqual(existing, merged) {
			return false, nil
		}
		if err := updateRunContextRecord(ctx, tx, merged); err != nil {
			return false, err
		}
		*runContext = merged
		return true, nil
	}
	if err != sql.ErrNoRows {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `
INSERT INTO run_context(run_id, cluster, namespace, team, profile, lane, local_queue, cluster_queue,
                        kueue_workload, pod_uid, ray_job, resource_claims, gpu_class, gpu_count,
                        node_names, mounts, queue_wait_seconds, gpu_hours, estimated_cost, runtime, dependencies, log_uri)
VALUES (?, nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''),
        nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''),
        ?, nullif(?, ''), nullif(?, ''), ?, ?, ?, nullif(?, ''), nullif(?, ''), nullif(?, ''))
`, runContext.RunID, runContext.Cluster, runContext.Namespace, runContext.Team, runContext.Profile, runContext.Lane,
		runContext.LocalQueue, runContext.ClusterQueue, runContext.KueueWorkload, runContext.PodUID, runContext.RayJob,
		runContext.ResourceClaims, runContext.GPUClass, nullableInt64(runContext.GPUCount), runContext.NodeNames, runContext.Mounts,
		nullableFloat64(runContext.QueueWaitSeconds), nullableFloat64(runContext.GPUHours), nullableFloat64(runContext.EstimatedCost),
		runContext.Runtime, runContext.Dependencies, runContext.LogURI)
	if err != nil {
		return false, err
	}
	return true, nil
}

func updateRunContextRecord(ctx context.Context, tx *sql.Tx, runContext RunContextRecord) error {
	_, err := tx.ExecContext(ctx, `
UPDATE run_context
SET cluster = nullif(?, ''), namespace = nullif(?, ''), team = nullif(?, ''),
    profile = nullif(?, ''), lane = nullif(?, ''), local_queue = nullif(?, ''),
    cluster_queue = nullif(?, ''), kueue_workload = nullif(?, ''), pod_uid = nullif(?, ''),
    ray_job = nullif(?, ''), resource_claims = nullif(?, ''), gpu_class = nullif(?, ''),
    gpu_count = ?, node_names = nullif(?, ''), mounts = nullif(?, ''),
    queue_wait_seconds = ?, gpu_hours = ?, estimated_cost = ?,
    runtime = nullif(?, ''), dependencies = nullif(?, ''), log_uri = nullif(?, '')
WHERE run_id = ?
`, runContext.Cluster, runContext.Namespace, runContext.Team, runContext.Profile, runContext.Lane, runContext.LocalQueue,
		runContext.ClusterQueue, runContext.KueueWorkload, runContext.PodUID, runContext.RayJob, runContext.ResourceClaims,
		runContext.GPUClass, nullableInt64(runContext.GPUCount), runContext.NodeNames, runContext.Mounts,
		nullableFloat64(runContext.QueueWaitSeconds), nullableFloat64(runContext.GPUHours), nullableFloat64(runContext.EstimatedCost),
		runContext.Runtime, runContext.Dependencies, runContext.LogURI, runContext.RunID)
	return err
}

func ensureConfigRecords(ctx context.Context, tx *sql.Tx, configs []ConfigRecord) ([]ConfigRecord, error) {
	created := []ConfigRecord{}
	for _, config := range configs {
		var existing ConfigRecord
		err := tx.QueryRowContext(ctx, `
SELECT config_hash, run_id, format, uri, coalesce(normalized_json, ''), coalesce(indexed_fields, '')
FROM configs WHERE config_hash = ? AND run_id = ?`, config.ConfigHash, config.RunID).Scan(
			&existing.ConfigHash,
			&existing.RunID,
			&existing.Format,
			&existing.URI,
			&existing.NormalizedJSON,
			&existing.IndexedFields,
		)
		if err == nil {
			if !configRecordEqual(existing, config) {
				return nil, fmt.Errorf("%w: config %q for run %q already exists with different metadata", ErrConflict, config.ConfigHash, config.RunID)
			}
			continue
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO configs(config_hash, run_id, format, uri, normalized_json, indexed_fields)
VALUES (?, ?, ?, ?, nullif(?, ''), nullif(?, ''))
`, config.ConfigHash, config.RunID, config.Format, config.URI, config.NormalizedJSON, config.IndexedFields)
		if err != nil {
			return nil, err
		}
		created = append(created, config)
	}
	return created, nil
}

func ensureTagRecords(ctx context.Context, tx *sql.Tx, tags []TagRecord) ([]TagRecord, error) {
	created := []TagRecord{}
	for _, tag := range tags {
		var existing string
		err := tx.QueryRowContext(ctx, `
SELECT value FROM tags WHERE scope_type = ? AND scope_id = ? AND key = ?`, tag.ScopeType, tag.ScopeID, tag.Key).Scan(&existing)
		if err == nil {
			if existing != tag.Value {
				return nil, fmt.Errorf("%w: tag %s/%s %q already exists with different value", ErrConflict, tag.ScopeType, tag.ScopeID, tag.Key)
			}
			continue
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO tags(scope_type, scope_id, key, value) VALUES (?, ?, ?, ?)`, tag.ScopeType, tag.ScopeID, tag.Key, tag.Value); err != nil {
			return nil, err
		}
		created = append(created, tag)
	}
	return created, nil
}

func ensureArtifactRecords(ctx context.Context, tx *sql.Tx, artifacts []ArtifactRecord) ([]ArtifactRecord, error) {
	created := []ArtifactRecord{}
	for _, artifact := range artifacts {
		var existing ArtifactRecord
		var size, step, rank sql.NullInt64
		err := tx.QueryRowContext(ctx, `
SELECT artifact_id, run_id, type, uri, name, coalesce(durable_ref, ''), coalesce(content_type, ''),
       coalesce(digest, ''), size_bytes, step, coalesce(tags, ''), rank, created_at, coalesce(preview, ''),
       coalesce(external_ref, ''), coalesce(caption, ''), coalesce(direction, ''),
       coalesce(alias, ''), coalesce(source_artifact_id, ''), coalesce(source_run_id, ''),
       coalesce(source_dataset_name, ''), coalesce(source_dataset_version, ''), coalesce(source_dataset_digest, '')
FROM artifacts WHERE artifact_id = ?`, artifact.ArtifactID).Scan(
			&existing.ArtifactID,
			&existing.RunID,
			&existing.Type,
			&existing.URI,
			&existing.Name,
			&existing.DurableRef,
			&existing.ContentType,
			&existing.Digest,
			&size,
			&step,
			&existing.Tags,
			&rank,
			&existing.CreatedAt,
			&existing.Preview,
			&existing.ExternalRef,
			&existing.Caption,
			&existing.Direction,
			&existing.Alias,
			&existing.SourceArtifactID,
			&existing.SourceRunID,
			&existing.SourceDatasetName,
			&existing.SourceDatasetVersion,
			&existing.SourceDatasetDigest,
		)
		if size.Valid {
			existing.SizeBytes = &size.Int64
		}
		if step.Valid {
			existing.Step = &step.Int64
		}
		if rank.Valid {
			existing.Rank = &rank.Int64
		}
		if err == nil {
			if !artifactRecordEqual(existing, artifact) {
				return nil, fmt.Errorf("%w: artifact %q already exists with different metadata", ErrConflict, artifact.ArtifactID)
			}
			continue
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO artifacts(artifact_id, run_id, type, uri, name, durable_ref, content_type, digest, size_bytes,
                      step, tags, rank, created_at, preview, external_ref,
                      caption, direction, alias, source_artifact_id, source_run_id,
                      source_dataset_name, source_dataset_version, source_dataset_digest)
VALUES (?, ?, ?, ?, ?, nullif(?, ''), nullif(?, ''), nullif(?, ''), ?,
        ?, nullif(?, ''), ?, ?, nullif(?, ''), nullif(?, ''),
        nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''), nullif(?, ''),
        nullif(?, ''), nullif(?, ''), nullif(?, ''))
`, artifact.ArtifactID, artifact.RunID, artifact.Type, artifact.URI, artifact.Name, artifact.DurableRef, artifact.ContentType, artifact.Digest,
			nullableInt64(artifact.SizeBytes), nullableInt64(artifact.Step), artifact.Tags, nullableInt64(artifact.Rank), artifact.CreatedAt,
			artifact.Preview, artifact.ExternalRef, artifact.Caption, artifact.Direction, artifact.Alias, artifact.SourceArtifactID, artifact.SourceRunID,
			artifact.SourceDatasetName, artifact.SourceDatasetVersion, artifact.SourceDatasetDigest)
		if err != nil {
			return nil, err
		}
		created = append(created, artifact)
	}
	return created, nil
}

func ensureMetricFileRecords(ctx context.Context, tx *sql.Tx, metricFiles []MetricFileRecord) ([]MetricFileRecord, error) {
	created := []MetricFileRecord{}
	for _, metricFile := range metricFiles {
		var existing MetricFileRecord
		var minStep, maxStep sql.NullInt64
		err := tx.QueryRowContext(ctx, `
SELECT file_id, path, format, schema_version, coalesce(schema_hash, ''), coalesce(project, ''),
       coalesce(run_group_id, ''),
       coalesce(run_id, ''), row_count, coalesce(digest, ''), min_step, max_step, created_at
FROM metric_files WHERE file_id = ?`, metricFile.FileID).Scan(
			&existing.FileID,
			&existing.Path,
			&existing.Format,
			&existing.SchemaVersion,
			&existing.SchemaHash,
			&existing.Project,
			&existing.RunGroupID,
			&existing.RunID,
			&existing.RowCount,
			&existing.Digest,
			&minStep,
			&maxStep,
			&existing.CreatedAt,
		)
		if minStep.Valid {
			existing.MinStep = &minStep.Int64
		}
		if maxStep.Valid {
			existing.MaxStep = &maxStep.Int64
		}
		if err == nil {
			if !metricFileRecordEqual(existing, metricFile) {
				return nil, fmt.Errorf("%w: metric file %q already exists with different metadata", ErrConflict, metricFile.FileID)
			}
			continue
		}
		if err != sql.ErrNoRows {
			return nil, err
		}
		_, err = tx.ExecContext(ctx, `
INSERT INTO metric_files(file_id, path, format, schema_version, schema_hash, project,
                         run_group_id, run_id, row_count, digest, min_step, max_step, created_at)
VALUES (?, ?, ?, ?, nullif(?, ''), nullif(?, ''),
        nullif(?, ''), nullif(?, ''), ?, nullif(?, ''), ?, ?, ?)
`, metricFile.FileID, metricFile.Path, metricFile.Format, metricFile.SchemaVersion, metricFile.SchemaHash,
			metricFile.Project, metricFile.RunGroupID, metricFile.RunID,
			metricFile.RowCount, metricFile.Digest, nullableInt64(metricFile.MinStep), nullableInt64(metricFile.MaxStep), metricFile.CreatedAt)
		if err != nil {
			return nil, err
		}
		created = append(created, metricFile)
	}
	return created, nil
}

func metricSummariesForFiles(summaries []MetricSummaryRecord, metricFiles []MetricFileRecord) []MetricSummaryRecord {
	if len(summaries) == 0 || len(metricFiles) == 0 {
		return nil
	}
	fileIDs := map[string]bool{}
	for _, metricFile := range metricFiles {
		fileIDs[metricFile.FileID] = true
	}
	out := make([]MetricSummaryRecord, 0, len(summaries))
	for _, summary := range summaries {
		if fileIDs[summary.FileID] {
			out = append(out, summary)
		}
	}
	return out
}

func ensureMetricSummaryRecordsTx(ctx context.Context, tx *sql.Tx, summaries []MetricSummaryRecord, metricFiles []MetricFileRecord) (int, error) {
	if len(metricFiles) == 0 {
		return 0, nil
	}
	now := time.Now().UTC().Format(time.RFC3339)
	created := 0
	summariesByFile := map[string][]MetricSummaryRecord{}
	for _, summary := range summaries {
		summariesByFile[summary.FileID] = append(summariesByFile[summary.FileID], summary)
	}
	for _, metricFile := range metricFiles {
		if metricFile.FileID == "" {
			continue
		}
		processed, err := metricSummaryFileProcessed(ctx, tx, metricFile.FileID)
		if err != nil {
			return 0, err
		}
		if processed {
			continue
		}
		fileSummaries := summariesByFile[metricFile.FileID]
		if len(fileSummaries) == 0 {
			continue
		}
		for _, summary := range fileSummaries {
			if err := mergeMetricSummaryRecord(ctx, tx, summary); err != nil {
				return 0, err
			}
			created++
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO metric_summary_files(file_id, run_id, summarized_at)
VALUES (?, nullif(?, ''), ?)`, metricFile.FileID, metricFile.RunID, now); err != nil {
			return 0, err
		}
	}
	return created, nil
}

func (s *Store) appendRecordMirrors(opts RecordRunDataOptions, runCreated, runContextCreated bool, configs []ConfigRecord, tags []TagRecord, artifacts []ArtifactRecord, metricFiles []MetricFileRecord, idempotencyCreated bool, requestHash, now string) (func() error, error) {
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
	if runCreated {
		if err := add("runs.jsonl", opts.Run); err != nil {
			return nil, err
		}
	}
	if runContextCreated {
		if err := add("run_context.jsonl", opts.RunContext); err != nil {
			return nil, err
		}
	}
	for _, config := range configs {
		if err := add("configs.jsonl", config); err != nil {
			return nil, err
		}
	}
	for _, tag := range tags {
		if err := add("tags.jsonl", tag); err != nil {
			return nil, err
		}
	}
	for _, artifact := range artifacts {
		if err := add("artifacts.jsonl", artifact); err != nil {
			return nil, err
		}
	}
	for _, metricFile := range metricFiles {
		if err := add("metric_files.jsonl", metricFile); err != nil {
			return nil, err
		}
	}
	if idempotencyCreated {
		if err := add("idempotency_keys.jsonl", map[string]string{
			"key":          opts.IdempotencyKey,
			"command":      opts.Command,
			"target_type":  "run",
			"target_id":    opts.Run.RunID,
			"request_hash": requestHash,
			"created_at":   now,
		}); err != nil {
			return nil, err
		}
	}
	return func() error {
		return cleanupJSONL(cleanups)
	}, nil
}

func recordRunDataRequestHash(opts RecordRunDataOptions) (string, error) {
	opts.IdempotencyKey = ""
	opts.RequestHash = ""
	raw, err := json.Marshal(opts)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func configRecordEqual(a, b ConfigRecord) bool {
	return a.ConfigHash == b.ConfigHash &&
		a.RunID == b.RunID &&
		a.Format == b.Format &&
		a.URI == b.URI &&
		a.NormalizedJSON == b.NormalizedJSON &&
		a.IndexedFields == b.IndexedFields
}

func artifactRecordEqual(a, b ArtifactRecord) bool {
	return a.ArtifactID == b.ArtifactID &&
		a.RunID == b.RunID &&
		a.Type == b.Type &&
		a.URI == b.URI &&
		a.Name == b.Name &&
		a.DurableRef == b.DurableRef &&
		a.ContentType == b.ContentType &&
		a.Digest == b.Digest &&
		int64PtrEqual(a.SizeBytes, b.SizeBytes) &&
		int64PtrEqual(a.Step, b.Step) &&
		a.Tags == b.Tags &&
		int64PtrEqual(a.Rank, b.Rank) &&
		a.CreatedAt == b.CreatedAt &&
		a.Preview == b.Preview &&
		a.ExternalRef == b.ExternalRef &&
		a.Caption == b.Caption &&
		a.Direction == b.Direction &&
		a.Alias == b.Alias &&
		a.SourceArtifactID == b.SourceArtifactID &&
		a.SourceRunID == b.SourceRunID &&
		a.SourceDatasetName == b.SourceDatasetName &&
		a.SourceDatasetVersion == b.SourceDatasetVersion &&
		a.SourceDatasetDigest == b.SourceDatasetDigest
}

func metricFileRecordEqual(a, b MetricFileRecord) bool {
	return a.FileID == b.FileID &&
		a.Path == b.Path &&
		a.Format == b.Format &&
		a.SchemaVersion == b.SchemaVersion &&
		a.SchemaHash == b.SchemaHash &&
		a.Project == b.Project &&
		a.RunGroupID == b.RunGroupID &&
		a.RunID == b.RunID &&
		a.RowCount == b.RowCount &&
		a.Digest == b.Digest &&
		int64PtrEqual(a.MinStep, b.MinStep) &&
		int64PtrEqual(a.MaxStep, b.MaxStep) &&
		a.CreatedAt == b.CreatedAt
}

func runContextRecordEqual(a, b RunContextRecord) bool {
	return a.RunID == b.RunID &&
		a.Cluster == b.Cluster &&
		a.Namespace == b.Namespace &&
		a.Team == b.Team &&
		a.Profile == b.Profile &&
		a.Lane == b.Lane &&
		a.LocalQueue == b.LocalQueue &&
		a.ClusterQueue == b.ClusterQueue &&
		a.KueueWorkload == b.KueueWorkload &&
		a.PodUID == b.PodUID &&
		a.RayJob == b.RayJob &&
		a.ResourceClaims == b.ResourceClaims &&
		a.GPUClass == b.GPUClass &&
		int64PtrEqual(a.GPUCount, b.GPUCount) &&
		a.NodeNames == b.NodeNames &&
		a.Mounts == b.Mounts &&
		float64PtrEqual(a.QueueWaitSeconds, b.QueueWaitSeconds) &&
		float64PtrEqual(a.GPUHours, b.GPUHours) &&
		float64PtrEqual(a.EstimatedCost, b.EstimatedCost) &&
		a.Runtime == b.Runtime &&
		a.Dependencies == b.Dependencies &&
		a.LogURI == b.LogURI
}

func runRecordCompatible(a, b RunRecord) bool {
	return a.RunID == b.RunID &&
		a.Project == b.Project &&
		a.RunGroupID == b.RunGroupID &&
		a.ParentRunID == b.ParentRunID &&
		a.State == b.State &&
		a.Owner == b.Owner &&
		a.ConfigHash == b.ConfigHash &&
		a.CodeSHA == b.CodeSHA &&
		a.ImageDigest == b.ImageDigest &&
		a.TauCommand == b.TauCommand &&
		a.ResultURI == b.ResultURI &&
		a.IndexVersion == b.IndexVersion
}

func int64PtrEqual(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func float64PtrEqual(a, b *float64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableFloat64(value *float64) any {
	if value == nil {
		return nil
	}
	return *value
}
