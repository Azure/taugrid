// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"database/sql"
	"strings"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const experimentFaultEvidenceLimit = 1000

// ExperimentRunEvidence is the durable run timing and allocation evidence
// available for correlating one experiment with current cluster signals.
type ExperimentRunEvidence struct {
	RunID       string
	CreatedAt   string
	StartedAt   string
	CompletedAt string
	Context     *RunContextRecord
}

// ExperimentFaultEvidence returns workspace-scoped run timing and allocation
// evidence for one experiment. Truncated is true when the bounded query omitted
// runs, which callers must treat as partial allocation coverage.
func (s *Store) ExperimentFaultEvidence(
	ctx context.Context,
	experimentID string,
	workspace string,
) (evidence []ExperimentRunEvidence, truncated bool, err error) {
	experimentID = strings.TrimSpace(experimentID)
	workspace = strings.TrimSpace(workspace)
	rows, err := s.db.QueryContext(ctx, `
SELECT r.run_id, r.created_at, coalesce(r.started_at, ''), coalesce(r.completed_at, ''),
       rc.run_id, coalesce(rc.cluster, ''), coalesce(rc.namespace, ''), coalesce(rc.team, ''),
       coalesce(rc.profile, ''), coalesce(rc.lane, ''), coalesce(rc.local_queue, ''),
       coalesce(rc.cluster_queue, ''), coalesce(rc.kueue_workload, ''), coalesce(rc.pod_uid, ''),
       coalesce(rc.ray_job, ''), coalesce(rc.resource_claims, ''), coalesce(rc.gpu_class, ''),
       rc.gpu_count, coalesce(rc.node_names, ''), coalesce(rc.mounts, ''), rc.queue_wait_seconds,
       rc.gpu_hours, rc.estimated_cost, coalesce(rc.runtime, ''), coalesce(rc.dependencies, ''),
       coalesce(rc.log_uri, '')
FROM runs r
LEFT JOIN run_context rc ON rc.run_id = r.run_id
WHERE r.experiment_id = ?
  AND (? = '' OR EXISTS (
    SELECT 1 FROM tags workspace_tag
    WHERE workspace_tag.scope_type = 'run'
      AND workspace_tag.scope_id = r.run_id
      AND workspace_tag.key = ?
      AND workspace_tag.value = ?
  ) OR NOT EXISTS (
    SELECT 1 FROM tags workspace_tag
    WHERE workspace_tag.scope_type = 'run'
      AND workspace_tag.scope_id = r.run_id
      AND workspace_tag.key = ?
  ))
ORDER BY r.created_at, r.run_id
LIMIT ?`,
		experimentID,
		workspace,
		exptelemetry.TauWorkspaceTag,
		workspace,
		exptelemetry.TauWorkspaceTag,
		experimentFaultEvidenceLimit+1,
	)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	for rows.Next() {
		var item ExperimentRunEvidence
		var contextRunID sql.NullString
		var runContext RunContextRecord
		var gpuCount sql.NullInt64
		var queueWait, gpuHours, estimatedCost sql.NullFloat64
		if err := rows.Scan(
			&item.RunID,
			&item.CreatedAt,
			&item.StartedAt,
			&item.CompletedAt,
			&contextRunID,
			&runContext.Cluster,
			&runContext.Namespace,
			&runContext.Team,
			&runContext.Profile,
			&runContext.Lane,
			&runContext.LocalQueue,
			&runContext.ClusterQueue,
			&runContext.KueueWorkload,
			&runContext.PodUID,
			&runContext.RayJob,
			&runContext.ResourceClaims,
			&runContext.GPUClass,
			&gpuCount,
			&runContext.NodeNames,
			&runContext.Mounts,
			&queueWait,
			&gpuHours,
			&estimatedCost,
			&runContext.Runtime,
			&runContext.Dependencies,
			&runContext.LogURI,
		); err != nil {
			return nil, false, err
		}
		if contextRunID.Valid {
			runContext.RunID = contextRunID.String
			if gpuCount.Valid {
				runContext.GPUCount = &gpuCount.Int64
			}
			if queueWait.Valid {
				runContext.QueueWaitSeconds = &queueWait.Float64
			}
			if gpuHours.Valid {
				runContext.GPUHours = &gpuHours.Float64
			}
			if estimatedCost.Valid {
				runContext.EstimatedCost = &estimatedCost.Float64
			}
			item.Context = &runContext
		}
		evidence = append(evidence, item)
		if len(evidence) > experimentFaultEvidenceLimit {
			evidence = evidence[:experimentFaultEvidenceLimit]
			truncated = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	return evidence, truncated, nil
}
