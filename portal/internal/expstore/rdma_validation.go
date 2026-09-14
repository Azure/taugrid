// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/rdmavalidation"
)

type RDMAValidationProjection struct {
	Run      RunRecord
	Tags     []TagRecord
	Artifact *ArtifactRecord
	Metrics  []MetricRow
}

// ProjectRDMAValidation creates records for the existing experiment store
// without writing them. A nil artifact link projects an in-progress RunRecord;
// a non-nil link projects the terminal run, immutable artifact, and summaries.
func ProjectRDMAValidation(
	result rdmavalidation.Result,
	link *rdmavalidation.ArtifactLink,
) (RDMAValidationProjection, error) {
	if err := exptelemetry.ValidateID("project_id", result.ProjectID); err != nil {
		return RDMAValidationProjection{}, err
	}
	if err := exptelemetry.ValidateID("run_group_id", result.RunGroupID); err != nil {
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
	projection := RDMAValidationProjection{
		Run: RunRecord{
			RunID:        result.RunID,
			Project:      result.ProjectID,
			ExperimentID: result.ExperimentID,
			RunGroupID:   result.RunGroupID,
			State:        lifecycle.State,
			Owner:        result.Producer.Name,
			CreatedAt:    formatRDMATime(lifecycle.CreatedAt),
			StartedAt:    formatRDMATime(lifecycle.StartedAt),
			CompletedAt:  formatRDMATime(lifecycle.CompletedAt),
			CodeSHA:      result.Source.Revision,
			ImageDigest:  result.Image.PlatformDigest,
			IndexVersion: SchemaVersion,
		},
		Tags: []TagRecord{{
			ScopeType: "run",
			ScopeID:   result.RunID,
			Key:       rdmavalidation.RunKindTag,
			Value:     rdmavalidation.Kind,
		}},
	}
	if link == nil {
		return projection, nil
	}
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
	projection.Metrics = make([]MetricRow, 0, len(metrics))
	for _, metric := range metrics {
		tags := cloneMetricTags(metric.Tags)
		tags[rdmavalidation.MetricArtifactURITag] = metric.ArtifactURI
		tags[rdmavalidation.MetricArtifactSHA256Tag] = metric.ArtifactSHA256
		rawTags, err := json.Marshal(tags)
		if err != nil {
			return RDMAValidationProjection{}, fmt.Errorf("marshal RDMA validation metric tags: %w", err)
		}
		step := int64(metric.Attempt)
		wallTime := metric.ObservedAt.UnixMicro()
		projection.Metrics = append(projection.Metrics, MetricRow{
			Project:    metric.ProjectID,
			RunGroupID: metric.RunGroupID,
			RunID:      metric.RunID,
			MetricName: metric.Name,
			Step:       &step,
			WallTime:   &wallTime,
			Value:      metric.Value,
			Unit:       rdmaMetricUnit(metric.Name),
			Source:     rdmavalidation.Kind,
			Tags:       string(rawTags),
		})
	}
	return projection, nil
}

func formatRDMATime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func cloneMetricTags(tags map[string]string) map[string]string {
	cloned := make(map[string]string, len(tags)+2)
	for key, value := range tags {
		cloned[key] = value
	}
	return cloned
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
