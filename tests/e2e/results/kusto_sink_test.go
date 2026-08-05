package results

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-kusto-go/azkustoingest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockIngestor records payloads and options sent via FromReader and can return a configured error.
type mockIngestor struct {
	mu       sync.Mutex
	payloads []string   // captured NDJSON payloads
	options  [][]string // captured option strings per call
	err      error
	closed   bool
}

func (m *mockIngestor) FromReader(_ context.Context, reader io.Reader, opts ...azkustoingest.FileOption) (*azkustoingest.Result, error) {
	body, readErr := io.ReadAll(reader)
	var optStrs []string
	for _, o := range opts {
		optStrs = append(optStrs, o.String())
	}
	m.mu.Lock()
	if readErr != nil {
		m.payloads = append(m.payloads, fmt.Sprintf("READ_ERROR: %v", readErr))
	} else {
		m.payloads = append(m.payloads, string(body))
	}
	m.options = append(m.options, optStrs)
	m.mu.Unlock()
	return nil, m.err
}

func (m *mockIngestor) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	return nil
}

func sampleOutcome(name string, runID int64) Outcome {
	return Outcome{
		RunID:       runID,
		RunAttempt:  1,
		TestName:    name,
		Suite:       "kueue",
		Status:      StatusPass,
		DurationSec: 1.23,
		Timestamp:   time.Date(2026, 4, 29, 12, 0, 0, 0, time.UTC),
		Branch:      "main",
	}
}

func TestKustoSink_FlushIngestsViaManaged(t *testing.T) {
	mock := &mockIngestor{}
	sink := newKustoSinkForTest(mock)

	ctx := context.Background()
	require.NoError(t, sink.Record(ctx, sampleOutcome("TestA", 100)))
	require.NoError(t, sink.Record(ctx, sampleOutcome("TestB", 100)))
	require.NoError(t, sink.Record(ctx, sampleOutcome("TestC", 100)))

	require.NoError(t, sink.Flush())

	require.Len(t, mock.payloads, 1, "expected exactly 1 FromReader call")

	payload := mock.payloads[0]
	assert.Contains(t, payload, "TestA")
	assert.Contains(t, payload, "TestB")
	assert.Contains(t, payload, "TestC")

	// Verify NDJSON structure: 3 JSON objects separated by newlines.
	lines := strings.Split(strings.TrimSpace(payload), "\n")
	assert.Len(t, lines, 3, "expected 3 NDJSON lines in payload")

	// Verify ingestion options: FileFormat(MultiJSON) + IngestionMappingRef.
	require.Len(t, mock.options, 1, "expected options from 1 FromReader call")
	opts := mock.options[0]
	assert.Contains(t, opts, "FileFormat", "should pass FileFormat option")
	assert.Contains(t, opts, "IngestionMappingRef", "should pass IngestionMappingRef option")

	assert.True(t, mock.closed, "ingestor should be closed after Flush")
}

func TestKustoSink_FlushEmptyBuffer(t *testing.T) {
	mock := &mockIngestor{}
	sink := newKustoSinkForTest(mock)

	require.NoError(t, sink.Flush())

	assert.Empty(t, mock.payloads, "no FromReader call for empty buffer")
	assert.True(t, mock.closed, "ingestor should still be closed")
}

func TestKustoSink_FlushSwallowsError(t *testing.T) {
	mock := &mockIngestor{err: fmt.Errorf("ingestion timeout")}
	sink := newKustoSinkForTest(mock)

	ctx := context.Background()
	require.NoError(t, sink.Record(ctx, sampleOutcome("TestFail", 100)))

	// Capture stderr.
	old := os.Stderr
	r, w, _ := os.Pipe()
	os.Stderr = w

	err := sink.Flush()

	w.Close()
	os.Stderr = old

	var buf bytes.Buffer
	io.Copy(&buf, r)

	assert.NoError(t, err, "Flush must return nil even on ingestion error")
	assert.Contains(t, buf.String(), "ingestion timeout", "error should be logged to stderr")
}

func TestKustoSink_SkipsLocalRuns(t *testing.T) {
	mock := &mockIngestor{}
	sink := newKustoSinkForTest(mock)

	ctx := context.Background()
	// RunID == 0 means local run — should be silently skipped.
	require.NoError(t, sink.Record(ctx, sampleOutcome("TestLocal", 0)))
	require.NoError(t, sink.Record(ctx, sampleOutcome("TestCI", 42)))

	require.NoError(t, sink.Flush())

	require.Len(t, mock.payloads, 1)
	payload := mock.payloads[0]
	assert.Contains(t, payload, "TestCI")
	assert.NotContains(t, payload, "TestLocal")
}

func TestInit_NoKustoWhenEnvUnset(t *testing.T) {
	// Reset global state for this test.
	globalOnce = sync.Once{}
	globalEmitter = nil
	globalErr = nil

	t.Setenv("E2E_KUSTO_URI", "")
	t.Setenv("E2E_RESULT_FILE", t.TempDir()+"/test-results.jsonl")

	emitter, err := Init()
	require.NoError(t, err)
	require.NotNil(t, emitter)

	multi, ok := emitter.(*Multi)
	require.True(t, ok, "expected *Multi emitter")
	assert.Len(t, multi.Emitters, 1, "only JSONL sink when E2E_KUSTO_URI is unset")

	// Cleanup: reset globals.
	t.Cleanup(func() {
		globalOnce = sync.Once{}
		globalEmitter = nil
		globalErr = nil
	})
}
