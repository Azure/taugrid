// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import "time"

const (
	MetricStatus                 = "rdma_validation/status"
	MetricAlgBWGbps              = "rdma_validation/algbw_gbps"
	MetricBusBWGbps              = "rdma_validation/busbw_gbps"
	MetricMaxError               = "rdma_validation/max_error"
	MetricDurationSeconds        = "rdma_validation/duration_seconds"
	MetricNetIBObserved          = "rdma_validation/net_ib_observed"
	MetricSocketFallbackObserved = "rdma_validation/socket_fallback_observed"
	MetricCleanupComplete        = "rdma_validation/cleanup_complete"
)

type SummaryMetric struct {
	ValidationID   string            `json:"validation_id"`
	RunID          string            `json:"run_id"`
	Attempt        int               `json:"attempt"`
	WorkspaceID    string            `json:"workspace_id"`
	Cluster        string            `json:"cluster"`
	Namespace      string            `json:"namespace"`
	ProjectID      string            `json:"project_id,omitempty"`
	ExperimentID   string            `json:"experiment_id,omitempty"`
	RunGroupID     string            `json:"run_group_id,omitempty"`
	Name           string            `json:"name"`
	Value          float64           `json:"value"`
	ObservedAt     time.Time         `json:"observed_at"`
	Tags           map[string]string `json:"tags"`
	ArtifactURI    string            `json:"artifact_uri,omitempty"`
	ArtifactSHA256 string            `json:"artifact_sha256,omitempty"`
}

func (result Result) SummaryMetrics() []SummaryMetric {
	statusValue := 0.0
	switch result.Status {
	case StatusPass:
		statusValue = 1
	case StatusFail:
		statusValue = -1
	}
	metrics := []SummaryMetric{result.summaryMetric(MetricStatus, statusValue)}
	if result.Measurements.AlgBWGbps.Min != nil {
		metrics = append(metrics, result.summaryMetric(MetricAlgBWGbps, *result.Measurements.AlgBWGbps.Min))
	}
	if result.Measurements.BusBWGbps.Min != nil {
		metrics = append(metrics, result.summaryMetric(MetricBusBWGbps, *result.Measurements.BusBWGbps.Min))
	}
	if result.Correctness.MaxError != nil {
		metrics = append(metrics, result.summaryMetric(MetricMaxError, *result.Correctness.MaxError))
	}
	if !result.StartedAt.IsZero() && !result.CompletedAt.IsZero() && !result.CompletedAt.Before(result.StartedAt) {
		metrics = append(metrics, result.summaryMetric(MetricDurationSeconds, result.CompletedAt.Sub(result.StartedAt).Seconds()))
	}
	if result.Transport.PositiveIBEvidence != nil {
		metrics = append(metrics, result.summaryMetric(MetricNetIBObserved, boolMetric(*result.Transport.PositiveIBEvidence)))
	}
	if result.Transport.SocketFallbackObserved != nil {
		metrics = append(metrics, result.summaryMetric(MetricSocketFallbackObserved, boolMetric(*result.Transport.SocketFallbackObserved)))
	}
	switch result.Cleanup.State {
	case CleanupComplete:
		metrics = append(metrics, result.summaryMetric(MetricCleanupComplete, 1))
	case CleanupIncomplete:
		metrics = append(metrics, result.summaryMetric(MetricCleanupComplete, 0))
	}
	return metrics
}

func (result Result) SummaryMetricsForArtifact(link ArtifactLink) ([]SummaryMetric, error) {
	if _, err := result.Lifecycle(&link); err != nil {
		return nil, err
	}
	metrics := result.SummaryMetrics()
	for index := range metrics {
		metrics[index].ArtifactURI = link.URI
		metrics[index].ArtifactSHA256 = link.SHA256
	}
	return metrics, nil
}

func (result Result) summaryMetric(name string, value float64) SummaryMetric {
	tags := map[string]string{
		MetricValidationIDTag:     result.ValidationID,
		MetricSchemaTag:           SchemaVersion,
		MetricKindTag:             Kind,
		MetricValidationStatusTag: string(result.Status),
		MetricValidationReasonTag: string(result.Reason),
	}
	if result.NCCL.Operation != "" {
		tags["operation"] = result.NCCL.Operation
	}
	if result.Transport.Backend != "" {
		tags["backend"] = result.Transport.Backend
	}
	return SummaryMetric{
		ValidationID: result.ValidationID, RunID: result.RunID, Attempt: result.Attempt,
		WorkspaceID: result.WorkspaceID, Cluster: result.Cluster, Namespace: result.Namespace,
		ProjectID: result.ProjectID, ExperimentID: result.ExperimentID, RunGroupID: result.RunGroupID,
		Name: name, Value: value, ObservedAt: result.ObservedAt, Tags: tags,
	}
}

func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
