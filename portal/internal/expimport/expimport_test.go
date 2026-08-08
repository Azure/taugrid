// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expimport

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/snappy"
	"github.com/parquet-go/parquet-go"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestImportJSONLWritesResearchMetricHistory(t *testing.T) {
	ctx := context.Background()
	store, _, err := expstore.Init(ctx, filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        "captioner-richer-metrics",
		Project:     "captioner",
		Description: "Do richer Captioner diagnostics distinguish candidate reruns?",
		Group:       "a100-rerun",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	history := filepath.Join(t.TempDir(), "metrics-history.jsonl")
	writeJSONLLines(t, history,
		`{"_step":1,"_timestamp":1770000000,"train/loss":0.42,"train/lr":0.0002,"train/grad_norm":1.7,"train/step_time_s":3.1,"train/examples_seen":64,"train/input_tokens":1024,"gpu/memory_allocated_gb":21.5,"checkpoint/file_count":8,"checkpoint/bytes":4096,"feature/image_text_alignment":0.61,"inference/time_s":0.2,"note":"ignored"}`,
		`{"_step":2,"train/loss":0.38,"train/tokens":2048,"gpu/max_memory_allocated_gb":23.75}`,
	)

	result, err := ImportJSONL(ctx, store, JSONLImportOptions{
		RunID:          "captioner2-stellar-a100-base-rich-v3",
		ExperimentID:   "captioner-richer-metrics",
		RunGroupID:     "a100-rerun",
		History:        []string{history},
		Source:         "captioner-jsonl",
		Tags:           map[string]string{"dataset": "vision", "recipe": "vit-enc"},
		IdempotencyKey: "jsonl-captioner-rich",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Rows != 14 || result.MetricFile == nil || len(result.Artifacts) != 1 || len(result.HistoryFiles) != 1 {
		t.Fatalf("unexpected JSONL result: %+v", result)
	}
	if result.ExperimentID != "captioner-richer-metrics" || !strings.Contains(filepath.ToSlash(result.MetricFile.Path), "/experiment=captioner-richer-metrics/") {
		t.Fatalf("import did not retain the experiment assignment in its result and metric path: %+v", result)
	}
	assigned, err := store.Query(ctx, "select count(*) as count from runs where run_id = 'captioner2-stellar-a100-base-rich-v3' and experiment_id = 'captioner-richer-metrics'")
	if err != nil {
		t.Fatal(err)
	}
	if assigned.Rows[0]["count"] != int64(1) {
		t.Fatalf("JSONL run did not retain its experiment assignment: %+v", assigned.Rows)
	}
	if result.MinStep == nil || *result.MinStep != 1 || result.MaxStep == nil || *result.MaxStep != 2 {
		t.Fatalf("unexpected step range: min=%v max=%v", result.MinStep, result.MaxStep)
	}

	rows, err := parquet.ReadFile[expstore.MetricRow](filepath.Join(store.Root, filepath.FromSlash(result.MetricFile.Path)))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 14 {
		t.Fatalf("parquet rows=%d, want 14", len(rows))
	}
	loss := findMetricRow(t, rows, "train/loss")
	if loss.Source != "captioner-jsonl" || loss.Step == nil || *loss.Step != 1 || loss.WallTime == nil {
		t.Fatalf("train/loss row missing JSONL scalar fields: %+v", loss)
	}
	var tags map[string]string
	if err := json.Unmarshal([]byte(loss.Tags), &tags); err != nil {
		t.Fatal(err)
	}
	if tags["tau.metric.card"] != "World model" || tags["dataset"] != "vision" || tags["recipe"] != "vit-enc" {
		t.Fatalf("train/loss should match TensorBoard card semantics: %+v", tags)
	}
	merged, err := store.RunTags(ctx, []string{"captioner2-stellar-a100-base-rich-v3"})
	if err != nil {
		t.Fatal(err)
	}
	if merged["captioner2-stellar-a100-base-rich-v3"]["dataset"] != "vision" || merged["captioner2-stellar-a100-base-rich-v3"]["recipe"] != "vit-enc" {
		t.Fatalf("run tags not recorded for JSONL import: %+v", merged)
	}
	gpu := findMetricRow(t, rows, "gpu/memory_allocated_gb")
	if err := json.Unmarshal([]byte(gpu.Tags), &tags); err != nil {
		t.Fatal(err)
	}
	if tags["jsonl.raw_key"] != "gpu/memory_allocated_gb" || tags["tau.metric.card"] != "Systems" || tags["tau.metric.standard"] != "true" {
		t.Fatalf("generic JSONL tags not preserved: %+v", tags)
	}
	feature := findMetricRow(t, rows, "feature/image_text_alignment")
	if err := json.Unmarshal([]byte(feature.Tags), &tags); err != nil {
		t.Fatal(err)
	}
	if tags["tau.metric.card"] != "Model diagnostics" || tags["tau.metric.standard"] != "true" {
		t.Fatalf("feature metric should be recognized as model diagnostic: %+v", tags)
	}
}

func TestJSONLRequestHashIgnoresProjectedFileIdentity(t *testing.T) {
	historyFiles := []JSONLHistoryFile{{
		Path:       "metrics-history.jsonl",
		SizeBytes:  123,
		ModTime:    "2026-05-21T00:00:00Z",
		SHA256:     "abc123",
		ScalarRows: 2,
	}}
	opts := JSONLImportOptions{
		RunID:        "run-1",
		Project:      "project-1",
		ExperimentID: "experiment-1",
		RunGroupID:   "group-1",
		Owner:        "owner-1",
		State:        "finished",
		MetricPrefix: "train/",
		Source:       "jsonl",
		StepField:    "_step",
		TimeField:    "_timestamp",
	}

	got, err := jsonlRequestHash(opts, historyFiles)
	if err != nil {
		t.Fatal(err)
	}
	projectedAgain := []JSONLHistoryFile{{
		Path:       "/var/lib/kubelet/pods/sample/volumes/kubernetes.io~configmap/sample/metrics-history.jsonl",
		SizeBytes:  historyFiles[0].SizeBytes,
		ModTime:    "2026-05-21T01:23:45Z",
		SHA256:     historyFiles[0].SHA256,
		ScalarRows: historyFiles[0].ScalarRows,
	}}
	gotAgain, err := jsonlRequestHash(opts, projectedAgain)
	if err != nil {
		t.Fatal(err)
	}
	if gotAgain != got {
		t.Fatalf("same JSONL content should produce stable request hash across projected file path/mtime changes: got %s want %s", gotAgain, got)
	}

	opts.SkipArtifacts = true
	skipArtifactsHash, err := jsonlRequestHash(opts, historyFiles)
	if err != nil {
		t.Fatal(err)
	}
	if skipArtifactsHash == got {
		t.Fatalf("SkipArtifacts=true should use a distinct JSONL request hash")
	}
}

func TestReplayMetricsRemoteWritePostsSnappyPrometheusPayload(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	writeJSONLLines(t, metricsFile,
		`{"source_store_id":"source-1","project":"sample-project","question_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":42.5,"unit":"","source":"wandb","split":"train","metric_file_id":"mf-1","metric_file_path":"/data/outputs/sample-training/seed-1/wandb/offline-run-1/run-1.wandb","tags":"{\"tau_workspace\":\"sample\"}"}`,
		`{"source_store_id":"source-1","project":"sample-project","question_id":"sample-project-wandb-migration","run_group_id":"reference-group","run_id":"seed-1","metric_name":"eval/score","step":2,"wall_time":"2026-05-21T00:01:00Z","value":9,"unit":"","source":"wandb","split":"eval","metric_file_id":"mf-1","metric_file_path":"/data/outputs/sample-training/seed-1/wandb/offline-run-1/run-1.wandb"}`,
	)
	var requests int32
	var samples int32
	var labelSetsMu sync.Mutex
	var labelSets []map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != metricsRemoteWriteUserAgent {
			t.Fatalf("unexpected user-agent: %q", r.Header.Get("User-Agent"))
		}
		if r.Header.Get("Content-Encoding") != "snappy" || r.Header.Get("Content-Type") != "application/x-protobuf" {
			t.Fatalf("unexpected remote-write headers: %v", r.Header)
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := snappy.Decode(nil, raw)
		if err != nil {
			t.Fatal(err)
		}
		atomic.AddInt32(&requests, 1)
		atomic.AddInt32(&samples, int32(countRemoteWriteSamples(t, decoded)))
		labelSetsMu.Lock()
		labelSets = append(labelSets, remoteWriteSeriesLabels(t, decoded)...)
		labelSetsMu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	result, err := ReplayMetricsRemoteWrite(context.Background(), RemoteWriteOptions{
		MetricsFile: metricsFile,
		Endpoint:    server.URL,
		BatchSize:   1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Requests != 2 || result.Samples != 2 || requests != 2 || samples != 2 {
		t.Fatalf("unexpected remote-write result: result=%+v requests=%d samples=%d", result, requests, samples)
	}
	labels := findRemoteWriteMetricLabels(t, labelSets, "train/return")
	if labels["source_store_id"] != "source-1" || labels["metric_file_id"] != "mf-1" || labels["metric_file_path"] == "" {
		t.Fatalf("remote-write labels missing source identity: %+v", labels)
	}
	if labels["workspace_id"] != "sample" {
		t.Fatalf("remote-write labels missing workspace identity: %+v", labels)
	}
}

func TestReplayMetricsRemoteWriteWritesCheckpointSchema(t *testing.T) {
	dir := t.TempDir()
	metricsFile := filepath.Join(dir, "TauExpMetrics.jsonl")
	checkpointFile := filepath.Join(dir, "metrics_remote_write_checkpoint.json")
	writeJSONLLines(t, metricsFile,
		`{"source_store_id":"source-1","project":"sample-project","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":42.5,"source":"stellar-online"}`,
	)
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&requests, 1)
		if r.Header.Get("User-Agent") != metricsRemoteWriteUserAgent {
			t.Fatalf("unexpected metrics remote-write user-agent: %q", r.Header.Get("User-Agent"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	result, err := ReplayMetricsRemoteWrite(context.Background(), RemoteWriteOptions{
		MetricsFile:     metricsFile,
		Endpoint:        server.URL,
		BatchSize:       1,
		CheckpointFile:  checkpointFile,
		SkipIfCompleted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Requests != 1 || result.Samples != 1 || requests != 1 {
		t.Fatalf("unexpected metrics remote-write result: result=%+v requests=%d", result, requests)
	}
	var checkpoint remoteWriteCheckpoint
	raw, err := os.ReadFile(checkpointFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.SchemaVersion != metricsRemoteWriteCheckpoint {
		t.Fatalf("metrics checkpoint schema = %q, want %q", checkpoint.SchemaVersion, metricsRemoteWriteCheckpoint)
	}
}

func TestReplayMetricsRemoteWriteRetriesTransientFailures(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	writeJSONLLines(t, metricsFile,
		`{"source_store_id":"source-1","project":"sample-project","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":42.5,"source":"wandb"}`,
	)
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := atomic.AddInt32(&attempts, 1)
		if attempt < 3 {
			http.Error(w, "adx throttled", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	result, err := ReplayMetricsRemoteWrite(context.Background(), RemoteWriteOptions{
		MetricsFile:  metricsFile,
		Endpoint:     server.URL,
		BatchSize:    1,
		MaxAttempts:  3,
		RetryBackoff: time.Nanosecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 3 || result.Requests != 1 || result.Samples != 1 || result.Retries != 2 {
		t.Fatalf("unexpected retry result: attempts=%d result=%+v", attempts, result)
	}
}

func TestReplayMetricsRemoteWriteReusesCompletedCheckpoint(t *testing.T) {
	dir := t.TempDir()
	metricsFile := filepath.Join(dir, "TauExpMetrics.jsonl")
	checkpointFile := filepath.Join(dir, "wandb_remote_write_checkpoint.json")
	writeJSONLLines(t, metricsFile,
		`{"source_store_id":"source-1","project":"sample-project","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":42.5,"source":"wandb"}`,
	)
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	first, err := ReplayMetricsRemoteWrite(context.Background(), RemoteWriteOptions{
		MetricsFile:     metricsFile,
		Endpoint:        server.URL,
		BatchSize:       1,
		CheckpointFile:  checkpointFile,
		SkipIfCompleted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused || first.CheckpointFile != checkpointFile || first.MetricsSHA256 == "" || first.MetricsBytes == 0 {
		t.Fatalf("unexpected first replay result: %+v", first)
	}
	if _, err := os.Stat(checkpointFile); err != nil {
		t.Fatal(err)
	}
	var checkpoint remoteWriteCheckpoint
	raw, err := os.ReadFile(checkpointFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.SchemaVersion != metricsRemoteWriteCheckpoint {
		t.Fatalf("checkpoint schema = %q, want %q", checkpoint.SchemaVersion, metricsRemoteWriteCheckpoint)
	}

	second, err := ReplayMetricsRemoteWrite(context.Background(), RemoteWriteOptions{
		MetricsFile:     metricsFile,
		Endpoint:        server.URL,
		BatchSize:       1,
		CheckpointFile:  checkpointFile,
		SkipIfCompleted: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || second.Samples != 1 || attempts != 1 {
		t.Fatalf("expected checkpoint reuse without repost: attempts=%d result=%+v", attempts, second)
	}
}

func TestReplayMetricsRemoteWriteDoesNotRetryPermanentFailure(t *testing.T) {
	metricsFile := filepath.Join(t.TempDir(), "TauExpMetrics.jsonl")
	writeJSONLLines(t, metricsFile,
		`{"source_store_id":"source-1","project":"sample-project","run_group_id":"reference-group","run_id":"seed-1","metric_name":"train/return","step":1,"wall_time":"2026-05-21T00:00:00Z","value":42.5,"source":"wandb"}`,
	)
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "bad labels", http.StatusBadRequest)
	}))
	defer server.Close()

	_, err := ReplayMetricsRemoteWrite(context.Background(), RemoteWriteOptions{
		MetricsFile:  metricsFile,
		Endpoint:     server.URL,
		BatchSize:    1,
		MaxAttempts:  3,
		RetryBackoff: time.Nanosecond,
	})
	if err == nil || !strings.Contains(err.Error(), "status=400") {
		t.Fatalf("expected permanent remote-write error, got %v", err)
	}
	if attempts != 1 {
		t.Fatalf("permanent failure attempts=%d, want 1", attempts)
	}
}

func writeJSONLLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func countRemoteWriteSamples(t *testing.T, payload []byte) int {
	t.Helper()
	count := 0
	for len(payload) > 0 {
		field, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			t.Fatalf("consume top-level tag: %v", protowire.ParseError(n))
		}
		payload = payload[n:]
		if field != 1 || typ != protowire.BytesType {
			t.Fatalf("unexpected write request field=%d type=%v", field, typ)
		}
		series, n := protowire.ConsumeBytes(payload)
		if n < 0 {
			t.Fatalf("consume series: %v", protowire.ParseError(n))
		}
		payload = payload[n:]
		count += countRemoteWriteSeriesSamples(t, series)
	}
	return count
}

func countRemoteWriteSeriesSamples(t *testing.T, series []byte) int {
	t.Helper()
	count := 0
	for len(series) > 0 {
		field, typ, n := protowire.ConsumeTag(series)
		if n < 0 {
			t.Fatalf("consume series tag: %v", protowire.ParseError(n))
		}
		series = series[n:]
		if typ != protowire.BytesType {
			t.Fatalf("unexpected series field=%d type=%v", field, typ)
		}
		_, n = protowire.ConsumeBytes(series)
		if n < 0 {
			t.Fatalf("consume nested series value: %v", protowire.ParseError(n))
		}
		series = series[n:]
		if field == 2 {
			count++
		}
	}
	return count
}

func remoteWriteSeriesLabels(t *testing.T, payload []byte) []map[string]string {
	t.Helper()
	var labels []map[string]string
	for len(payload) > 0 {
		field, typ, n := protowire.ConsumeTag(payload)
		if n < 0 {
			t.Fatalf("consume top-level tag: %v", protowire.ParseError(n))
		}
		payload = payload[n:]
		if field != 1 || typ != protowire.BytesType {
			t.Fatalf("unexpected write request field=%d type=%v", field, typ)
		}
		series, n := protowire.ConsumeBytes(payload)
		if n < 0 {
			t.Fatalf("consume series: %v", protowire.ParseError(n))
		}
		payload = payload[n:]
		labels = append(labels, remoteWriteLabels(t, series))
	}
	return labels
}

func remoteWriteLabels(t *testing.T, series []byte) map[string]string {
	t.Helper()
	labels := map[string]string{}
	for len(series) > 0 {
		field, typ, n := protowire.ConsumeTag(series)
		if n < 0 {
			t.Fatalf("consume series tag: %v", protowire.ParseError(n))
		}
		series = series[n:]
		value, n := protowire.ConsumeBytes(series)
		if n < 0 {
			t.Fatalf("consume nested series value: %v", protowire.ParseError(n))
		}
		series = series[n:]
		if field == 1 && typ == protowire.BytesType {
			name, value := remoteWriteLabelPair(t, value)
			labels[name] = value
		}
	}
	return labels
}

func remoteWriteLabelPair(t *testing.T, raw []byte) (string, string) {
	t.Helper()
	var name, value string
	for len(raw) > 0 {
		field, typ, n := protowire.ConsumeTag(raw)
		if n < 0 {
			t.Fatalf("consume label tag: %v", protowire.ParseError(n))
		}
		raw = raw[n:]
		if typ != protowire.BytesType {
			t.Fatalf("unexpected label field=%d type=%v", field, typ)
		}
		text, n := protowire.ConsumeString(raw)
		if n < 0 {
			t.Fatalf("consume label value: %v", protowire.ParseError(n))
		}
		raw = raw[n:]
		switch field {
		case 1:
			name = text
		case 2:
			value = text
		}
	}
	return name, value
}

func findRemoteWriteMetricLabels(t *testing.T, labelSets []map[string]string, metric string) map[string]string {
	t.Helper()
	for _, labels := range labelSets {
		if labels["metric_name"] == metric {
			return labels
		}
	}
	t.Fatalf("missing remote-write metric labels for %q in %+v", metric, labelSets)
	return nil
}

func findMetricRow(t *testing.T, rows []expstore.MetricRow, name string) expstore.MetricRow {
	t.Helper()
	for _, row := range rows {
		if row.MetricName == name {
			return row
		}
	}
	t.Fatalf("missing metric row %q in %+v", name, rows)
	return expstore.MetricRow{}
}

func TestImportJSONLCleansFilesWhenRecordRunDataFails(t *testing.T) {
	ctx := context.Background()
	store, _, err := expstore.Init(ctx, filepath.Join(t.TempDir(), "store"), expstore.InitOptions{
		Name:        "experiment-alpha",
		Project:     "project-alpha",
		Description: "Can we reproduce Candidate training sample benchmark on A100?",
		Group:       "reference-group",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.RecordRunData(ctx, expstore.RecordRunDataOptions{
		Run: expstore.RunRecord{
			RunID:      "seed-1",
			Project:    "project-alpha",
			RunGroupID: "reference-group",
			State:      "succeeded",
			Owner:      "human",
		},
		Tags: []expstore.TagRecord{
			{ScopeType: "run", ScopeID: "seed-1", Key: "source", Value: "not-jsonl"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	history := filepath.Join(t.TempDir(), "metrics-history.jsonl")
	writeJSONLLines(t, history, `{"_step":1,"_timestamp":10,"train/return":10}`)

	// JSONL import preserves existing run metadata, so the conflict is forced
	// through a run tag the importer always writes.
	_, err = ImportJSONL(ctx, store, JSONLImportOptions{
		RunID:          "seed-1",
		RunGroupID:     "reference-group",
		History:        []string{history},
		IdempotencyKey: "jsonl-conflict-seed-1",
	})
	if !errors.Is(err, expstore.ErrConflict) {
		t.Fatalf("expected RecordRunData conflict, got %v", err)
	}
	assertNoFiles(t, filepath.Join(store.Root, expstore.MetricsDir))
	assertNoFiles(t, filepath.Join(store.Root, expstore.ArtifactsDir))
	for _, query := range []string{
		"select count(*) as count from metric_files where run_id = 'seed-1'",
		"select count(*) as count from artifacts where run_id = 'seed-1'",
		"select count(*) as count from idempotency_keys where key = 'jsonl-conflict-seed-1'",
	} {
		got, err := store.Query(ctx, query)
		if err != nil {
			t.Fatal(err)
		}
		if got.Rows[0]["count"] != int64(0) {
			t.Fatalf("%s => %+v", query, got.Rows)
		}
	}
}

func assertNoFiles(t *testing.T, root string) {
	t.Helper()
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			t.Fatalf("unexpected file left after failed import: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
