// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/portal/internal/expstore"
	"github.com/Azure/taugrid/portal/internal/jsonlutil"
)

func TestMetricsOffloadWatchMetadataCheckpointRestartAndFinalFlush(t *testing.T) {
	for _, separateChunk := range []bool{false, true} {
		name := "appended"
		if separateChunk {
			name = "separate-chunks"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			historyDir := t.TempDir()
			history := filepath.Join(historyDir, "history.jsonl")
			whitespace := filepath.Join(historyDir, "whitespace.jsonl")
			if err := os.WriteFile(history, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(whitespace, []byte(" \n\t\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			bodies := make(chan []byte, 16)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				select {
				case bodies <- body:
				default:
					t.Error("unexpected excess remote writes")
				}
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()
			opts := metricsOffloadOptions{
				History:        []string{filepath.Join(historyDir, "*.jsonl")},
				RunID:          name,
				Project:        "pretraining",
				RunGroupID:     "bounded",
				Source:         "stellar-online",
				Out:            filepath.Join(t.TempDir(), "out"),
				CompletionFile: filepath.Join(t.TempDir(), "complete"),
				RemoteWrite:    remoteWriteConfig{Endpoint: server.URL, BatchSize: 1},
			}
			storePath := filepath.Join(t.TempDir(), "store")
			store, _, err := expstore.Init(ctx, storePath, expstore.InitOptions{Name: name})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			// Reopen the actual store on each watch start, retaining only durable state.
			watch := func() []metricsOffloadResult {
				t.Helper()
				store, err := expstore.Open(ctx, storePath)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				results, err := runMetricsOffloadWatch(ctx, store, opts, time.Millisecond, 2, nil)
				if err != nil {
					t.Fatal(err)
				}
				return results
			}
			assertIdle := func(results []metricsOffloadResult) {
				t.Helper()
				if len(results) != 2 {
					t.Fatalf("watch stopped before two idle iterations: %+v", results)
				}
				for _, result := range results {
					if result.ImportRows != 0 || result.Rows != 0 || result.Completed {
						t.Fatalf("idle history produced metrics or completed: %+v", result)
					}
				}
			}
			assertIdle(watch())
			requireMetricsHistoryOffsetForTest(t, opts, history, 0)
			requireMetricsHistoryOffsetForTest(t, opts, whitespace, 4)

			const metadata = "{\"_step\":0,\"_timestamp\":1770000000,\"phase\":\"starting\",\"ready\":false,\"value\":null,\"shape\":[1,2]}\n"
			appendMetricsHistoryForTest(t, history, metadata)
			scalars := history
			if separateChunk {
				scalars = filepath.Join(historyDir, "scalars.jsonl")
			}
			const partial = `{"_step":1`
			appendMetricsHistoryForTest(t, scalars, partial)
			assertIdle(watch())
			requireMetricsHistoryOffsetForTest(t, opts, history, int64(len(metadata)))
			scalarOffset := int64(0)
			if !separateChunk {
				scalarOffset = int64(len(metadata))
			}
			requireMetricsHistoryOffsetForTest(t, opts, scalars, scalarOffset)
			if len(bodies) != 0 {
				t.Fatal("empty or metadata-only history was remote-written")
			}

			const scalarEnd = ",\"_timestamp\":1770000001,\"train/loss\":1.25}\n"
			appendMetricsHistoryForTest(t, scalars, scalarEnd)
			first := watch()
			if len(first) != 2 || first[0].ImportRows != 1 || first[0].RemoteWriteSamples != 1 || first[1].ImportRows != 0 {
				t.Fatalf("completed scalar was not exported once: %+v", first)
			}
			scalarOffset += int64(len(partial) + len(scalarEnd))
			requireMetricsHistoryOffsetForTest(t, opts, scalars, scalarOffset)

			const finalMetadata = "{\"_step\":2,\"_timestamp\":1770000002,\"phase\":\"finishing\"}\n"
			const finalScalar = `{"_step":2,"_timestamp":1770000002,"train/loss":0.5}`
			appendMetricsHistoryForTest(t, scalars, finalMetadata+finalScalar)
			assertIdle(watch())
			scalarOffset += int64(len(finalMetadata))
			requireMetricsHistoryOffsetForTest(t, opts, scalars, scalarOffset)
			if len(bodies) != 1 {
				t.Fatalf("restart replayed data or imported an unterminated scalar: %d requests", len(bodies))
			}

			if err := os.WriteFile(opts.CompletionFile, []byte(`{"state":"succeeded"}`), 0o644); err != nil {
				t.Fatal(err)
			}
			completionTime := time.Date(2026, time.February, 2, 0, 0, 0, 0, time.UTC)
			if err := os.Chtimes(opts.CompletionFile, completionTime, completionTime); err != nil {
				t.Fatal(err)
			}
			final := watch()
			if len(final) != 1 || !final[0].Completed || final[0].StatusState != "succeeded" || final[0].ImportRows != 1 || final[0].RemoteWriteSamples != 2 {
				t.Fatalf("completion did not drain final scalar and status: %+v", final)
			}
			requireMetricsHistoryOffsetForTest(t, opts, scalars, scalarOffset+int64(len(finalScalar)))
			restarted := watch()
			if len(restarted) != 1 || !restarted[0].Completed || restarted[0].ImportRows != 0 {
				t.Fatalf("restart did not preserve terminal checkpoint: %+v", restarted)
			}
			if len(bodies) != 3 {
				t.Fatalf("remote writes = %d, want two scalars and one status with no restart replay", len(bodies))
			}
			for _, step := range []string{"1", "2"} {
				labels, timestamp := decodeFirstRemoteWriteSeriesForTest(t, <-bodies)
				wantTimestamp := int64(1770000001000)
				if step == "2" {
					wantTimestamp = 1770000002000
				}
				if labels["metric_name"] != "train/loss" || labels["step"] != step || timestamp != wantTimestamp {
					t.Fatalf("unexpected remote scalar: labels=%v timestamp=%d", labels, timestamp)
				}
			}
			labels, timestamp := decodeFirstRemoteWriteSeriesForTest(t, <-bodies)
			if labels["metric_name"] != expkusto.RunStatusMetricName || timestamp != completionTime.UnixMilli() {
				t.Fatalf("final remote write is not a stable terminal marker: labels=%v timestamp=%d", labels, timestamp)
			}
		})
	}
}

func TestMetricsOffloadWatchInvalidChunkPreservesCheckpoint(t *testing.T) {
	for _, invalid := range []string{
		`{"_step":2,`,
		`{"_step":2,"_timestamp":1770000002,"phase":"starting"} garbage`,
		`{"_step":2,"_timestamp":1770000002,"loss":1e309}`,
		`{"_step":2,"_timestamp":1770000002,"loss":-1e309,"accuracy":0.5}`,
	} {
		t.Run(invalid, func(t *testing.T) {
			store := newTestMetricsOffloadStore(t, "invalid-chunk")
			history := filepath.Join(t.TempDir(), "history.jsonl")
			const metadata = "{\"_step\":0,\"_timestamp\":1770000000,\"phase\":\"starting\"}\n"
			appendMetricsHistoryForTest(t, history, metadata)
			opts := metricsOffloadOptions{
				History:    []string{history},
				RunID:      "invalid-chunk",
				RunGroupID: "bounded",
				Out:        filepath.Join(t.TempDir(), "out"),
			}
			if _, err := runMetricsOffloadWatch(context.Background(), store, opts, time.Millisecond, 2, nil); err != nil {
				t.Fatal(err)
			}
			checkpointPath := filepath.Join(opts.Out, "metrics_jsonl_checkpoint.json")
			before, err := os.ReadFile(checkpointPath)
			if err != nil {
				t.Fatal(err)
			}
			const scalar = "{\"_step\":1,\"_timestamp\":1770000001,\"train/loss\":1.25}\n"
			appendMetricsHistoryForTest(t, history, scalar+invalid+"\n")
			opts.CompletionFile = filepath.Join(t.TempDir(), "complete")
			if err := os.WriteFile(opts.CompletionFile, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				results, err := runMetricsOffloadWatch(context.Background(), store, opts, time.Millisecond, 1, nil)
				if err == nil || !strings.Contains(err.Error(), "line 2") || len(results) != 0 {
					t.Fatalf("invalid chunk was skipped or partially imported: results=%+v err=%v", results, err)
				}
				after, err := os.ReadFile(checkpointPath)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(before, after) {
					t.Fatalf("failed chunk advanced checkpoint:\nbefore %s\nafter %s", before, after)
				}
				files, err := store.Query(context.Background(), "select count(*) as count from metric_files")
				if err != nil {
					t.Fatal(err)
				}
				if files.Rows[0]["count"] != int64(0) {
					t.Fatalf("failed chunk created scalar or terminal data: %+v", files.Rows)
				}
			}
			if err := os.WriteFile(history, []byte(metadata+scalar), 0o644); err != nil {
				t.Fatal(err)
			}
			results, err := runMetricsOffloadWatch(context.Background(), store, opts, time.Millisecond, 1, nil)
			if err != nil || len(results) != 1 || results[0].ImportRows != 1 || !results[0].Completed {
				t.Fatalf("repair did not resume from last valid checkpoint: results=%+v err=%v", results, err)
			}
			requireMetricsHistoryOffsetForTest(t, opts, history, int64(len(metadata)+len(scalar)))
		})
	}
}

func TestMetricsOffloadWatchLaterFileFailureDoesNotReplayEarlierChunk(t *testing.T) {
	store := newTestMetricsOffloadStore(t, "later-file-failure")
	historyDir := t.TempDir()
	history := filepath.Join(historyDir, "1-history.jsonl")
	badHistory := filepath.Join(historyDir, "2-invalid.jsonl")
	const metadata = "{\"_step\":0,\"_timestamp\":1770000000,\"phase\":\"starting\"}\n"
	appendMetricsHistoryForTest(t, history, metadata)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	opts := metricsOffloadOptions{
		History:     []string{filepath.Join(historyDir, "*.jsonl")},
		RunID:       "later-file-failure",
		RunGroupID:  "bounded",
		Out:         filepath.Join(t.TempDir(), "out"),
		RemoteWrite: remoteWriteConfig{Endpoint: server.URL, BatchSize: 1},
	}
	if _, err := runMetricsOffloadWatch(context.Background(), store, opts, time.Millisecond, 1, nil); err != nil {
		t.Fatal(err)
	}
	const firstScalar = "{\"_step\":1,\"_timestamp\":1770000001,\"loss\":1.25}\n"
	appendMetricsHistoryForTest(t, history, firstScalar)
	appendMetricsHistoryForTest(t, badHistory, "invalid JSON\n")
	if _, err := runMetricsOffloadWatch(context.Background(), store, opts, time.Millisecond, 1, nil); err == nil {
		t.Fatal("later invalid file did not stop the watch")
	}
	// Work already exported before a later file fails must survive restart,
	// even if the producer appends more data before the failed file is repaired.
	requireMetricsHistoryOffsetForTest(t, opts, history, int64(len(metadata)+len(firstScalar)))
	checkpoint, err := jsonlutil.ReadFileCheckpointSet(filepath.Join(opts.Out, "metrics_jsonl_checkpoint.json"), metricsJSONLCheckpointSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := checkpoint.Files[badHistory]; ok {
		t.Fatal("failed file was checkpointed")
	}
	const secondScalar = "{\"_step\":2,\"_timestamp\":1770000002,\"loss\":0.5}\n"
	appendMetricsHistoryForTest(t, history, secondScalar)
	if err := os.WriteFile(badHistory, []byte(metadata), 0o644); err != nil {
		t.Fatal(err)
	}
	restarted, err := expstore.Open(context.Background(), store.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	results, err := runMetricsOffloadWatch(context.Background(), restarted, opts, time.Millisecond, 2, nil)
	if err != nil || len(results) != 2 || results[0].ImportRows != 1 || results[1].ImportRows != 0 {
		t.Fatalf("restart replayed or lost scalar rows: results=%+v err=%v", results, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("remote writes = %d, want exactly two scalar samples", requests.Load())
	}
}

func TestMetricsOffloadWatchNonScalarShutdown(t *testing.T) {
	for _, mode := range []string{"signal", "completion", "context-cancel"} {
		t.Run(mode, func(t *testing.T) {
			history := filepath.Join(t.TempDir(), "history.jsonl")
			const metadata = "{\"_step\":0,\"_timestamp\":1770000000,\"phase\":\"starting\"}\n"
			const tail = `{"_step":1,"_timestamp":1770000001,"phase":"finished"}`
			appendMetricsHistoryForTest(t, history, metadata+tail)
			opts := metricsOffloadOptions{
				History:    []string{history},
				RunID:      mode,
				RunGroupID: "bounded",
				Out:        filepath.Join(t.TempDir(), "out"),
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			shutdown := make(chan metricsCompletionStatus, 1)
			if mode == "context-cancel" {
				cancel()
			} else {
				shutdown <- metricsSidecarShutdownStatus("SIGTERM")
			}
			wantOffset := int64(len(metadata))
			if mode == "completion" {
				opts.CompletionFile = filepath.Join(t.TempDir(), "complete")
				if err := os.WriteFile(opts.CompletionFile, nil, 0o644); err != nil {
					t.Fatal(err)
				}
				wantOffset += int64(len(tail))
			}
			results, err := runMetricsOffloadWatch(ctx, newTestMetricsOffloadStore(t, mode), opts, time.Hour, 0, shutdown)
			if mode == "context-cancel" {
				if !errors.Is(err, context.Canceled) || len(results) != 1 || results[0].StatusState != "" {
					t.Fatalf("context cancellation = %+v, %v", results, err)
				}
			} else {
				wantState := "cancelled"
				if mode == "completion" {
					wantState = "succeeded"
				}
				if err != nil || len(results) != 1 || results[0].ImportRows != 0 || results[0].StatusState != wantState || results[0].Completed != (mode == "completion") {
					t.Fatalf("metadata-only shutdown = %+v, %v", results, err)
				}
			}
			requireMetricsHistoryOffsetForTest(t, opts, history, wantOffset)
		})
	}
}

func appendMetricsHistoryForTest(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := f.WriteString(text)
	closeErr := f.Close()
	if writeErr != nil {
		t.Fatal(writeErr)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func requireMetricsHistoryOffsetForTest(t *testing.T, opts metricsOffloadOptions, history string, offset int64) {
	t.Helper()
	checkpoints, err := jsonlutil.ReadFileCheckpointSet(filepath.Join(opts.Out, "metrics_jsonl_checkpoint.json"), metricsJSONLCheckpointSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint, ok := checkpoints.Files[history]; !ok || checkpoint.Offset != offset || (offset > 0 && checkpoint.PrefixSHA256 == "") {
		t.Fatalf("checkpoint for %s = %+v (present=%t), want offset %d with durable prefix identity", history, checkpoint, ok, offset)
	}
}
