// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-kusto-go/azkustoingest"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/taugrid/core/exptelemetry"
)

type fakeADXClient struct {
	mu       sync.Mutex
	results  []adxQueuedResult
	errors   []error
	payloads [][]byte
	requests []adxIngestRequest
}

func (c *fakeADXClient) Ingest(_ context.Context, payload []byte, request adxIngestRequest) (adxQueuedResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.payloads = append(c.payloads, append([]byte(nil), payload...))
	c.requests = append(c.requests, request)
	index := len(c.requests) - 1
	if index < len(c.errors) && c.errors[index] != nil {
		return nil, c.errors[index]
	}
	if index >= len(c.results) {
		return nil, errors.New("unexpected ADX ingestion")
	}
	return c.results[index], nil
}

type fakeADXResult struct {
	status adxFinalStatus
	err    error
	waited bool
}

type timeoutADXResult struct{}

func (timeoutADXResult) Wait(ctx context.Context, _ time.Duration) (adxFinalStatus, error) {
	<-ctx.Done()
	return adxStatusFailed, ctx.Err()
}

func (r *fakeADXResult) Wait(context.Context, time.Duration) (adxFinalStatus, error) {
	r.waited = true
	return r.status, r.err
}

func adxTestSink(client adxQueuedClient) *ADXQueuedSink {
	return &ADXQueuedSink{
		Config: ADXQueuedConfig{
			ClusterURI: "https://cluster.kusto.windows.net", Database: "metrics",
			Table: "MetricEvents", IngestionMapping: "MetricEventChunkNDJSON",
			MaxAttempts: 1, RetryBackoff: time.Millisecond,
			FinalStatusTimeout: time.Second, StatusPollInterval: time.Millisecond,
		},
		client: client,
	}
}

func adxTestChunk(t *testing.T) MetricEventChunk {
	t.Helper()
	event := testEvent(t, "train/loss")
	raw, err := event.MarshalNDJSON()
	if err != nil {
		t.Fatal(err)
	}
	chunk, err := writeChunk(t.TempDir(), MetricEventChunk{
		Sequence: 1, Events: []exptelemetry.MetricEvent{event}, NDJSON: raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	return chunk
}

func TestADXQueuedWaitsForFinalSuccessBeforeAcknowledging(t *testing.T) {
	final := &fakeADXResult{status: adxStatusSucceeded}
	client := &fakeADXClient{results: []adxQueuedResult{final}}
	sink := adxTestSink(client)
	chunk := adxTestChunk(t)

	ack, err := sink.Deliver(context.Background(), chunk)
	if err != nil {
		t.Fatal(err)
	}
	if !final.waited {
		t.Fatal("delivery acknowledged before waiting for final ingestion status")
	}
	if ack.Samples != 1 || ack.Requests != 1 || ack.Retries != 0 ||
		ack.Metadata["final_status"] != "Succeeded" {
		t.Fatalf("ack=%+v", ack)
	}
	if string(client.payloads[0]) != string(chunk.NDJSON) {
		t.Fatal("ADX did not receive the canonical chunk NDJSON directly")
	}
	request := client.requests[0]
	wantValue := "taugrid-metric-chunk-" + chunk.Digest
	wantTag := "ingest-by:" + wantValue
	if request.IngestByValue != wantValue || ack.Metadata["ingest_by_tag"] != wantTag ||
		request.Mapping != "MetricEventChunkNDJSON" {
		t.Fatalf("request=%+v ack=%+v", request, ack)
	}
}

func TestADXQueuedStatusIsNotFinalAcknowledgment(t *testing.T) {
	client := &fakeADXClient{results: []adxQueuedResult{
		&fakeADXResult{status: adxStatusQueued},
	}}
	sink := adxTestSink(client)
	if ack, err := sink.Deliver(context.Background(), adxTestChunk(t)); err == nil || ack.Samples != 0 {
		t.Fatalf("queued ingestion acknowledged: ack=%+v err=%v", ack, err)
	}
}

func TestADXQueuedRetriesTransientFailure(t *testing.T) {
	client := &fakeADXClient{
		errors: []error{adxTransientError{err: errors.New("status service unavailable")}, nil},
		results: []adxQueuedResult{
			nil,
			&fakeADXResult{status: adxStatusSucceeded},
		},
	}

	sink := adxTestSink(client)
	sink.Config.MaxAttempts = 2
	ack, err := sink.Deliver(context.Background(), adxTestChunk(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(client.requests) != 2 || ack.Requests != 2 || ack.Retries != 1 {
		t.Fatalf("requests=%d ack=%+v", len(client.requests), ack)
	}
	if client.requests[0].IngestByValue != client.requests[1].IngestByValue {
		t.Fatal("retry changed the digest-derived idempotency tag")
	}
}

func TestADXQueuedFinalStatusTimeoutIsBoundedAndRetried(t *testing.T) {
	client := &fakeADXClient{results: []adxQueuedResult{timeoutADXResult{}, timeoutADXResult{}}}
	sink := adxTestSink(client)
	sink.Config.MaxAttempts = 2
	sink.Config.FinalStatusTimeout = time.Millisecond

	start := time.Now()
	if _, err := sink.Deliver(context.Background(), adxTestChunk(t)); err == nil {
		t.Fatal("final status timeout unexpectedly succeeded")
	}
	if len(client.requests) != 2 {
		t.Fatalf("timeout attempts=%d, want 2", len(client.requests))
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("final status retries were not bounded: %s", elapsed)
	}
}

func TestADXQueuedPermanentFailuresDoNotRetryAndErrorsAreBounded(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "authentication", err: &azcore.ResponseError{StatusCode: 401, ErrorCode: "Unauthorized"}},
		{name: "schema", err: errors.New("schema mismatch " + strings.Repeat("x", 2048))},
		{name: "mapping", err: errors.New("ingestion mapping does not exist")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakeADXClient{errors: []error{tt.err}}
			sink := adxTestSink(client)
			sink.Config.MaxAttempts = 3
			_, err := sink.Deliver(context.Background(), adxTestChunk(t))
			if err == nil {
				t.Fatal("permanent failure unexpectedly succeeded")
			}
			if len(client.requests) != 1 {
				t.Fatalf("permanent failure attempts=%d, want 1", len(client.requests))
			}
			if len(err.Error()) > 1100 {
				t.Fatalf("error was not bounded: length=%d", len(err.Error()))
			}
		})
	}
}

func TestADXQueuedConfigIdentityAndReceiptReplay(t *testing.T) {
	client := &fakeADXClient{results: []adxQueuedResult{
		&fakeADXResult{status: adxStatusSucceeded},
	}}
	sink := adxTestSink(client)
	same := adxTestSink(&fakeADXClient{})
	if sink.ConfigIdentity() != same.ConfigIdentity() {
		t.Fatal("identical ADX configuration produced unstable identity")
	}
	changed := adxTestSink(&fakeADXClient{})
	changed.Config.Table = "OtherTable"
	if sink.ConfigIdentity() == changed.ConfigIdentity() {
		t.Fatal("destination change did not change ADX configuration identity")
	}
	changed = adxTestSink(&fakeADXClient{})
	changed.Config.ClientID = "00000000-0000-0000-0000-000000000001"
	if sink.ConfigIdentity() == changed.ConfigIdentity() {
		t.Fatal("identity client ID change did not change ADX configuration identity")
	}

	root := t.TempDir()
	chunk := adxTestChunk(t)
	if _, reused, err := deliverWithReceipt(context.Background(), root, sink, chunk); err != nil || reused {
		t.Fatalf("initial delivery reused=%v err=%v", reused, err)
	}
	if _, reused, err := deliverWithReceipt(context.Background(), root, sink, chunk); err != nil || !reused {
		t.Fatalf("replay delivery reused=%v err=%v", reused, err)
	}
	if len(client.requests) != 1 {
		t.Fatalf("receipt replay submitted %d ADX requests, want 1", len(client.requests))
	}
}

func TestADXRetryClassification(t *testing.T) {
	if !adxRetryable(adxTransientError{err: errors.New("temporary")}) {
		t.Fatal("explicit transient error was not retryable")
	}
	if adxRetryable(fmt.Errorf("invalid ingestion mapping")) {
		t.Fatal("mapping error was retryable")
	}
	if adxRetryable(context.Canceled) {
		t.Fatal("context cancellation was retryable")
	}
	if !adxRetryable(&azcore.ResponseError{StatusCode: 409, ErrorCode: "Conflict"}) {
		t.Fatal("transient schema conflict was not retryable")
	}
	transientStatus := azkustoingest.StatusFromMapForTests(map[string]interface{}{
		"Status": "Failed", "FailureStatus": "Transient", "ErrorCode": "ServiceUnavailable",
	})
	if !adxRetryable(transientStatus) {
		t.Fatal("SDK transient status was not retryable")
	}
	if !adxRetryable(fmt.Errorf("ADX final ingestion status: %w", transientStatus)) {
		t.Fatal("wrapped SDK transient status was not retryable")
	}
	permanentStatus := azkustoingest.StatusFromMapForTests(map[string]interface{}{
		"Status": "Failed", "FailureStatus": "Permanent", "ErrorCode": "BadRequest_MappingReferenceWasNotFound",
	})
	if adxRetryable(permanentStatus) {
		t.Fatal("SDK permanent status was retryable")
	}
}

func TestADXQueuedRejectsUnboundedAttempts(t *testing.T) {
	sink := adxTestSink(&fakeADXClient{})
	sink.Config.MaxAttempts = MaxADXQueuedAttempts + 1
	if err := sink.validate(); err == nil || !strings.Contains(err.Error(), "max attempts") {
		t.Fatalf("validate max attempts error = %v", err)
	}
}
