package expstore

import (
	"context"
	"database/sql"
	"fmt"
)

func (s *Store) Run(ctx context.Context, runID string) (RunRecord, error) {
	var run RunRecord
	err := s.db.QueryRowContext(ctx, `
SELECT run_id, project, coalesce(experiment_id, ''), run_group_id,
       coalesce(parent_run_id, ''), state, coalesce(owner, ''), created_at, coalesce(started_at, ''),
       coalesce(completed_at, ''), coalesce(config_hash, ''), coalesce(code_sha, ''),
       coalesce(image_digest, ''), coalesce(tau_command, ''), coalesce(result_uri, ''), index_version
FROM runs WHERE run_id = ?`, runID).Scan(
		&run.RunID,
		&run.Project,
		&run.ExperimentID,
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
		return RunRecord{}, ErrNotFound
	}
	if err != nil {
		return RunRecord{}, err
	}
	return run, nil
}

func (s *Store) ArtifactsForRun(ctx context.Context, runID string) ([]ArtifactRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT artifact_id, run_id, type, uri, name, coalesce(durable_ref, ''), coalesce(content_type, ''),
       coalesce(digest, ''), size_bytes, step, coalesce(tags, ''), rank, created_at, coalesce(preview, ''),
       coalesce(external_ref, ''), coalesce(caption, ''), coalesce(direction, ''), coalesce(alias, ''),
       coalesce(source_artifact_id, ''), coalesce(source_run_id, ''), coalesce(source_dataset_name, ''),
       coalesce(source_dataset_version, ''), coalesce(source_dataset_digest, '')
FROM artifacts WHERE run_id = ? ORDER BY created_at, artifact_id`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var artifacts []ArtifactRecord
	for rows.Next() {
		artifact, err := scanArtifactRecord(rows)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return artifacts, nil
}

func (s *Store) UpdateArtifactDurableRefs(ctx context.Context, artifacts []ArtifactRecord) error {
	if len(artifacts) == 0 {
		return nil
	}
	return s.withWriteLock(ctx, func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, artifact := range artifacts {
			result, err := tx.ExecContext(ctx, `
UPDATE artifacts
SET durable_ref = nullif(?, ''),
    content_type = nullif(?, ''),
    digest = nullif(?, ''),
    size_bytes = ?,
    step = ?,
    tags = nullif(?, ''),
    rank = ?
WHERE artifact_id = ? AND run_id = ?`,
				artifact.DurableRef,
				artifact.ContentType,
				artifact.Digest,
				nullableInt64(artifact.SizeBytes),
				nullableInt64(artifact.Step),
				artifact.Tags,
				nullableInt64(artifact.Rank),
				artifact.ArtifactID,
				artifact.RunID,
			)
			if err != nil {
				return err
			}
			rows, err := result.RowsAffected()
			if err != nil {
				return err
			}
			if rows == 0 {
				return fmt.Errorf("%w: artifact %q was not found for run %q", ErrNotFound, artifact.ArtifactID, artifact.RunID)
			}
		}
		var cleanups []func() error
		for _, artifact := range artifacts {
			cleanup, err := s.appendJSONLWithRollback("artifacts.jsonl", artifact)
			if err != nil {
				if cleanupErr := cleanupJSONL(cleanups); cleanupErr != nil {
					return fmt.Errorf("%w; cleanup artifact mirrors: %v", err, cleanupErr)
				}
				return err
			}
			cleanups = append(cleanups, cleanup)
		}
		if err := tx.Commit(); err != nil {
			if cleanupErr := cleanupJSONL(cleanups); cleanupErr != nil {
				return fmt.Errorf("commit artifact durable refs: %w; cleanup artifact mirrors: %v", err, cleanupErr)
			}
			return err
		}
		return nil
	})
}

type artifactScanner interface {
	Scan(dest ...any) error
}

func scanArtifactRecord(scanner artifactScanner) (ArtifactRecord, error) {
	var artifact ArtifactRecord
	var size, step, rank sql.NullInt64
	err := scanner.Scan(
		&artifact.ArtifactID,
		&artifact.RunID,
		&artifact.Type,
		&artifact.URI,
		&artifact.Name,
		&artifact.DurableRef,
		&artifact.ContentType,
		&artifact.Digest,
		&size,
		&step,
		&artifact.Tags,
		&rank,
		&artifact.CreatedAt,
		&artifact.Preview,
		&artifact.ExternalRef,
		&artifact.Caption,
		&artifact.Direction,
		&artifact.Alias,
		&artifact.SourceArtifactID,
		&artifact.SourceRunID,
		&artifact.SourceDatasetName,
		&artifact.SourceDatasetVersion,
		&artifact.SourceDatasetDigest,
	)
	if err != nil {
		return ArtifactRecord{}, err
	}
	if size.Valid {
		artifact.SizeBytes = &size.Int64
	}
	if step.Valid {
		artifact.Step = &step.Int64
	}
	if rank.Valid {
		artifact.Rank = &rank.Int64
	}
	return artifact, nil
}
