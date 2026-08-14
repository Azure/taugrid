// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expimport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/golang/snappy"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const (
	defaultRemoteWriteBatchSize  = 5000
	metricsRemoteWriteUserAgent  = "tau-exp-metrics-offload"
	metricsRemoteWriteCheckpoint = "tau.metrics.remote_write.v1"
	remoteWriteMetricName        = exptelemetry.RemoteWriteMetricName
)

type RemoteWriteOptions struct {
	MetricsFile             string
	Endpoint                string
	BatchSize               int
	MaxAttempts             int
	RetryBackoff            time.Duration
	CheckpointFile          string
	SkipIfCompleted         bool
	CheckpointSchemaVersion string
	UserAgent               string
	Client                  *http.Client
}

type RemoteWriteResult struct {
	Endpoint       string `json:"endpoint"`
	Requests       int    `json:"requests"`
	Samples        int    `json:"samples"`
	Retries        int    `json:"retries"`
	Reused         bool   `json:"reused,omitempty"`
	CheckpointFile string `json:"checkpoint_file,omitempty"`
	MetricsSHA256  string `json:"metrics_sha256,omitempty"`
	MetricsBytes   int64  `json:"metrics_bytes,omitempty"`
}

type remoteWriteMetricRow struct {
	SourceStoreID  string  `json:"source_store_id"`
	Project        string  `json:"project"`
	ExperimentID   string  `json:"experiment_id"`
	RunGroupID     string  `json:"run_group_id"`
	RunID          string  `json:"run_id"`
	MetricName     string  `json:"metric_name"`
	Step           int64   `json:"step"`
	WallTime       string  `json:"wall_time"`
	Value          float64 `json:"value"`
	Unit           string  `json:"unit"`
	Source         string  `json:"source"`
	Split          string  `json:"split"`
	ExportedAt     string  `json:"exported_at"`
	MetricFileID   string  `json:"metric_file_id"`
	MetricFilePath string  `json:"metric_file_path"`
	Tags           string  `json:"tags"`
}

type remoteWriteTimeSeries struct {
	Labels  []remoteWriteLabel
	Samples []remoteWriteSample
}

type remoteWriteLabel struct {
	Name  string
	Value string
}

type remoteWriteSample struct {
	Value       float64
	TimestampMS int64
}

type remoteWriteCheckpoint struct {
	SchemaVersion string `json:"schema_version"`
	MetricsFile   string `json:"metrics_file"`
	MetricsSHA256 string `json:"metrics_sha256"`
	MetricsBytes  int64  `json:"metrics_bytes"`
	Endpoint      string `json:"endpoint"`
	BatchSize     int    `json:"batch_size"`
	Requests      int    `json:"requests"`
	Samples       int    `json:"samples"`
	Retries       int    `json:"retries"`
	CompletedAt   string `json:"completed_at"`
}

func replayRemoteWrite(ctx context.Context, opts RemoteWriteOptions) (RemoteWriteResult, error) {
	endpoint := strings.TrimSpace(opts.Endpoint)
	if endpoint == "" {
		return RemoteWriteResult{}, fmt.Errorf("remote write endpoint is required")
	}
	metricsFile := strings.TrimSpace(opts.MetricsFile)
	if metricsFile == "" {
		return RemoteWriteResult{}, fmt.Errorf("metrics file is required")
	}
	batchSize := opts.BatchSize
	if batchSize <= 0 {
		batchSize = defaultRemoteWriteBatchSize
	}
	retryOpts := remoteWriteRetryConfig(opts)
	metricsSHA256, metricsBytes, err := fileDigest(metricsFile)
	if err != nil {
		return RemoteWriteResult{}, err
	}
	checkpointFile := strings.TrimSpace(opts.CheckpointFile)
	if checkpointFile != "" && opts.SkipIfCompleted {
		checkpoint, ok, err := readRemoteWriteCheckpoint(checkpointFile)
		if err != nil {
			return RemoteWriteResult{}, err
		}
		if ok && checkpoint.matches(metricsFile, metricsSHA256, endpoint, batchSize) {
			return RemoteWriteResult{
				Endpoint:       endpoint,
				Requests:       checkpoint.Requests,
				Samples:        checkpoint.Samples,
				Retries:        checkpoint.Retries,
				Reused:         true,
				CheckpointFile: checkpointFile,
				MetricsSHA256:  metricsSHA256,
				MetricsBytes:   metricsBytes,
			}, nil
		}
	}
	client := opts.Client
	if client == nil {
		client = http.DefaultClient
	}
	f, err := os.Open(metricsFile)
	if err != nil {
		return RemoteWriteResult{}, err
	}
	defer f.Close()

	result := RemoteWriteResult{
		Endpoint:       endpoint,
		CheckpointFile: checkpointFile,
		MetricsSHA256:  metricsSHA256,
		MetricsBytes:   metricsBytes,
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	batch := make([]remoteWriteTimeSeries, 0, batchSize)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row remoteWriteMetricRow
		if err := json.Unmarshal(line, &row); err != nil {
			return RemoteWriteResult{}, fmt.Errorf("decode metric row: %w", err)
		}
		if strings.TrimSpace(row.MetricName) == "" || strings.TrimSpace(row.RunID) == "" {
			continue
		}
		batch = append(batch, remoteWriteSeries(row))
		if len(batch) >= batchSize {
			retries, err := sendRemoteWriteBatch(ctx, client, endpoint, batch, retryOpts, opts.UserAgent)
			if err != nil {
				return RemoteWriteResult{}, err
			}
			result.Retries += retries
			result.Requests++
			result.Samples += len(batch)
			batch = batch[:0]
		}
	}
	if err := scanner.Err(); err != nil {
		return RemoteWriteResult{}, err
	}
	if len(batch) > 0 {
		retries, err := sendRemoteWriteBatch(ctx, client, endpoint, batch, retryOpts, opts.UserAgent)
		if err != nil {
			return RemoteWriteResult{}, err
		}
		result.Retries += retries
		result.Requests++
		result.Samples += len(batch)
	}
	if checkpointFile != "" {
		if err := writeJSONFileAtomic(checkpointFile, remoteWriteCheckpoint{
			SchemaVersion: strings.TrimSpace(opts.CheckpointSchemaVersion),
			MetricsFile:   metricsFile,
			MetricsSHA256: metricsSHA256,
			MetricsBytes:  metricsBytes,
			Endpoint:      endpoint,
			BatchSize:     batchSize,
			Requests:      result.Requests,
			Samples:       result.Samples,
			Retries:       result.Retries,
			CompletedAt:   time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return RemoteWriteResult{}, err
		}
	}
	return result, nil
}

// ReplayMetricsRemoteWrite streams an offloaded metrics JSONL file to an
// adx-mon remote-write endpoint, resuming from a checkpoint when present.
func ReplayMetricsRemoteWrite(ctx context.Context, opts RemoteWriteOptions) (RemoteWriteResult, error) {
	opts.CheckpointSchemaVersion = metricsRemoteWriteCheckpoint
	opts.UserAgent = metricsRemoteWriteUserAgent
	return replayRemoteWrite(ctx, opts)
}

func readRemoteWriteCheckpoint(path string) (remoteWriteCheckpoint, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return remoteWriteCheckpoint{}, false, nil
		}
		return remoteWriteCheckpoint{}, false, err
	}
	var checkpoint remoteWriteCheckpoint
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return remoteWriteCheckpoint{}, false, fmt.Errorf("read remote-write checkpoint %s: %w", path, err)
	}
	return checkpoint, true, nil
}

func (c remoteWriteCheckpoint) matches(metricsFile, metricsSHA256, endpoint string, batchSize int) bool {
	return c.MetricsFile == metricsFile &&
		c.MetricsSHA256 == metricsSHA256 &&
		c.Endpoint == endpoint &&
		c.BatchSize == batchSize &&
		c.Samples > 0
}

func remoteWriteSeries(row remoteWriteMetricRow) remoteWriteTimeSeries {
	labels := []remoteWriteLabel{
		{Name: "__name__", Value: remoteWriteMetricName},
		{Name: "project", Value: row.Project},
		{Name: "experiment_id", Value: row.experimentID()},
		{Name: "run_group_id", Value: row.RunGroupID},
		{Name: "run_id", Value: row.RunID},
		{Name: "metric_name", Value: row.MetricName},
		{Name: "source", Value: row.Source},
		{Name: "split", Value: row.Split},
		{Name: "unit", Value: row.Unit},
		{Name: "step", Value: strconv.FormatInt(row.Step, 10)},
	}
	if row.MetricFileID != "" {
		labels = append(labels, remoteWriteLabel{Name: "metric_file_id", Value: row.MetricFileID})
	}
	if row.MetricFilePath != "" {
		labels = append(labels, remoteWriteLabel{Name: "metric_file_path", Value: row.MetricFilePath})
	}
	if row.SourceStoreID != "" {
		labels = append(labels, remoteWriteLabel{Name: "source_store_id", Value: row.SourceStoreID})
	}
	if row.Tags != "" {
		labels = append(labels, remoteWriteLabel{Name: "tags", Value: row.Tags})
	}
	if workspace := remoteWriteWorkspace(row.Tags); workspace != "" {
		labels = append(labels, remoteWriteLabel{Name: "workspace_id", Value: workspace})
	}
	sort.Slice(labels, func(i, j int) bool {
		return labels[i].Name < labels[j].Name
	})
	return remoteWriteTimeSeries{
		Labels: labels,
		Samples: []remoteWriteSample{{
			Value:       row.Value,
			TimestampMS: remoteWriteTimestampMS(row),
		}},
	}
}

func remoteWriteWorkspace(rawTags string) string {
	var tags map[string]string
	if err := json.Unmarshal([]byte(rawTags), &tags); err != nil {
		return ""
	}
	return strings.TrimSpace(tags[exptelemetry.TauWorkspaceTag])
}

func remoteWriteTimestampMS(row remoteWriteMetricRow) int64 {
	for _, raw := range []string{row.WallTime, row.ExportedAt} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
			return t.UTC().UnixMilli()
		}
	}
	return time.Now().UTC().UnixMilli()
}

type remoteWriteRetryOptions struct {
	maxAttempts int
	backoff     time.Duration
}

func remoteWriteRetryConfig(opts RemoteWriteOptions) remoteWriteRetryOptions {
	maxAttempts := opts.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	backoff := opts.RetryBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	return remoteWriteRetryOptions{maxAttempts: maxAttempts, backoff: backoff}
}

type remoteWriteHTTPError struct {
	statusCode int
	body       string
}

func (e remoteWriteHTTPError) Error() string {
	return fmt.Sprintf("remote write failed: status=%d body=%s", e.statusCode, strings.TrimSpace(e.body))
}

func (e remoteWriteHTTPError) retryable() bool {
	return e.statusCode == http.StatusRequestTimeout || e.statusCode == http.StatusTooManyRequests || e.statusCode >= 500
}

func sendRemoteWriteBatch(ctx context.Context, client *http.Client, endpoint string, series []remoteWriteTimeSeries, retryOpts remoteWriteRetryOptions, userAgent string) (int, error) {
	var lastErr error
	for attempt := 1; attempt <= retryOpts.maxAttempts; attempt++ {
		err := postRemoteWriteBatch(ctx, client, endpoint, series, userAgent)
		if err == nil {
			return attempt - 1, nil
		}
		lastErr = err
		if !retryRemoteWriteError(ctx, err) || attempt == retryOpts.maxAttempts {
			return attempt - 1, err
		}
		if err := sleepRemoteWriteRetry(ctx, retryOpts.backoff, attempt); err != nil {
			return attempt - 1, err
		}
	}
	return retryOpts.maxAttempts - 1, lastErr
}

func retryRemoteWriteError(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if httpErr, ok := err.(remoteWriteHTTPError); ok {
		return httpErr.retryable()
	}
	return true
}

func sleepRemoteWriteRetry(ctx context.Context, base time.Duration, attempt int) error {
	delay := base
	for i := 1; i < attempt; i++ {
		delay *= 2
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func postRemoteWriteBatch(ctx context.Context, client *http.Client, endpoint string, series []remoteWriteTimeSeries, userAgent string) error {
	payload := encodeRemoteWriteRequest(series)
	body := snappy.Encode(nil, payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	if strings.TrimSpace(userAgent) == "" {
		userAgent = metricsRemoteWriteUserAgent
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return remoteWriteHTTPError{statusCode: resp.StatusCode, body: string(b)}
	}
	return nil
}

func encodeRemoteWriteRequest(series []remoteWriteTimeSeries) []byte {
	var out []byte
	for _, ts := range series {
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, encodeRemoteWriteTimeSeries(ts))
	}
	return out
}

func encodeRemoteWriteTimeSeries(ts remoteWriteTimeSeries) []byte {
	var out []byte
	for _, label := range ts.Labels {
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, encodeRemoteWriteLabel(label))
	}
	for _, sample := range ts.Samples {
		out = protowire.AppendTag(out, 2, protowire.BytesType)
		out = protowire.AppendBytes(out, encodeRemoteWriteSample(sample))
	}
	return out
}

func encodeRemoteWriteLabel(label remoteWriteLabel) []byte {
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	out = protowire.AppendString(out, label.Name)
	out = protowire.AppendTag(out, 2, protowire.BytesType)
	out = protowire.AppendString(out, label.Value)
	return out
}

func encodeRemoteWriteSample(sample remoteWriteSample) []byte {
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.Fixed64Type)
	out = protowire.AppendFixed64(out, math.Float64bits(sample.Value))
	out = protowire.AppendTag(out, 2, protowire.VarintType)
	out = protowire.AppendVarint(out, uint64(sample.TimestampMS))
	return out
}

func (r *remoteWriteMetricRow) UnmarshalJSON(data []byte) error {
	type wireRow remoteWriteMetricRow
	decoded := struct {
		*wireRow
		LegacyExperimentID string `json:"question_id,omitempty"`
	}{
		wireRow: (*wireRow)(r),
	}
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	if strings.TrimSpace(r.ExperimentID) == "" {
		r.ExperimentID = decoded.LegacyExperimentID
	}
	return nil
}

func (r remoteWriteMetricRow) experimentID() string {
	return strings.TrimSpace(r.ExperimentID)
}
