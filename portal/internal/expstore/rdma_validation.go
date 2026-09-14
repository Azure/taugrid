// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/rdmavalidation"
)

const maxRDMAMetricStep = int64(1<<63 - 1)

const (
	maxRDMAMetricTags       = 14
	maxRDMAMetricTagValue   = 256
	maxRDMAArtifactURIBytes = 2048
	defaultRDMADimension    = "default"
)

type RDMAValidationProjection struct {
	Phase          string
	Step           int64
	IdempotencyKey string
	MetricFileID   string
	RequestHash    string
	Run            RunRecord
	Tags           []TagRecord
	Artifact       *ArtifactRecord
	Metrics        []MetricRow
}

// ProjectRDMAValidation creates records for the existing experiment store
// without writing them. A nil artifact link projects an in-progress RunRecord;
// a non-nil link projects the terminal run, immutable artifact, and summaries.
func ProjectRDMAValidation(
	result rdmavalidation.Result,
	link *rdmavalidation.ArtifactLink,
) (RDMAValidationProjection, error) {
	for kind, value := range map[string]string{
		"workspace_id":  result.WorkspaceID,
		"cluster":       result.Cluster,
		"namespace":     result.Namespace,
		"validation_id": result.ValidationID,
	} {
		if err := exptelemetry.ValidateID(kind, value); err != nil {
			return RDMAValidationProjection{}, err
		}
	}
	projectID := result.ProjectID
	if projectID == "" {
		projectID = defaultRDMADimension
	} else if err := exptelemetry.ValidateID("project_id", projectID); err != nil {
		return RDMAValidationProjection{}, err
	}
	runGroupID := result.RunGroupID
	if runGroupID == "" {
		runGroupID = defaultRDMADimension
	} else if err := exptelemetry.ValidateID("run_group_id", runGroupID); err != nil {
		return RDMAValidationProjection{}, err
	}
	if result.ExperimentID != "" {
		if err := exptelemetry.ValidateID("experiment_id", result.ExperimentID); err != nil {
			return RDMAValidationProjection{}, err
		}
	}
	lifecycle, err := result.Lifecycle(link)
	if err != nil {
		return RDMAValidationProjection{}, err
	}
	phase, eventTime, step, err := rdmaProjectionPhase(lifecycle, result.Attempt)
	if err != nil {
		return RDMAValidationProjection{}, err
	}
	projectedResult := result
	projectedResult.ProjectID = projectID
	projectedResult.RunGroupID = runGroupID
	projection := RDMAValidationProjection{
		Phase: phase,
		Step:  step,
		Run: RunRecord{
			RunID:        result.RunID,
			Project:      projectID,
			ExperimentID: result.ExperimentID,
			RunGroupID:   runGroupID,
			State:        lifecycle.State,
			Owner:        result.Producer.Name,
			CreatedAt:    formatRDMATime(lifecycle.CreatedAt),
			StartedAt:    formatRDMATime(lifecycle.StartedAt),
			CompletedAt:  formatRDMATime(lifecycle.CompletedAt),
			CodeSHA:      result.Source.Revision,
			ImageDigest:  result.Image.PlatformDigest,
			IndexVersion: SchemaVersion,
		},
		Tags: rdmaValidationRunTags(projectedResult),
	}
	statusValue := 0.0
	if link != nil {
		if lifecycle.State == rdmavalidation.RunStateSucceeded {
			statusValue = 1
		} else {
			statusValue = -1
		}
	}
	statusTags := rdmaValidationMetricTags(result, lifecycle.State)
	statusTags[exptelemetry.RunStatusStateTag] = lifecycle.State
	if link != nil {
		statusTags[exptelemetry.RunStatusReasonTag] = string(result.Reason)
		statusTags[rdmavalidation.MetricArtifactURITag] = link.URI
		statusTags[rdmavalidation.MetricArtifactSHA256Tag] = link.SHA256
	}
	statusRow, err := rdmaValidationMetricRow(
		projectedResult, exptelemetry.RunStatusMetricName, statusValue, nil, step, eventTime, statusTags,
	)
	if err != nil {
		return RDMAValidationProjection{}, err
	}
	projection.Metrics = []MetricRow{statusRow}
	if link != nil {
		projection.Run.ResultURI = link.URI
		size := link.SizeBytes
		projection.Artifact = &ArtifactRecord{
			ArtifactID:  result.ValidationID + "-result",
			RunID:       result.RunID,
			Type:        rdmavalidation.ArtifactType,
			URI:         link.URI,
			Name:        result.ValidationID + ".json",
			ContentType: rdmavalidation.ArtifactContentType,
			Digest:      link.SHA256,
			SizeBytes:   &size,
			CreatedAt:   formatRDMATime(link.FinalizedAt),
		}
		metrics, err := result.SummaryMetricsForArtifact(*link)
		if err != nil {
			return RDMAValidationProjection{}, err
		}
		for _, metric := range metrics {
			tags := rdmaValidationMetricTags(result, lifecycle.State)
			for key, value := range metric.Tags {
				tags[key] = value
			}
			tags[rdmavalidation.MetricArtifactURITag] = metric.ArtifactURI
			tags[rdmavalidation.MetricArtifactSHA256Tag] = metric.ArtifactSHA256
			row, err := rdmaValidationMetricRow(
				projectedResult, metric.Name, metric.Value, rdmaMetricUnit(metric.Name), step, metric.ObservedAt, tags,
			)
			if err != nil {
				return RDMAValidationProjection{}, err
			}
			projection.Metrics = append(projection.Metrics, row)
		}
	}
	identity := result.WorkspaceID + "-" + result.ValidationID +
		"-attempt-" + strconv.Itoa(result.Attempt) + "-" + phase
	projection.IdempotencyKey = identity
	projection.MetricFileID = identity + "-metrics"
	projection.RequestHash, err = rdmaValidationProjectionHash(projection)
	if err != nil {
		return RDMAValidationProjection{}, err
	}
	return projection, nil
}

func rdmaProjectionPhase(lifecycle rdmavalidation.RunLifecycle, attempt int) (string, time.Time, int64, error) {
	var phase string
	var eventTime time.Time
	var offset int64
	switch lifecycle.State {
	case rdmavalidation.RunStatePending:
		phase, eventTime, offset = "pending", lifecycle.CreatedAt, 0
	case rdmavalidation.RunStateRunning:
		phase, eventTime, offset = "running", lifecycle.StartedAt, 1
	case rdmavalidation.RunStateSucceeded, rdmavalidation.RunStateFailed:
		phase, eventTime, offset = "terminal", lifecycle.CompletedAt, 2
	default:
		return "", time.Time{}, 0, fmt.Errorf("unsupported RDMA validation lifecycle state %q", lifecycle.State)
	}
	if eventTime.IsZero() {
		return "", time.Time{}, 0, fmt.Errorf("%s lifecycle event time is required", phase)
	}
	attemptBase := int64(attempt - 1)
	if attemptBase > (maxRDMAMetricStep-offset)/3 {
		return "", time.Time{}, 0, fmt.Errorf("attempt is too large to encode a metric step")
	}
	return phase, eventTime, attemptBase*3 + offset, nil
}

func rdmaValidationRunTags(result rdmavalidation.Result) []TagRecord {
	values := []struct {
		key   string
		value string
	}{
		{exptelemetry.TauWorkspaceTag, result.WorkspaceID},
		{exptelemetry.TauClusterTag, result.Cluster},
		{exptelemetry.TauNamespaceTag, result.Namespace},
		{rdmavalidation.RunKindTag, rdmavalidation.Kind},
		{rdmavalidation.MetricValidationIDTag, result.ValidationID},
		{rdmavalidation.MetricSchemaTag, rdmavalidation.SchemaVersion},
	}
	tags := make([]TagRecord, 0, len(values))
	for _, value := range values {
		tags = append(tags, TagRecord{
			ScopeType: "run", ScopeID: result.RunID, Key: value.key, Value: value.value,
		})
	}
	return tags
}

func rdmaValidationMetricTags(result rdmavalidation.Result, lifecycleState string) map[string]string {
	tags := map[string]string{
		exptelemetry.TauWorkspaceTag:           result.WorkspaceID,
		exptelemetry.TauClusterTag:             result.Cluster,
		exptelemetry.TauNamespaceTag:           result.Namespace,
		rdmavalidation.MetricValidationIDTag:   result.ValidationID,
		rdmavalidation.MetricSchemaTag:         rdmavalidation.SchemaVersion,
		rdmavalidation.MetricKindTag:           rdmavalidation.Kind,
		rdmavalidation.MetricLifecycleStateTag: lifecycleState,
	}
	if lifecycleState == rdmavalidation.RunStateSucceeded || lifecycleState == rdmavalidation.RunStateFailed {
		tags[rdmavalidation.MetricValidationStatusTag] = string(result.Status)
		tags[rdmavalidation.MetricValidationReasonTag] = string(result.Reason)
	}
	return tags
}

func rdmaValidationMetricRow(
	result rdmavalidation.Result,
	name string,
	value float64,
	unit *string,
	step int64,
	eventTime time.Time,
	tags map[string]string,
) (MetricRow, error) {
	if len(tags) > maxRDMAMetricTags {
		return MetricRow{}, fmt.Errorf("RDMA validation metric tags exceed the fixed limit of %d", maxRDMAMetricTags)
	}
	for key, tagValue := range tags {
		limit := maxRDMAMetricTagValue
		if key == rdmavalidation.MetricArtifactURITag {
			limit = maxRDMAArtifactURIBytes
		}
		if len(key) > 128 || len(tagValue) > limit {
			return MetricRow{}, fmt.Errorf("RDMA validation metric tag %q exceeds its fixed size limit", key)
		}
	}
	rawTags, err := json.Marshal(tags)
	if err != nil {
		return MetricRow{}, fmt.Errorf("marshal RDMA validation metric tags: %w", err)
	}
	wallTime := eventTime.UnixMicro()
	return MetricRow{
		Project: result.ProjectID, RunGroupID: result.RunGroupID, RunID: result.RunID,
		MetricName: name, Step: &step, WallTime: &wallTime, Value: value, Unit: unit,
		Source: rdmavalidation.Kind, Tags: string(rawTags),
	}, nil
}

func rdmaValidationProjectionHash(projection RDMAValidationProjection) (string, error) {
	payload := struct {
		Phase          string
		Step           int64
		IdempotencyKey string
		MetricFileID   string
		Run            RunRecord
		Tags           []TagRecord
		Artifact       *ArtifactRecord
		Metrics        []MetricRow
	}{
		Phase: projection.Phase, Step: projection.Step,
		IdempotencyKey: projection.IdempotencyKey, MetricFileID: projection.MetricFileID,
		Run: projection.Run, Tags: projection.Tags, Artifact: projection.Artifact, Metrics: projection.Metrics,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal RDMA validation projection hash: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func formatRDMATime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func rdmaMetricUnit(name string) *string {
	var unit string
	switch name {
	case rdmavalidation.MetricAlgBWGbps, rdmavalidation.MetricBusBWGbps:
		unit = "GB/s"
	case rdmavalidation.MetricDurationSeconds:
		unit = "s"
	default:
		return nil
	}
	return &unit
}
