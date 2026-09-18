// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/golang/snappy"
	"google.golang.org/protobuf/encoding/protowire"
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

func baseOptions(root, history string, sink Sink) Options {
	return Options{
		Run: "run-1", Project: "project", Experiment: "experiment", Group: "group",
		Source: "stellar-online", Out: filepath.Join(root, "out"), History: []string{history},
		Interval: 5 * time.Millisecond, Sinks: []Sink{sink},
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
	paths, err := filepath.Glob(filepath.Join(options.Out, "chunks", "*.ndjson"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("chunk paths=%v err=%v", paths, err)
	}
	raw, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
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

func TestReceiptReuseAndConfigInvalidation(t *testing.T) {
	root := t.TempDir()
	event := testEvent(t, "train/loss")
	raw, err := event.MarshalNDJSON()
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := writeChunk(root, MetricEventChunk{Sequence: 1, Events: []exptelemetry.MetricEvent{event}, NDJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	first := &recordingSink{name: "sink", config: "a"}
	if _, reused, err := deliverWithReceipt(context.Background(), root, first, chunk); err != nil || reused {
		t.Fatalf("first reused=%v err=%v", reused, err)
	}
	if _, reused, err := deliverWithReceipt(context.Background(), root, first, chunk); err != nil || !reused {
		t.Fatalf("second reused=%v err=%v", reused, err)
	}
	changed := &recordingSink{name: "sink", config: "b"}
	if _, reused, err := deliverWithReceipt(context.Background(), root, changed, chunk); err != nil || reused {
		t.Fatalf("changed reused=%v err=%v", reused, err)
	}
	if first.deliveries != 1 || changed.deliveries != 1 {
		t.Fatalf("deliveries first=%d changed=%d", first.deliveries, changed.deliveries)
	}
	otherEvent := testEvent(t, "eval/score")
	otherRaw, err := otherEvent.MarshalNDJSON()
	if err != nil {
		t.Fatal(err)
	}
	otherChunk, err := writeChunk(root, MetricEventChunk{Sequence: 2, Events: []exptelemetry.MetricEvent{otherEvent}, NDJSON: otherRaw})
	if err != nil {
		t.Fatal(err)
	}
	if _, reused, err := deliverWithReceipt(context.Background(), root, first, otherChunk); err != nil || reused {
		t.Fatalf("new digest reused=%v err=%v", reused, err)
	}
	if first.deliveries != 2 {
		t.Fatalf("new digest deliveries=%d, want 2", first.deliveries)
	}
}

func TestRemoteWriteRetriesRetryableAndRejectsPermanent(t *testing.T) {
	var attempts atomic.Int32
	retryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			http.Error(w, "retry", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer retryServer.Close()
	event := testEvent(t, "train/loss")
	sink := &RemoteWriteSink{Endpoint: retryServer.URL, BatchSize: 1, MaxAttempts: 3, Backoff: time.Millisecond}
	ack, err := sink.Deliver(context.Background(), MetricEventChunk{Events: []exptelemetry.MetricEvent{event}})
	if err != nil || ack.Retries != 2 || attempts.Load() != 3 {
		t.Fatalf("ack=%+v attempts=%d err=%v", ack, attempts.Load(), err)
	}

	attempts.Store(0)
	permanentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer permanentServer.Close()
	sink.Endpoint = permanentServer.URL
	if _, err := sink.Deliver(context.Background(), MetricEventChunk{Events: []exptelemetry.MetricEvent{event}}); err == nil {
		t.Fatal("permanent 4xx unexpectedly succeeded")
	}
	if attempts.Load() != 1 {
		t.Fatalf("permanent attempts=%d, want 1", attempts.Load())
	}
}

func TestRemoteWriteSeriesMatchesExistingContract(t *testing.T) {
	event := testEvent(t, "train/loss")
	event.SourceStoreID = "store"
	event.MetricFileID = "file-id"
	event.MetricFilePath = "/data/history.jsonl"
	event.Source = "stellar-online"
	event.Split = "train"
	event.Unit = "loss"
	event.Tags = map[string]string{exptelemetry.TauWorkspaceTag: "workspace", "model": "small"}
	event.EventID = ""
	event, err := exptelemetry.NewMetricEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	labels, value, timestamp := decodeSeriesForTest(t, encodeTimeSeries(event))
	want := map[string]string{
		"__name__": exptelemetry.RemoteWriteMetricName, "project": "project",
		"experiment_id": "experiment", "run_group_id": "group", "run_id": "run-1",
		"metric_name": "train/loss", "source": "stellar-online", "split": "train",
		"unit": "loss", "step": "1", "metric_file_id": "file-id",
		"metric_file_path": "/data/history.jsonl", "source_store_id": "store",
		"tags": `{"model":"small","tau_workspace":"workspace"}`, "workspace_id": "workspace",
	}
	if fmt.Sprint(labels) != fmt.Sprint(want) {
		t.Fatalf("labels=%v want=%v", labels, want)
	}
	if value != 1 || timestamp != event.WallTime.UnixMilli() {
		t.Fatalf("sample value=%v timestamp=%d", value, timestamp)
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

func TestOptionalShadowSinkFailureIsVisibleAndDoesNotBlockCompletion(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	completionPath := filepath.Join(root, "completion.json")
	done := filepath.Join(root, "done")
	writeFile(t, history, historyRow)
	writeFile(t, completionPath, `{"state":"succeeded","completed_at":"2023-11-14T22:15:00Z"}`)
	required := &recordingSink{name: "required", config: "v1"}
	shadow := &recordingSink{name: "shadow", config: "v1", fail: errors.New(strings.Repeat("failure ", 300))}
	options := baseOptions(root, history, required)
	options.OptionalSinks = []Sink{shadow}
	options.CompletionFile, options.DoneFile = completionPath, done

	result := runCollector(t, options)
	if !result.Completed {
		t.Fatal("optional shadow failure blocked terminal completion")
	}
	if _, err := os.Stat(done); err != nil {
		t.Fatal(err)
	}
	if len(result.OptionalSinkFailures) != 2 {
		t.Fatalf("optional failures=%+v, want history and terminal visibility", result.OptionalSinkFailures)
	}
	if result.OptionalSinkFailureCount != 2 {
		t.Fatalf("optional failure count=%d, want 2", result.OptionalSinkFailureCount)
	}
	for _, failure := range result.OptionalSinkFailures {
		if failure.Sink != "shadow" || failure.ChunkDigest == "" || len(failure.Error) > 1027 {
			t.Fatalf("invalid optional failure=%+v", failure)
		}
	}
	if required.deliveries != 2 {
		t.Fatalf("required deliveries=%d, want history and terminal", required.deliveries)
	}
}

func TestOptionalSinkFailureDetailsAreBoundedAndDeduplicated(t *testing.T) {
	result := &Result{}
	for i := 0; i < 300; i++ {
		recordOptionalSinkFailure(result, SinkFailure{
			Sink: "shadow", ChunkDigest: fmt.Sprintf("%064x", i), Error: "failed",
		})
	}
	recordOptionalSinkFailure(result, SinkFailure{
		Sink: "shadow", ChunkDigest: fmt.Sprintf("%064x", 0), Error: "latest failure",
	})
	if len(result.OptionalSinkFailures) != 256 {
		t.Fatalf("failure details=%d, want bounded 256", len(result.OptionalSinkFailures))
	}
	if result.OptionalSinkFailures[0].Error != "latest failure" {
		t.Fatalf("duplicate detail was not updated: %+v", result.OptionalSinkFailures[0])
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
	t.Run("chunk", func(t *testing.T) {
		root := t.TempDir()
		history := filepath.Join(root, "history.jsonl")
		writeFile(t, history, historyRow)
		options := baseOptions(root, history, &recordingSink{name: "test", config: "v1"})
		writeFile(t, filepath.Join(options.Out, "chunks", "00000000000000000001-deadbeef.ndjson"), "broken\n")
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

func decodeSeriesForTest(t *testing.T, raw []byte) (map[string]string, float64, int64) {
	t.Helper()
	labels := map[string]string{}
	var value float64
	var timestamp int64
	for len(raw) > 0 {
		number, wireType, n := protowire.ConsumeTag(raw)
		if n < 0 || wireType != protowire.BytesType {
			t.Fatalf("invalid time series tag: %d %v %d", number, wireType, n)
		}
		raw = raw[n:]
		field, n := protowire.ConsumeBytes(raw)
		if n < 0 {
			t.Fatalf("invalid time series field: %d", n)
		}
		raw = raw[n:]
		switch number {
		case 1:
			var name, labelValue string
			for len(field) > 0 {
				labelNumber, labelType, consumed := protowire.ConsumeTag(field)
				if consumed < 0 || labelType != protowire.BytesType {
					t.Fatal("invalid label")
				}
				field = field[consumed:]
				text, consumed := protowire.ConsumeString(field)
				if consumed < 0 {
					t.Fatal("invalid label value")
				}
				field = field[consumed:]
				if labelNumber == 1 {
					name = text
				} else if labelNumber == 2 {
					labelValue = text
				}
			}
			labels[name] = labelValue
		case 2:
			sampleNumber, sampleType, consumed := protowire.ConsumeTag(field)
			if sampleNumber != 1 || sampleType != protowire.Fixed64Type || consumed < 0 {
				t.Fatal("invalid sample value tag")
			}
			field = field[consumed:]
			bits, consumed := protowire.ConsumeFixed64(field)
			if consumed < 0 {
				t.Fatal("invalid sample value")
			}
			value = math.Float64frombits(bits)
			field = field[consumed:]
			sampleNumber, sampleType, consumed = protowire.ConsumeTag(field)
			if sampleNumber != 2 || sampleType != protowire.VarintType || consumed < 0 {
				t.Fatal("invalid sample timestamp tag")
			}
			field = field[consumed:]
			rawTimestamp, consumed := protowire.ConsumeVarint(field)
			if consumed < 0 {
				t.Fatal("invalid sample timestamp")
			}
			timestamp = int64(rawTimestamp)
		}
	}
	return labels, value, timestamp
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

func TestReceiptDigestTamperingFailsClosed(t *testing.T) {
	root := t.TempDir()
	event := testEvent(t, "metric")
	raw, _ := event.MarshalNDJSON()
	chunk, err := writeChunk(root, MetricEventChunk{Sequence: 1, Events: []exptelemetry.MetricEvent{event}, NDJSON: raw})
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{name: "sink", config: "v1"}
	receipt := receiptPath(root, sink, chunk.Digest)
	writeFile(t, receipt, fmt.Sprintf(`{"schema_version":%q,"chunk_digest":"wrong","config_identity":%q}`, ReceiptSchemaV1, sink.ConfigIdentity()))
	if _, _, err := deliverWithReceipt(context.Background(), root, sink, chunk); err == nil {
		t.Fatal("tampered receipt unexpectedly succeeded")
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
	source := checkpoint.Sources[history]
	if source.FileID == "" || source.PrefixSHA256 == "" || source.Offset != int64(len(historyRow)) || source.Sequence != 1 {
		t.Fatalf("source checkpoint=%+v", source)
	}
	if source.ChunkDigest == "" {
		t.Fatal("source checkpoint does not reference its durable chunk")
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
		{name: "after checkpoint write", point: faultAfterCheckpointWrite, deliveriesAfterRun: 0, deliveriesAfterRestart: 1},
		{name: "after sink accept", point: faultAfterSinkAccept, deliveriesAfterRun: 1, deliveriesAfterRestart: 2},
		{name: "after sink receipt", point: faultAfterSinkReceipt, deliveriesAfterRun: 1, deliveriesAfterRestart: 1},
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
			manifests, err := filepath.Glob(filepath.Join(options.Out, "manifests", "*.json"))
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
			chunks, err := filepath.Glob(filepath.Join(options.Out, "chunks", "*.ndjson"))
			if err != nil || len(chunks) != 1 {
				t.Fatalf("chunks=%v err=%v", chunks, err)
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
		{name: "after terminal checkpoint write", point: faultAfterTerminalCheckpointWrite, deliveriesAfterRun: 0, deliveriesAfterRestart: 1},
		{name: "after terminal sink accept", point: faultAfterTerminalSinkAccept, deliveriesAfterRun: 1, deliveriesAfterRestart: 2},
		{name: "after terminal sink receipt", point: faultAfterTerminalSinkReceipt, deliveriesAfterRun: 1, deliveriesAfterRestart: 1},
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
			manifests, err := filepath.Glob(filepath.Join(options.Out, "manifests", "*.json"))
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
	raw, err := os.ReadFile(onlyPath(t, filepath.Join(options.Out, "manifests", "*.json")))
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

func TestRestartDeliversOnlySinksMissingReceipts(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	first := &recordingSink{name: "first", config: "v1"}
	second := &recordingSink{name: "second", config: "v1"}
	options := baseOptions(root, history, first)
	options.Sinks = []Sink{first, second}
	options.fault = failOnce(faultAfterSinkReceipt)
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Run(context.Background()); err == nil {
		t.Fatal("receipt fault unexpectedly succeeded")
	}
	if first.deliveries != 1 || second.deliveries != 0 {
		t.Fatalf("deliveries before restart first=%d second=%d", first.deliveries, second.deliveries)
	}
	options.fault = nil
	runCollector(t, options)
	if first.deliveries != 1 || second.deliveries != 1 {
		t.Fatalf("deliveries after restart first=%d second=%d", first.deliveries, second.deliveries)
	}
}

func TestSpoolCanBeReplayedToNewOrReconfiguredSink(t *testing.T) {
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	original := &recordingSink{name: "remote-write-v1", config: "endpoint-a"}
	options := baseOptions(root, history, original)
	runCollector(t, options)

	reconfigured := &recordingSink{name: "remote-write-v1", config: "endpoint-b"}
	added := &recordingSink{name: "typed-adx-v1", config: "table-a"}
	options.Sinks = []Sink{reconfigured, added}
	runCollector(t, options)

	if reconfigured.deliveries != 1 || added.deliveries != 1 {
		t.Fatalf("replayed deliveries reconfigured=%d added=%d", reconfigured.deliveries, added.deliveries)
	}
	if reconfigured.chunks[0].Digest != added.chunks[0].Digest {
		t.Fatalf("sinks consumed different durable chunks: %s != %s", reconfigured.chunks[0].Digest, added.chunks[0].Digest)
	}
}

func TestManifestCanReconstructMissingChunk(t *testing.T) {
	options, sink := completedSpool(t)
	chunkPath := onlyPath(t, filepath.Join(options.Out, "chunks", "*.ndjson"))
	if err := os.Remove(chunkPath); err != nil {
		t.Fatal(err)
	}
	runCollector(t, options)
	if _, err := os.Stat(chunkPath); err != nil {
		t.Fatalf("chunk was not reconstructed: %v", err)
	}
	if sink.deliveries != 1 {
		t.Fatalf("receipt was not reused after reconstruction: deliveries=%d", sink.deliveries)
	}
}

func TestSpoolMetadataAndJSONFramingFailClosed(t *testing.T) {
	t.Run("manifest schema", func(t *testing.T) {
		options, _ := completedSpool(t)
		path := onlyPath(t, filepath.Join(options.Out, "manifests", "*.json"))
		rewriteJSONField(t, path, "schema_version", "unknown")
		expectRunError(t, options, "manifest")
	})
	t.Run("chunk digest", func(t *testing.T) {
		options, _ := completedSpool(t)
		path := onlyPath(t, filepath.Join(options.Out, "chunks", "*.ndjson"))
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("{}\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		expectRunError(t, options, "chunk")
	})
	t.Run("receipt schema", func(t *testing.T) {
		options, _ := completedSpool(t)
		path := onlyPath(t, filepath.Join(options.Out, "receipts", "*", "*", "*.json"))
		rewriteJSONField(t, path, "schema_version", "unknown")
		expectRunError(t, options, "receipt")
	})
	t.Run("receipt config", func(t *testing.T) {
		options, _ := completedSpool(t)
		path := onlyPath(t, filepath.Join(options.Out, "receipts", "*", "*", "*.json"))
		rewriteJSONField(t, path, "config_identity", "other")
		expectRunError(t, options, "receipt")
	})
	t.Run("collector config", func(t *testing.T) {
		options, _ := completedSpool(t)
		options.Project = "other-project"
		expectRunError(t, options, "configuration")
	})
	t.Run("manifest trailing object", func(t *testing.T) {
		options, _ := completedSpool(t)
		appendFile(t, onlyPath(t, filepath.Join(options.Out, "manifests", "*.json")), "{}")
		expectRunError(t, options, "manifest")
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
			source.(map[string]any)["chunk_digest"] = strings.Repeat("0", 64)
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
	t.Run("receipt trailing object", func(t *testing.T) {
		options, _ := completedSpool(t)
		appendFile(t, onlyPath(t, filepath.Join(options.Out, "receipts", "*", "*", "*.json")), "{}")
		expectRunError(t, options, "receipt")
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

func TestRemoteWriteRequestCompatibilityHeadersAndFraming(t *testing.T) {
	event := testEvent(t, "train/loss")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Content-Encoding"); got != "snappy" {
			t.Errorf("Content-Encoding=%q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-protobuf" {
			t.Errorf("Content-Type=%q", got)
		}
		if got := r.Header.Get("X-Prometheus-Remote-Write-Version"); got != "0.1.0" {
			t.Errorf("remote-write version=%q", got)
		}
		compressed, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		raw, err := snappy.Decode(nil, compressed)
		if err != nil {
			t.Error(err)
			return
		}
		number, wireType, n := protowire.ConsumeTag(raw)
		if number != 1 || wireType != protowire.BytesType || n < 0 {
			t.Errorf("invalid WriteRequest tag: %d %v %d", number, wireType, n)
			return
		}
		_, fieldBytes := protowire.ConsumeBytes(raw[n:])
		if fieldBytes < 0 || n+fieldBytes != len(raw) {
			t.Errorf("invalid WriteRequest timeseries: %d", n)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	sink := &RemoteWriteSink{Endpoint: server.URL, MaxAttempts: 1}
	if _, err := sink.Deliver(context.Background(), MetricEventChunk{Events: []exptelemetry.MetricEvent{event}}); err != nil {
		t.Fatal(err)
	}
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
