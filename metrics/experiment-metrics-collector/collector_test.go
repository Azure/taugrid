// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
)

const historyRow = `{"_step":1,"_timestamp":1700000000.25,"train/loss":0.5}` + "\n"

type recordingSink struct {
	name       string
	config     string
	donePath   string
	fail       error
	failMetric string
	deliveries int
	chunks     []MetricEventChunk
	mu         sync.Mutex
}

type boundedContextSink struct {
	recordingSink
	contexts int
	budget   time.Duration
}

func (s *recordingSink) Name() string           { return s.name }
func (s *recordingSink) ConfigIdentity() string { return s.config }
func (s *recordingSink) Deliver(_ context.Context, chunk MetricEventChunk) (DeliveryAck, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.donePath != "" {
		if _, err := os.Stat(s.donePath); err == nil {
			return DeliveryAck{}, errors.New("done file existed before delivery")
		}
	}
	if s.fail != nil {
		return DeliveryAck{}, s.fail
	}
	for _, event := range chunk.Events {
		if event.MetricName == s.failMetric {
			return DeliveryAck{}, errors.New("selected metric failed")
		}
	}
	s.deliveries++
	s.chunks = append(s.chunks, chunk)
	return DeliveryAck{Samples: len(chunk.Events)}, nil
}

func (s *boundedContextSink) Deliver(ctx context.Context, chunk MetricEventChunk) (DeliveryAck, error) {
	if err := ctx.Err(); err != nil {
		return DeliveryAck{}, fmt.Errorf("delivery inherited cancelled context: %w", err)
	}
	if _, ok := ctx.Deadline(); !ok {
		return DeliveryAck{}, errors.New("delivery context has no deadline")
	}
	s.contexts++
	return s.recordingSink.Deliver(ctx, chunk)
}

func (s *boundedContextSink) TerminalDrainTimeout() (time.Duration, error) {
	return s.budget, nil
}

func baseOptions(root, history string, sink Sink) Options {
	return Options{
		Run: "run-1", Project: "project", Experiment: "experiment", Group: "group",
		Source: "stellar-online", Out: filepath.Join(root, "out"), History: []string{history},
		Interval: 5 * time.Millisecond, Sink: sink,
		Now: func() time.Time { return time.Unix(1700000100, 0).UTC() },
	}
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runCollector(t *testing.T, options Options) Result {
	t.Helper()
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func runnerConfigIdentity(t *testing.T, options Options) string {
	t.Helper()
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	return runner.configIdentity()
}

func TestRunnerRequiresSink(t *testing.T) {
	options := baseOptions(t.TempDir(), filepath.Join(t.TempDir(), "history.jsonl"), nil)
	options.Sink = nil
	if _, err := New(options); err == nil || !strings.Contains(err.Error(), "required sink") {
		t.Fatalf("error=%v, want required sink validation", err)
	}
}

func TestCanonicalChunkAndRestartNoReplay(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)

	first := runCollector(t, options)
	second := runCollector(t, options)
	if first.Events != 1 || second.Events != 0 || sink.deliveries != 1 {
		t.Fatalf("first=%+v second=%+v deliveries=%d", first, second, sink.deliveries)
	}
	if paths, err := filepath.Glob(filepath.Join(options.Out, "pending", "*.json")); err != nil || len(paths) != 0 {
		t.Fatalf("pending chunks=%v err=%v", paths, err)
	}
	raw := sink.chunks[0].NDJSON
	events, err := decodeCanonicalEvents(raw)
	if err != nil || len(events) != 1 {
		t.Fatalf("canonical events=%d err=%v", len(events), err)
	}
}

func TestIncompleteTrailingLineWaitsForNewline(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, strings.TrimSuffix(historyRow, "\n"))
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)

	if got := runCollector(t, options); got.Events != 0 {
		t.Fatalf("events=%d, want 0", got.Events)
	}
	f, err := os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runCollector(t, options); got.Events != 1 {
		t.Fatalf("events=%d, want 1", got.Events)
	}
}

func TestCompletionDrainsValidUnterminatedTrailingLine(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completion := filepath.Join(root, "completion.json")
	writeFile(t, history, strings.TrimSuffix(historyRow, "\n"))
	writeFile(t, completion, `{"state":"succeeded","completed_at":"2023-11-14T22:15:00Z"}`)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	options.CompletionFile = completion

	result := runCollector(t, options)
	if !result.Completed || result.Events != 2 {
		t.Fatalf("result=%+v, want one metric plus terminal status", result)
	}
	if len(sink.chunks) != 2 || len(sink.chunks[0].Events) != 1 ||
		sink.chunks[0].Events[0].MetricName != "train/loss" {
		t.Fatalf("delivered chunks=%+v", sink.chunks)
	}
}

func TestUnterminatedFinalRecordReplaysAcrossCrashWindows(t *testing.T) {
	for _, point := range []faultPoint{faultAfterChunkWrite, faultAfterCheckpointWrite} {
		t.Run(string(point), func(t *testing.T) {
			root := t.TempDir()
			history := filepath.Join(root, "history.jsonl")
			completion := filepath.Join(root, "completion.json")
			writeFile(t, history, strings.TrimSuffix(historyRow, "\n"))
			writeFile(t, completion, `{"state":"succeeded","completed_at":"2023-11-14T22:15:00Z"}`)
			sink := &recordingSink{name: "test", config: "v1"}
			options := baseOptions(root, history, sink)
			options.CompletionFile = completion
			options.fault = failOnce(point)

			runner, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runner.Run(context.Background()); err == nil {
				t.Fatalf("fault %s unexpectedly succeeded", point)
			}

			options.fault = nil
			result := runCollector(t, options)
			if !result.Completed || sink.deliveries != 2 {
				t.Fatalf("result=%+v deliveries=%d, want metric and terminal delivery", result, sink.deliveries)
			}
			raw, err := os.ReadFile(filepath.Join(options.Out, "checkpoint.json"))
			if err != nil {
				t.Fatal(err)
			}
			var checkpoints checkpointSet
			if err := decodeOneJSON(raw, &checkpoints); err != nil {
				t.Fatal(err)
			}
			if got := checkpoints.Sources[history].Lines; got != 1 {
				t.Fatalf("checkpoint lines=%d, want 1", got)
			}
		})
	}
}

func TestBaselineSkipsExistingCompleteHistoryAndPublishesReady(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	ready := filepath.Join(root, "ready")
	writeFile(t, history, historyRow)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	options.BaselineExistingHistory = true
	options.ReadyFile = ready

	if got := runCollector(t, options); got.Events != 0 {
		t.Fatalf("baseline events=%d", got.Events)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"_step":2,"_timestamp":1700000001,"eval/score":2}` + "\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if got := runCollector(t, options); got.Events != 1 {
		t.Fatalf("post-baseline events=%d", got.Events)
	}
}

func TestWatchDrainsLineAppendedImmediatelyBeforeCompletion(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completionPath := filepath.Join(root, "completion.json")
	writeFile(t, history, "")
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	options.Watch = true
	options.CompletionFile = completionPath
	options.MaxIterations = 20
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	resultCh := make(chan Result, 1)
	errCh := make(chan error, 1)
	go func() {
		result, err := runner.Run(context.Background())
		resultCh <- result
		errCh <- err
	}()
	time.Sleep(15 * time.Millisecond)
	writeFile(t, history, historyRow)
	writeFile(t, completionPath, `{"state":"succeeded","completed_at":"2023-11-14T22:15:00Z"}`)
	result := <-resultCh
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if !result.Completed || result.Events != 2 {
		t.Fatalf("result=%+v, want history plus status", result)
	}
}

func TestCompletionStatusDeliveredBeforeDone(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completionPath := filepath.Join(root, "completion.json")
	done := filepath.Join(root, "done")
	writeFile(t, history, historyRow)
	writeFile(t, completionPath, `{"state":"failed","reason":"exit","message":"code 2","completed_at":"2023-11-14T22:15:00Z"}`)
	sink := &recordingSink{name: "test", config: "v1", donePath: done}
	options := baseOptions(root, history, sink)
	options.CompletionFile = completionPath
	options.DoneFile = done
	options.StatusArtifactURI = "https://example/artifact"
	options.StatusCheckpointURI = "https://example/checkpoint"

	result := runCollector(t, options)
	if !result.Completed {
		t.Fatal("collector did not complete")
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatal(err)
	}
	if len(sink.chunks) != 2 {
		t.Fatalf("chunks=%d, want metric and status", len(sink.chunks))
	}
	status := sink.chunks[1].Events[0]
	if status.MetricName != exptelemetry.RunStatusMetricName || status.Value != -1 ||
		status.Tags[exptelemetry.RunStatusReasonTag] != "exit" ||
		status.Tags[exptelemetry.RunStatusArtifactURITag] == "" ||
		status.Tags[exptelemetry.RunStatusCheckpointURITag] == "" {
		t.Fatalf("status=%+v", status)
	}
}

func TestSinkAndStatusFailureDoNotPublishDone(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completionPath := filepath.Join(root, "completion.json")
	done := filepath.Join(root, "done")
	writeFile(t, history, historyRow)
	writeFile(t, completionPath, `{"state":"succeeded"}`)
	sink := &recordingSink{name: "test", config: "v1", fail: errors.New("sink down")}
	options := baseOptions(root, history, sink)
	options.CompletionFile, options.DoneFile = completionPath, done
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("sink failure unexpectedly succeeded")
	}
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Fatalf("done file exists after sink failure: %v", err)
	}
}

func TestStatusDeliveryFailureDoesNotPublishDone(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completionPath := filepath.Join(root, "completion.json")
	done := filepath.Join(root, "done")
	writeFile(t, history, historyRow)
	writeFile(t, completionPath, `{"state":"succeeded"}`)
	sink := &recordingSink{name: "test", config: "v1", failMetric: exptelemetry.RunStatusMetricName}
	options := baseOptions(root, history, sink)
	options.CompletionFile, options.DoneFile = completionPath, done
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "terminal status") {
		t.Fatalf("error=%v, want terminal status delivery failure", err)
	}
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Fatalf("done file exists after status failure: %v", err)
	}
}

func TestRequiredADXFinalStatusFailureDoesNotPublishDone(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completionPath := filepath.Join(root, "completion.json")
	done := filepath.Join(root, "done")
	writeFile(t, history, "")
	writeFile(t, completionPath, `{"state":"succeeded","completed_at":"2023-11-14T22:15:00Z"}`)
	client := &fakeADXClient{results: []adxQueuedResult{
		&fakeADXResult{status: adxStatusQueued},
	}}
	sink := adxTestSink(client)
	options := baseOptions(root, history, sink)
	options.CompletionFile, options.DoneFile = completionPath, done

	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if result, err := runner.Run(context.Background()); err == nil || result.Completed {
		t.Fatalf("required non-final ADX status completed: result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(done); !os.IsNotExist(err) {
		t.Fatalf("done file exists after non-final ADX status: %v", err)
	}
}

func TestCorruptCheckpointAndChunkFailClosed(t *testing.T) {
	t.Run("checkpoint", func(t *testing.T) {
		root := t.TempDir()
		history := filepath.Join(root, "history.jsonl")
		writeFile(t, history, historyRow)
		options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
		writeFile(t, filepath.Join(options.Out, "checkpoint.json"), "{broken")
		runner, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(context.Background()); err == nil {
			t.Fatal("corrupt checkpoint unexpectedly succeeded")
		}
	})
	t.Run("pending chunk", func(t *testing.T) {
		root := t.TempDir()
		history := filepath.Join(root, "history.jsonl")
		writeFile(t, history, historyRow)
		options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
		writeFile(t, filepath.Join(options.Out, "pending", "00000000000000000001-deadbeef.json"), "broken\n")
		runner, err := New(options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runner.Run(context.Background()); err == nil {
			t.Fatal("corrupt chunk unexpectedly succeeded")
		}
	})
}

func TestCheckpointPrefixMismatchFailsClosed(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	runCollector(t, options)
	writeFile(t, history, strings.Replace(historyRow, "0.5", "0.7", 1))
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "prefix mismatch") {
		t.Fatalf("error=%v, want prefix mismatch", err)
	}
}

func TestInvalidHistoryRowFailsClosed(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, `{"_timestamp":1700000000,"loss":1}`+"\n")
	options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("invalid row unexpectedly succeeded")
	}
}

func testEvent(t *testing.T, name string) exptelemetry.MetricEvent {
	t.Helper()
	event, err := exptelemetry.NewMetricEvent(exptelemetry.MetricEvent{
		Project: "project", ExperimentID: "experiment", RunGroupID: "group", RunID: "run-1",
		MetricName: name, Step: 1, WallTime: time.Unix(1700000000, 0).UTC(), Value: 1,
		Source: "test", ExportedAt: time.Unix(1700000001, 0).UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestCompletionFallsBackToFileModTime(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "completion")
	writeFile(t, path, `{"state":"cancelled"}`)
	want := time.Unix(1700000200, 0).UTC()
	if err := os.Chtimes(path, want, want); err != nil {
		t.Fatal(err)
	}
	got, err := readCompletion(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "cancelled" || !got.CompletedAt.Equal(want) {
		t.Fatalf("completion=%+v", got)
	}
}

func TestCheckpointContainsIdentityPrefixOffsetAndSequence(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
	runCollector(t, options)
	raw, err := os.ReadFile(filepath.Join(options.Out, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint checkpointSet
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		t.Fatal(err)
	}
	if checkpoint.ConfigIdentity != runnerConfigIdentity(t, options) {
		t.Fatalf("checkpoint config identity=%q", checkpoint.ConfigIdentity)
	}
	source := checkpoint.Sources[history]
	if source.FileID == "" || source.PrefixSHA256 == "" || source.Offset != int64(len(historyRow)) || source.Sequence != 1 {
		t.Fatalf("source checkpoint=%+v", source)
	}
	if source.ChunkDigest == "" {
		t.Fatal("source checkpoint does not reference its durable chunk")
	}
}

func TestMetadataOnlyProgressCheckpointRestarts(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	metadataOnly := `{"_step":2,"_timestamp":1700000001,"note":"checkpoint"}` + "\n"
	writeFile(t, history, historyRow+metadataOnly)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)

	first := runCollector(t, options)
	second := runCollector(t, options)
	if first.Events != 1 || second.Events != 0 || sink.deliveries != 1 {
		t.Fatalf("first=%+v second=%+v deliveries=%d", first, second, sink.deliveries)
	}
	raw, err := os.ReadFile(filepath.Join(options.Out, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints checkpointSet
	if err := json.Unmarshal(raw, &checkpoints); err != nil {
		t.Fatal(err)
	}
	checkpoint := checkpoints.Sources[history]
	if checkpoint.Offset != int64(len(historyRow)+len(metadataOnly)) ||
		checkpoint.Sequence != 1 || checkpoint.ChunkDigest == "" {
		t.Fatalf("metadata checkpoint=%+v", checkpoint)
	}
}

func TestHistoryCrashBoundariesReplaySameDurableChunk(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		point                  faultPoint
		deliveriesAfterRun     int
		deliveriesAfterRestart int
	}{
		{name: "after chunk write", point: faultAfterChunkWrite, deliveriesAfterRun: 0, deliveriesAfterRestart: 1},
		{name: "after checkpoint write", point: faultAfterCheckpointWrite, deliveriesAfterRun: 1, deliveriesAfterRestart: 1},
		{name: "after sink accept", point: faultAfterSinkAccept, deliveriesAfterRun: 1, deliveriesAfterRestart: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			history := filepath.Join(root, "history.jsonl")
			writeFile(t, history, historyRow)
			sink := &recordingSink{name: "test", config: "v1"}
			options := baseOptions(root, history, sink)
			options.Now = func() time.Time { return time.Unix(1700000100, 0).UTC() }
			options.fault = failOnce(tt.point)
			runner, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runner.Run(context.Background()); err == nil {
				t.Fatalf("fault %s unexpectedly succeeded", tt.point)
			}
			if sink.deliveries != tt.deliveriesAfterRun {
				t.Fatalf("deliveries after fault=%d, want %d", sink.deliveries, tt.deliveriesAfterRun)
			}
			manifests, err := filepath.Glob(filepath.Join(options.Out, "pending", "*.json"))
			if err != nil || len(manifests) != 1 {
				t.Fatalf("manifests=%v err=%v", manifests, err)
			}
			var durable chunkManifest
			raw, err := os.ReadFile(manifests[0])
			if err != nil || decodeOneJSON(raw, &durable) != nil {
				t.Fatalf("read durable manifest: %v", err)
			}

			options.fault = nil
			options.Now = func() time.Time { return time.Unix(1800000000, 0).UTC() }
			runCollector(t, options)
			if sink.deliveries != tt.deliveriesAfterRestart {
				t.Fatalf("deliveries after restart=%d, want %d", sink.deliveries, tt.deliveriesAfterRestart)
			}
			for _, chunk := range sink.chunks {
				if chunk.Digest != durable.ChunkDigest {
					t.Fatalf("restarted digest=%s, durable digest=%s", chunk.Digest, durable.ChunkDigest)
				}
			}
			chunks, err := filepath.Glob(filepath.Join(options.Out, "pending", "*.json"))
			if err != nil || len(chunks) != 0 {
				t.Fatalf("acknowledged chunks=%v err=%v", chunks, err)
			}
		})
	}
}

func TestTerminalCrashBoundariesReplaySameChunkBeforeDone(t *testing.T) {
	for _, tt := range []struct {
		name                   string
		point                  faultPoint
		deliveriesAfterRun     int
		deliveriesAfterRestart int
	}{
		{name: "after terminal chunk write", point: faultAfterTerminalChunkWrite, deliveriesAfterRun: 0, deliveriesAfterRestart: 1},
		{name: "after terminal checkpoint write", point: faultAfterTerminalCheckpointWrite, deliveriesAfterRun: 1, deliveriesAfterRestart: 1},
		{name: "after terminal sink accept", point: faultAfterTerminalSinkAccept, deliveriesAfterRun: 1, deliveriesAfterRestart: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			history := filepath.Join(root, "history.jsonl")
			completionPath := filepath.Join(root, "completion.json")
			done := filepath.Join(root, "done")
			writeFile(t, history, "")
			writeFile(t, completionPath, `{"state":"succeeded","completed_at":"2023-11-14T22:15:00Z"}`)
			sink := &recordingSink{name: "test", config: "v1", donePath: done}
			options := baseOptions(root, history, sink)
			options.CompletionFile, options.DoneFile = completionPath, done
			options.fault = failOnce(tt.point)
			runner, err := New(options)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runner.Run(context.Background()); err == nil {
				t.Fatalf("fault %s unexpectedly succeeded", tt.point)
			}
			if _, err := os.Stat(done); !os.IsNotExist(err) {
				t.Fatalf("done exists before terminal delivery recovery: %v", err)
			}
			if sink.deliveries != tt.deliveriesAfterRun {
				t.Fatalf("deliveries after fault=%d, want %d", sink.deliveries, tt.deliveriesAfterRun)
			}
			manifests, err := filepath.Glob(filepath.Join(options.Out, "pending", "*.json"))
			if err != nil || len(manifests) != 1 {
				t.Fatalf("manifests=%v err=%v", manifests, err)
			}
			var durable chunkManifest
			raw, err := os.ReadFile(manifests[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := decodeOneJSON(raw, &durable); err != nil {
				t.Fatal(err)
			}

			options.fault = nil
			result := runCollector(t, options)
			if !result.Completed {
				t.Fatal("restart did not complete")
			}
			if _, err := os.Stat(done); err != nil {
				t.Fatal(err)
			}
			if sink.deliveries != tt.deliveriesAfterRestart {
				t.Fatalf("deliveries after restart=%d, want %d", sink.deliveries, tt.deliveriesAfterRestart)
			}
			for _, chunk := range sink.chunks {
				if chunk.Digest != durable.ChunkDigest {
					t.Fatalf("restarted digest=%s, durable digest=%s", chunk.Digest, durable.ChunkDigest)
				}
			}
		})
	}
}

func TestCancellationDrainsTrailingHistoryAndPublishesCancelledStatus(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, strings.TrimSuffix(historyRow, "\n"))
	sink := &boundedContextSink{
		recordingSink: recordingSink{name: "test", config: "v1"},
		budget:        250 * time.Millisecond,
	}
	options := baseOptions(root, history, sink)
	options.Watch = true
	options.Interval = time.Hour
	options.Now = func() time.Time { return time.Unix(1700000200, 0).UTC() }
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	result, err := runner.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error=%v, want context cancellation", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancelled drain took %s, want bounded exit", elapsed)
	}
	if result.Events != 2 || result.Chunks != 2 || sink.contexts != 2 || len(sink.chunks) != 2 {
		t.Fatalf("result=%+v contexts=%d chunks=%d", result, sink.contexts, len(sink.chunks))
	}
	if sink.chunks[0].Events[0].MetricName == exptelemetry.RunStatusMetricName {
		t.Fatal("terminal status was published before trailing history")
	}
	status := sink.chunks[1].Events[0]
	if status.MetricName != exptelemetry.RunStatusMetricName ||
		status.Tags[exptelemetry.RunStatusStateTag] != "cancelled" {
		t.Fatalf("terminal event=%+v, want cancelled run status", status)
	}
}

func TestCancellationWaitsForActualCompletionBeforeFinalDrain(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completionPath := filepath.Join(root, "completion.json")
	donePath := filepath.Join(root, "done")
	writeFile(t, history, historyRow)
	sink := &boundedContextSink{
		recordingSink: recordingSink{name: "test", config: "v1"},
		budget:        500 * time.Millisecond,
	}
	options := baseOptions(root, history, sink)
	options.CompletionFile = completionPath
	options.DoneFile = donePath
	options.Watch = true
	options.Interval = 10 * time.Millisecond
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	writeErr := make(chan error, 1)
	go func() {
		time.Sleep(30 * time.Millisecond)
		file, err := os.OpenFile(history, os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, err = file.WriteString(`{"_step":2,"_timestamp":1700000001,"eval/score":2}` + "\n")
			closeErr := file.Close()
			if err == nil {
				err = closeErr
			}
		}
		if err == nil {
			temp := completionPath + ".tmp"
			err = os.WriteFile(
				temp,
				[]byte(`{"state":"succeeded","completed_at":"2023-11-14T22:16:00Z"}`),
				0o644,
			)
			if err == nil {
				err = os.Rename(temp, completionPath)
			}
		}
		writeErr <- err
	}()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := runner.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error=%v, want original context cancellation", err)
	}
	if err := <-writeErr; err != nil {
		t.Fatal(err)
	}
	if !result.Completed {
		t.Fatalf("result=%+v, want actual completion publication", result)
	}
	if _, err := os.Stat(donePath); err != nil {
		t.Fatalf("done file was not published: %v", err)
	}
	if len(sink.chunks) != 3 {
		t.Fatalf("delivered chunks=%d, want pending history, trailing history, terminal", len(sink.chunks))
	}
	for index := 0; index < 2; index++ {
		if sink.chunks[index].Events[0].MetricName == exptelemetry.RunStatusMetricName {
			t.Fatalf("chunk %d published terminal status before history", index)
		}
	}
	status := sink.chunks[2].Events[0]
	if status.MetricName != exptelemetry.RunStatusMetricName ||
		status.Tags[exptelemetry.RunStatusStateTag] != "succeeded" {
		t.Fatalf("terminal event=%+v, want actual succeeded state", status)
	}
}

func TestRetryPublishesFailedThenSucceededTerminalObservationsOnce(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completionPath := filepath.Join(root, "completion.json")
	writeFile(t, history, "")
	writeFile(t, completionPath, `{"state":"failed","reason":"exit","completed_at":"2023-11-14T22:15:00Z"}`)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	options.CompletionFile = completionPath

	first := runCollector(t, options)
	if !first.Completed || len(sink.chunks) != 1 {
		t.Fatalf("failed attempt result=%+v chunks=%d", first, len(sink.chunks))
	}
	writeFile(t, completionPath, `{"state":"succeeded","completed_at":"2023-11-14T22:16:00Z"}`)
	second := runCollector(t, options)
	if !second.Completed || len(sink.chunks) != 2 {
		t.Fatalf("successful retry result=%+v chunks=%d", second, len(sink.chunks))
	}
	third := runCollector(t, options)
	if !third.Completed || len(sink.chunks) != 2 {
		t.Fatalf("replay result=%+v chunks=%d, want no duplicate terminal delivery", third, len(sink.chunks))
	}
	var states []string
	for _, chunk := range sink.chunks {
		if len(chunk.Events) != 1 || chunk.Events[0].MetricName != exptelemetry.RunStatusMetricName {
			t.Fatalf("terminal chunk=%+v", chunk)
		}
		states = append(states, chunk.Events[0].Tags[exptelemetry.RunStatusStateTag])
	}
	if strings.Join(states, ",") != "failed,succeeded" {
		t.Fatalf("terminal states=%v, want failed then succeeded", states)
	}
	raw, err := os.ReadFile(filepath.Join(options.Out, "checkpoint.json"))
	if err != nil {
		t.Fatal(err)
	}
	var checkpoints checkpointSet
	if err := json.Unmarshal(raw, &checkpoints); err != nil {
		t.Fatal(err)
	}
	if len(checkpoints.Terminals) != 2 {
		t.Fatalf("terminal checkpoints=%d, want 2", len(checkpoints.Terminals))
	}
	pending, err := filepath.Glob(filepath.Join(options.Out, "pending", "*.json"))
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending terminal observations=%v err=%v", pending, err)
	}
}

func TestPendingDurableChunkIsDeliveredBeforeReadingNewHistory(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	options.fault = failOnce(faultAfterChunkWrite)
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("chunk-write fault unexpectedly succeeded")
	}
	var durable chunkManifest
	raw, err := os.ReadFile(onlyPath(t, filepath.Join(options.Out, "pending", "*.json")))
	if err != nil {
		t.Fatal(err)
	}
	if err := decodeOneJSON(raw, &durable); err != nil {
		t.Fatal(err)
	}
	appendFile(t, history, `{"_step":2,"_timestamp":1700000001,"eval/score":2}`+"\n")
	options.fault = nil
	options.Now = func() time.Time { return time.Unix(1800000000, 0).UTC() }
	runCollector(t, options)
	if len(sink.chunks) != 2 {
		t.Fatalf("delivered chunks=%d, want durable then new", len(sink.chunks))
	}
	if sink.chunks[0].Digest != durable.ChunkDigest || sink.chunks[0].Sequence != 1 || sink.chunks[1].Sequence != 2 {
		t.Fatalf("delivery order=%d:%s then %d:%s", sink.chunks[0].Sequence, sink.chunks[0].Digest, sink.chunks[1].Sequence, sink.chunks[1].Digest)
	}
}

func TestPendingSpoolRejectsSinkConfigurationChanges(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	original := &recordingSink{name: "remote-write-v1", config: "endpoint-a"}
	options := baseOptions(root, history, original)
	options.fault = failOnce(faultAfterChunkWrite)
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("chunk-write fault unexpectedly succeeded")
	}

	reconfigured := &recordingSink{name: "remote-write-v1", config: "endpoint-b"}
	options.Sink = reconfigured
	options.fault = nil
	expectRunError(t, options, "configuration")
}

func TestCheckpointRejectsSinkConfigurationChanges(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "history"
		if terminal {
			name = "terminal"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			history := filepath.Join(root, "history.jsonl")
			writeFile(t, history, historyRow)
			options := baseOptions(root, history, &recordingSink{name: "remote-write-v1", config: "endpoint-a"})
			if terminal {
				options.CompletionFile = filepath.Join(root, "complete")
				writeFile(t, options.CompletionFile, "succeeded\n")
			}
			runCollector(t, options)

			options.Sink = &recordingSink{name: "remote-write-v1", config: "endpoint-b"}
			expectRunError(t, options, "different collector or sink configuration")
		})
	}
}

func TestAbandonedWriterTempsAreDiscarded(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		options, sink := pendingSpool(t)
		pending := onlyPath(t, filepath.Join(options.Out, "pending", "*.json"))
		writeFile(t, filepath.Join(filepath.Dir(pending), "."+filepath.Base(pending)+".tmp-dead"), "{")
		runCollector(t, options)
		if sink.deliveries != 1 {
			t.Fatalf("deliveries=%d, want 1", sink.deliveries)
		}
	})
	t.Run("unknown file remains fail closed", func(t *testing.T) {
		options, _ := pendingSpool(t)
		writeFile(t, filepath.Join(options.Out, "pending", ".unknown.tmp-dead"), "partial")
		expectRunError(t, options, "unexpected file")
	})
	t.Run("unknown directory remains fail closed", func(t *testing.T) {
		options, _ := pendingSpool(t)
		if err := os.Mkdir(filepath.Join(options.Out, "pending", "unknown"), 0o755); err != nil {
			t.Fatal(err)
		}
		expectRunError(t, options, "unexpected directory")
	})
}

func TestDurableWriteReportsSyncFailures(t *testing.T) {
	syncErr := errors.New("injected sync failure")
	for _, relative := range []string{
		"checkpoint.json",
		filepath.Join("pending", strings.Repeat("0", 20)+"-"+strings.Repeat("a", 64)+".json"),
	} {
		t.Run(relative, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), relative)
			ops := defaultStorage()
			ops.syncDir = func(dir string) error {
				if dir == filepath.Dir(path) {
					return syncErr
				}
				return syncDirectory(dir)
			}
			err := writeFileDurable(path, []byte("{}\n"), 0o644, ops)
			if !errors.Is(err, syncErr) {
				t.Fatalf("error=%v, want %v", err, syncErr)
			}
		})
	}

	path := filepath.Join(t.TempDir(), "pending", strings.Repeat("0", 20)+"-"+strings.Repeat("d", 64)+".json")
	ops := defaultStorage()
	ops.syncFile = func(*os.File) error { return syncErr }
	if err := writeFileDurable(path, []byte("{}\n"), 0o644, ops); !errors.Is(err, syncErr) {
		t.Fatalf("file sync error=%v, want %v", err, syncErr)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file-sync failure published destination: %v", err)
	}
}

func TestSourceSyncFailurePreventsDurableProgress(t *testing.T) {
	syncErr := errors.New("source sync failed")
	t.Run("history", func(t *testing.T) {
		root := t.TempDir()
		history := filepath.Join(root, "history.jsonl")
		writeFile(t, history, historyRow)
		options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
		options.syncSource = func(*os.File) error { return syncErr }
		expectRunError(t, options, "source sync failed")
		if _, err := os.Stat(filepath.Join(options.Out, "checkpoint.json")); !os.IsNotExist(err) {
			t.Fatalf("source sync failure wrote checkpoint: %v", err)
		}
		if pending, err := filepath.Glob(filepath.Join(options.Out, "pending", "*.json")); err != nil || len(pending) != 0 {
			t.Fatalf("source sync failure wrote pending chunks=%v err=%v", pending, err)
		}
	})
	t.Run("baseline", func(t *testing.T) {
		root := t.TempDir()
		history := filepath.Join(root, "history.jsonl")
		writeFile(t, history, historyRow)
		options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
		options.BaselineExistingHistory = true
		options.ReadyFile = filepath.Join(root, "ready")
		options.syncSource = func(*os.File) error { return syncErr }
		expectRunError(t, options, "source sync failed")
		if _, err := os.Stat(filepath.Join(options.Out, "checkpoint.json")); !os.IsNotExist(err) {
			t.Fatalf("baseline sync failure wrote checkpoint: %v", err)
		}
	})
}

func TestAcknowledgedPayloadsAreDeletedAndRestartIsBounded(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, "")
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	for index := 1; index <= 25; index++ {
		appendFile(t, history, fmt.Sprintf(`{"_step":%d,"_timestamp":%d,"train/loss":%f}`+"\n", index, 1700000000+index, float64(index)))
		runCollector(t, options)
	}
	deliveries := sink.deliveries
	if pending, err := filepath.Glob(filepath.Join(options.Out, "pending", "*.json")); err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged pending chunks=%v err=%v", pending, err)
	}
	runCollector(t, options)
	if sink.deliveries != deliveries {
		t.Fatalf("restart redelivered compacted history: before=%d after=%d", deliveries, sink.deliveries)
	}
}

func TestSpoolMetadataAndJSONFramingFailClosed(t *testing.T) {
	t.Run("pending schema", func(t *testing.T) {
		options, _ := pendingSpool(t)
		path := onlyPath(t, filepath.Join(options.Out, "pending", "*.json"))
		rewriteJSONField(t, path, "schema_version", "unknown")
		expectRunError(t, options, "pending")
	})
	t.Run("pending digest", func(t *testing.T) {
		options, _ := pendingSpool(t)
		path := onlyPath(t, filepath.Join(options.Out, "pending", "*.json"))
		rewriteJSONField(t, path, "chunk_digest", strings.Repeat("0", 64))
		expectRunError(t, options, "digest")
	})
	t.Run("collector config", func(t *testing.T) {
		options, _ := pendingSpool(t)
		options.Project = "other-project"
		expectRunError(t, options, "configuration")
	})
	t.Run("pending trailing object", func(t *testing.T) {
		options, _ := pendingSpool(t)
		appendFile(t, onlyPath(t, filepath.Join(options.Out, "pending", "*.json")), "{}")
		expectRunError(t, options, "pending")
	})
	t.Run("checkpoint trailing object", func(t *testing.T) {
		options, _ := completedSpool(t)
		appendFile(t, filepath.Join(options.Out, "checkpoint.json"), "{}")
		expectRunError(t, options, "checkpoint")
	})
	t.Run("checkpoint chunk reference", func(t *testing.T) {
		options, _ := completedSpool(t)
		path := filepath.Join(options.Out, "checkpoint.json")
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var checkpoint map[string]any
		if err := json.Unmarshal(raw, &checkpoint); err != nil {
			t.Fatal(err)
		}
		sources := checkpoint["sources"].(map[string]any)
		for _, source := range sources {
			source.(map[string]any)["chunk_digest"] = "wrong"
		}
		raw, err = json.Marshal(checkpoint)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
		expectRunError(t, options, "checkpoint")
	})
	t.Run("history trailing object", func(t *testing.T) {
		root := t.TempDir()
		history := filepath.Join(root, "history.jsonl")
		writeFile(t, history, strings.TrimSuffix(historyRow, "\n")+" {}\n")
		options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
		expectRunError(t, options, "exactly one")
	})
	t.Run("completion trailing object", func(t *testing.T) {
		root := t.TempDir()
		history := filepath.Join(root, "history.jsonl")
		completion := filepath.Join(root, "completion.json")
		writeFile(t, history, "")
		writeFile(t, completion, `{"state":"succeeded"} {}`)
		options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
		options.CompletionFile = completion
		expectRunError(t, options, "completion")
	})
}

func failOnce(want faultPoint) func(faultPoint) error {
	failed := false
	return func(got faultPoint) error {
		if !failed && got == want {
			failed = true
			return fmt.Errorf("injected fault at %s", got)
		}
		return nil
	}
}

func completedSpool(t *testing.T) (Options, *recordingSink) {
	t.Helper()
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	runCollector(t, options)
	return options, sink
}

func pendingSpool(t *testing.T) (Options, *recordingSink) {
	t.Helper()
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	sink := &recordingSink{name: "test", config: "v1"}
	options := baseOptions(root, history, sink)
	options.fault = failOnce(faultAfterChunkWrite)
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("chunk-write fault unexpectedly succeeded")
	}
	options.fault = nil
	return options, sink
}

func onlyPath(t *testing.T, pattern string) string {
	t.Helper()
	paths, err := filepath.Glob(pattern)
	if err != nil || len(paths) != 1 {
		t.Fatalf("paths=%v err=%v", paths, err)
	}
	return paths[0]
}

func rewriteJSONField(t *testing.T, path, field string, value any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatal(err)
	}
	object[field] = value
	raw, err = json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, value string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(value); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func expectRunError(t *testing.T, options Options, contains string) {
	t.Helper()
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(contains)) {
		t.Fatalf("error=%v, want containing %q", err, contains)
	}
}
