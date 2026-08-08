// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/portal/internal/expimport"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

type metricsCompletionStatus struct {
	State         string `json:"state"`
	Reason        string `json:"reason,omitempty"`
	Message       string `json:"message,omitempty"`
	ArtifactURI   string `json:"artifact_uri,omitempty"`
	CheckpointURI string `json:"checkpoint_uri,omitempty"`
	CompletedAt   string `json:"completed_at,omitempty"`
}

type metricsStatusMarkerResult struct {
	State       string
	MetricsFile string
	Samples     int
	RemoteWrite *expimport.RemoteWriteResult
}

type metricsRunStatusRow struct {
	SourceStoreID string  `json:"source_store_id,omitempty"`
	Project       string  `json:"project"`
	ExperimentID  string  `json:"experiment_id"`
	RunGroupID    string  `json:"run_group_id"`
	RunID         string  `json:"run_id"`
	MetricName    string  `json:"metric_name"`
	Step          int64   `json:"step"`
	WallTime      string  `json:"wall_time"`
	Value         float64 `json:"value"`
	Source        string  `json:"source"`
	ExportedAt    string  `json:"exported_at"`
	MetricFileID  string  `json:"metric_file_id"`
	Tags          string  `json:"tags,omitempty"`
}

func emitMetricsCompletionStatus(ctx context.Context, store *expstore.Store, opts metricsOffloadOptions, result metricsOffloadResult) (metricsStatusMarkerResult, error) {
	status, err := readMetricsCompletionStatus(opts.CompletionFile)
	if err != nil {
		return metricsStatusMarkerResult{}, err
	}
	return emitMetricsRunStatus(ctx, store, opts, status, result)
}

func emitMetricsRunStatus(ctx context.Context, store *expstore.Store, opts metricsOffloadOptions, status metricsCompletionStatus, result metricsOffloadResult) (metricsStatusMarkerResult, error) {
	if strings.TrimSpace(status.ArtifactURI) == "" {
		status.ArtifactURI = strings.TrimSpace(opts.ArtifactURI)
	}
	if strings.TrimSpace(status.CheckpointURI) == "" {
		status.CheckpointURI = strings.TrimSpace(opts.CheckpointURI)
	}
	normalized, err := normalizeMetricsCompletionState(status.State)
	if err != nil {
		return metricsStatusMarkerResult{}, err
	}
	status.State = normalized
	row, err := metricsCompletionStatusRow(store, opts, status, result)
	if err != nil {
		return metricsStatusMarkerResult{}, err
	}
	out := strings.TrimSpace(opts.Out)
	if out == "" {
		return metricsStatusMarkerResult{}, fmt.Errorf("--out is required")
	}
	statusDir := filepath.Join(out, "metrics-status")
	identity, err := json.Marshal(row)
	if err != nil {
		return metricsStatusMarkerResult{}, err
	}
	digest := sha256.Sum256(identity)
	fileID := fmt.Sprintf("run-status-%s-%x", fileutil.SafePathComponent(row.RunID), digest[:6])
	row.MetricFileID = fileID
	metricsFile := filepath.Join(statusDir, fileID+".jsonl")
	raw, err := json.Marshal(row)
	if err != nil {
		return metricsStatusMarkerResult{}, err
	}
	raw = append(raw, '\n')
	if err := fileutil.WriteFileAtomic(metricsFile, raw, 0o644); err != nil {
		return metricsStatusMarkerResult{}, err
	}
	statusResult := metricsStatusMarkerResult{
		State:       status.State,
		MetricsFile: metricsFile,
		Samples:     1,
	}
	if strings.TrimSpace(opts.RemoteWrite.Endpoint) != "" {
		remote, err := expimport.ReplayMetricsRemoteWrite(ctx, expimport.RemoteWriteOptions{
			MetricsFile:     metricsFile,
			Endpoint:        opts.RemoteWrite.Endpoint,
			BatchSize:       opts.RemoteWrite.BatchSize,
			MaxAttempts:     opts.RemoteWrite.MaxAttempts,
			RetryBackoff:    opts.RemoteWrite.RetryBackoff,
			CheckpointFile:  filepath.Join(statusDir, fileID+"_remote_write_checkpoint.json"),
			SkipIfCompleted: true,
		})
		if err != nil {
			return metricsStatusMarkerResult{}, err
		}
		statusResult.RemoteWrite = &remote
	}
	return statusResult, nil
}

func readMetricsCompletionStatus(path string) (metricsCompletionStatus, error) {
	status := metricsCompletionStatus{State: "succeeded"}
	path = strings.TrimSpace(path)
	if path == "" {
		return status, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return metricsCompletionStatus{}, err
	}
	rawText := strings.TrimSpace(string(raw))
	if rawText == "" {
		return status, nil
	}
	if strings.HasPrefix(rawText, "{") {
		if err := json.Unmarshal(raw, &status); err != nil {
			return metricsCompletionStatus{}, fmt.Errorf("read completion status %s: %w", path, err)
		}
	} else {
		status.State = rawText
	}
	normalized, err := normalizeMetricsCompletionState(status.State)
	if err != nil {
		return metricsCompletionStatus{}, fmt.Errorf("read completion status %s: %w", path, err)
	}
	status.State = normalized
	return status, nil
}

func metricsCompletionStatusRow(store *expstore.Store, opts metricsOffloadOptions, status metricsCompletionStatus, result metricsOffloadResult) (metricsRunStatusRow, error) {
	if strings.TrimSpace(opts.RunID) == "" {
		return metricsRunStatusRow{}, fmt.Errorf("--run is required with --completion-file")
	}
	value, err := metricsCompletionStateValue(status.State)
	if err != nil {
		return metricsRunStatusRow{}, err
	}
	observedAt := strings.TrimSpace(status.CompletedAt)
	if observedAt == "" {
		observedAt = time.Now().UTC().Format(time.RFC3339)
	} else if parsed, err := time.Parse(time.RFC3339Nano, observedAt); err == nil {
		observedAt = parsed.UTC().Format(time.RFC3339Nano)
	} else {
		return metricsRunStatusRow{}, fmt.Errorf("completion status completed_at must be RFC3339: %w", err)
	}
	manifest := store.Manifest()
	tags, err := metricsCompletionStatusTags(status, opts.Tags)
	if err != nil {
		return metricsRunStatusRow{}, err
	}
	return metricsRunStatusRow{
		SourceStoreID: result.SourceStoreID,
		Project:       firstNonEmpty(opts.Project, manifest.Project, "default"),
		ExperimentID:  metricsOffloadExperimentID(opts),
		RunGroupID:    firstNonEmpty(opts.RunGroupID, "default"),
		RunID:         strings.TrimSpace(opts.RunID),
		MetricName:    expkusto.RunStatusMetricName,
		Step:          0,
		WallTime:      observedAt,
		Value:         value,
		Source:        firstNonEmpty(opts.Source, "stellar-online") + "-status",
		ExportedAt:    observedAt,
		Tags:          tags,
	}, nil
}

func metricsCompletionStatusTags(status metricsCompletionStatus, rawTags []string) (string, error) {
	tags, err := parseExpTags(rawTags)
	if err != nil {
		return "", err
	}
	if tags == nil {
		tags = map[string]string{}
	}
	tags[expkusto.RunStatusStateTag] = status.State
	if status.Reason != "" {
		tags[expkusto.RunStatusReasonTag] = status.Reason
	}
	if status.Message != "" {
		tags[expkusto.RunStatusMessageTag] = status.Message
	}
	if status.ArtifactURI != "" {
		tags[expkusto.RunStatusArtifactURITag] = status.ArtifactURI
	}
	if status.CheckpointURI != "" {
		tags[expkusto.RunStatusCheckpointURITag] = status.CheckpointURI
	}
	raw, err := json.Marshal(tags)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

func normalizeMetricsCompletionState(state string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "success", "successful", "succeeded", "complete", "completed", "done":
		return "succeeded", nil
	case "fail", "failed", "failure", "error", "errored":
		return "failed", nil
	case "cancel", "cancelled", "canceled":
		return "cancelled", nil
	default:
		return "", fmt.Errorf("state must be succeeded, failed, or cancelled")
	}
}

func metricsCompletionStateValue(state string) (float64, error) {
	switch state {
	case "succeeded":
		return 1, nil
	case "failed":
		return -1, nil
	case "cancelled":
		return -2, nil
	default:
		return 0, fmt.Errorf("unsupported completion state %q", state)
	}
}
