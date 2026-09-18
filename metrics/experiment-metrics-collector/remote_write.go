// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/golang/snappy"
	"google.golang.org/protobuf/encoding/protowire"
)

const remoteWriteUserAgent = "taugrid-metrics-collector"

type RemoteWriteSink struct {
	Endpoint    string
	BatchSize   int
	MaxAttempts int
	Backoff     time.Duration
	Client      *http.Client
}

func (s *RemoteWriteSink) Name() string { return "remote-write-v1" }

func (s *RemoteWriteSink) ConfigIdentity() string {
	config := struct {
		Version   string `json:"version"`
		Endpoint  string `json:"endpoint"`
		BatchSize int    `json:"batch_size"`
	}{
		Version: "prometheus-remote-write-v1", Endpoint: strings.TrimSpace(s.Endpoint), BatchSize: s.batchSize(),
	}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	return "remote-write-v1:" + hex.EncodeToString(sum[:])
}

func (s *RemoteWriteSink) Deliver(ctx context.Context, chunk MetricEventChunk) (DeliveryAck, error) {
	if strings.TrimSpace(s.Endpoint) == "" {
		return DeliveryAck{}, fmt.Errorf("remote-write endpoint is required")
	}
	for index, event := range chunk.Events {
		if err := event.Validate(); err != nil {
			return DeliveryAck{}, fmt.Errorf("remote-write event %d: %w", index, err)
		}
	}
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	ack := DeliveryAck{Samples: len(chunk.Events)}
	for start := 0; start < len(chunk.Events); start += s.batchSize() {
		end := min(start+s.batchSize(), len(chunk.Events))
		retries, err := s.send(ctx, client, chunk.Events[start:end])
		if err != nil {
			return DeliveryAck{}, err
		}
		ack.Requests++
		ack.Retries += retries
	}
	return ack, nil
}

func (s *RemoteWriteSink) batchSize() int {
	if s.BatchSize <= 0 {
		return 5000
	}
	return s.BatchSize
}

func (s *RemoteWriteSink) attempts() int {
	if s.MaxAttempts <= 0 {
		return 3
	}
	return s.MaxAttempts
}

func (s *RemoteWriteSink) retryBackoff() time.Duration {
	if s.Backoff <= 0 {
		return time.Second
	}
	return s.Backoff
}

func (s *RemoteWriteSink) send(ctx context.Context, client *http.Client, events []exptelemetry.MetricEvent) (int, error) {
	var last error
	for attempt := 1; attempt <= s.attempts(); attempt++ {
		err := postRemoteWrite(ctx, client, s.Endpoint, events)
		if err == nil {
			return attempt - 1, nil
		}
		last = err
		var statusErr remoteWriteStatusError
		if ctx.Err() != nil || (errors.As(err, &statusErr) && !statusErr.retryable()) || attempt == s.attempts() {
			return attempt - 1, err
		}
		timer := time.NewTimer(s.retryBackoff() * time.Duration(1<<(attempt-1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return attempt - 1, ctx.Err()
		case <-timer.C:
		}
	}
	return s.attempts() - 1, last
}

type remoteWriteStatusError struct {
	code int
	body string
}

func (e remoteWriteStatusError) Error() string {
	return fmt.Sprintf("remote write failed: status=%d body=%s", e.code, strings.TrimSpace(e.body))
}

func (e remoteWriteStatusError) retryable() bool {
	return e.code == http.StatusRequestTimeout || e.code == http.StatusTooManyRequests || e.code >= 500
}

func postRemoteWrite(ctx context.Context, client *http.Client, endpoint string, events []exptelemetry.MetricEvent) error {
	payload := encodeWriteRequest(events)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(snappy.Encode(nil, payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Encoding", "snappy")
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("User-Agent", remoteWriteUserAgent)
	req.Header.Set("X-Prometheus-Remote-Write-Version", "0.1.0")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return remoteWriteStatusError{code: resp.StatusCode, body: string(body)}
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return fmt.Errorf("read remote write response: %w", err)
	}
	return nil
}

func encodeWriteRequest(events []exptelemetry.MetricEvent) []byte {
	var out []byte
	for _, event := range events {
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, encodeTimeSeries(event))
	}
	return out
}

func encodeTimeSeries(event exptelemetry.MetricEvent) []byte {
	tags := ""
	if len(event.Tags) > 0 {
		raw, _ := json.Marshal(event.Tags)
		tags = string(raw)
	}
	labels := []label{
		{"__name__", exptelemetry.RemoteWriteMetricName},
		{"project", event.Project},
		{"experiment_id", event.ExperimentID},
		{"run_group_id", event.RunGroupID},
		{"run_id", event.RunID},
		{"metric_name", event.MetricName},
		{"source", event.Source},
		{"split", event.Split},
		{"unit", event.Unit},
		{"step", strconv.FormatInt(event.Step, 10)},
	}
	for name, value := range map[string]string{
		"metric_file_id": event.MetricFileID, "metric_file_path": event.MetricFilePath,
		"source_store_id": event.SourceStoreID, "tags": tags,
	} {
		if value != "" {
			labels = append(labels, label{name, value})
		}
	}
	if workspace := strings.TrimSpace(event.Tags[exptelemetry.TauWorkspaceTag]); workspace != "" {
		labels = append(labels, label{"workspace_id", workspace})
	}
	sort.Slice(labels, func(i, j int) bool { return labels[i].name < labels[j].name })
	var out []byte
	for _, item := range labels {
		out = protowire.AppendTag(out, 1, protowire.BytesType)
		out = protowire.AppendBytes(out, encodeLabel(item))
	}
	out = protowire.AppendTag(out, 2, protowire.BytesType)
	out = protowire.AppendBytes(out, encodeSample(event.Value, event.WallTime.UTC().UnixMilli()))
	return out
}

type label struct{ name, value string }

func encodeLabel(value label) []byte {
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.BytesType)
	out = protowire.AppendString(out, value.name)
	out = protowire.AppendTag(out, 2, protowire.BytesType)
	out = protowire.AppendString(out, value.value)
	return out
}

func encodeSample(value float64, timestamp int64) []byte {
	var out []byte
	out = protowire.AppendTag(out, 1, protowire.Fixed64Type)
	out = protowire.AppendFixed64(out, math.Float64bits(value))
	out = protowire.AppendTag(out, 2, protowire.VarintType)
	out = protowire.AppendVarint(out, uint64(timestamp))
	return out
}
