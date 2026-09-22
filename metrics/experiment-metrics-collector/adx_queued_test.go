// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata"
	kustoerrors "github.com/Azure/azure-kusto-go/azkustodata/errors"
	"github.com/Azure/azure-kusto-go/azkustoingest"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/metricsoffload"
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

type sdkStatusADXResult struct {
	err error
}

type hangingADXClient struct {
	mu       sync.Mutex
	attempts int
}

func (c *hangingADXClient) Ingest(ctx context.Context, _ []byte, _ adxIngestRequest) (adxQueuedResult, error) {
	c.mu.Lock()
	c.attempts++
	c.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

type staticTokenCredential struct{}

func (staticTokenCredential) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	return azcore.AccessToken{Token: "test-token", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func (timeoutADXResult) Wait(ctx context.Context, _ time.Duration) (adxFinalStatus, error) {
	<-ctx.Done()
	return adxStatusFailed, ctx.Err()
}

func (r sdkStatusADXResult) Wait(context.Context, time.Duration) (adxFinalStatus, error) {
	return adxFinalStatusFromSDKError(r.err)
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
	sum := sha256.Sum256(raw)
	return MetricEventChunk{
		SchemaVersion: ChunkSchemaV1,
		Digest:        hex.EncodeToString(sum[:]),
		Sequence:      1,
		Events:        []exptelemetry.MetricEvent{event},
		NDJSON:        raw,
	}
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

func TestSDKADXQueuedClientSendsJSONIngestIfNotExistsArray(t *testing.T) {
	var queueMessage []byte
	baseTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body []byte
		if request.Body != nil {
			var err error
			body, err = io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
		}
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}
		switch {
		case strings.Contains(request.URL.Path, "/v1/rest/auth/metadata"):
			response.Header.Set("Content-Type", "application/json")
			response.Body = io.NopCloser(strings.NewReader(`{"AzureAD":{"LoginEndpoint":"https://login.microsoftonline.com","LoginMfaRequired":false,"KustoClientAppId":"client-id","KustoClientRedirectUri":"https://microsoft/kusto","KustoServiceResourceId":"https://kusto.windows.net","FirstPartyAuthorityUrl":"https://login.microsoftonline.com/tenant"},"dSTS":{"CloudEndpointSuffix":"windows.net","DstsRealm":"realm","DstsInstance":"dsts.core.windows.net","KustoDnsHostName":"kusto.windows.net","ServiceName":"kusto"}}`))
		case strings.Contains(request.URL.Path, "/v1/rest/mgmt"):
			var command struct {
				CSL string `json:"csl"`
			}
			if err := json.Unmarshal(body, &command); err != nil {
				return nil, err
			}
			response.Header.Set("Content-Type", "application/json")
			switch command.CSL {
			case ".get kusto identity token":
				response.Body = io.NopCloser(strings.NewReader(`{"Tables":[{"TableName":"Table_0","Columns":[{"ColumnName":"AuthorizationContext","DataType":"String","ColumnType":"string"}],"Rows":[["auth-context"]]}]}`))
			case ".get ingestion resources":
				response.Body = io.NopCloser(strings.NewReader(`{"Tables":[{"TableName":"Table_0","Columns":[{"ColumnName":"ResourceTypeName","DataType":"String","ColumnType":"string"},{"ColumnName":"StorageRoot","DataType":"String","ColumnType":"string"}],"Rows":[["TempStorage","https://storage.blob.core.windows.net/container?sv=2024-01-01&sig=blob-secret"],["SecuredReadyForAggregationQueue","https://storage.queue.core.windows.net/queue?sv=2024-01-01&sig=queue-secret"],["IngestionsStatusTable","https://storage.table.core.windows.net/status?sv=2024-01-01&sig=table-secret"]]}]}`))
			default:
				return nil, fmt.Errorf("unexpected management command %q", command.CSL)
			}
		case request.URL.Host == "storage.blob.core.windows.net":
			response.StatusCode = http.StatusCreated
			response.Header.Set("ETag", `"test-etag"`)
		case request.URL.Host == "storage.table.core.windows.net":
			response.StatusCode = http.StatusNoContent
		case request.URL.Host == "storage.queue.core.windows.net":
			response.StatusCode = http.StatusCreated
			queueMessage = append([]byte(nil), body...)
			response.Header.Set("Content-Type", "application/xml")
			response.Body = io.NopCloser(strings.NewReader(`<QueueMessagesList><QueueMessage><MessageId>message-id</MessageId><InsertionTime>Mon, 21 Sep 2026 00:00:00 GMT</InsertionTime><ExpirationTime>Tue, 22 Sep 2026 00:00:00 GMT</ExpirationTime><PopReceipt>receipt</PopReceipt><TimeNextVisible>Mon, 21 Sep 2026 00:00:00 GMT</TimeNextVisible></QueueMessage></QueueMessagesList>`))
		default:
			return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL)
		}
		return response, nil
	})
	httpClient := &http.Client{Transport: &adxResourceCachingTransport{base: baseTransport}}
	kcsb := azkustodata.NewConnectionStringBuilder("https://cluster.kusto.windows.net").
		WithTokenCredential(staticTokenCredential{})
	client, err := newSDKADXQueuedClient(
		kcsb,
		ADXQueuedConfig{Database: "metrics", Table: "MetricEvents"},
		httpClient,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.discovery.Close()
	})

	ingestByValue := "taugrid-metric-chunk-" + strings.Repeat("a", 64)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := client.Ingest(ctx, []byte("{}\n"), adxIngestRequest{
		Database: "metrics", Table: "MetricEvents", Mapping: "MetricEventChunkNDJSON",
		IngestByValue: ingestByValue,
	}); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		MessageText string `xml:"MessageText"`
	}
	if err := xml.Unmarshal(queueMessage, &envelope); err != nil {
		t.Fatal(err)
	}
	message, err := base64.StdEncoding.DecodeString(envelope.MessageText)
	if err != nil {
		t.Fatal(err)
	}
	var properties struct {
		AdditionalProperties struct {
			IngestIfNotExists string `json:"ingestIfNotExists"`
		} `json:"AdditionalProperties"`
	}
	if err := json.Unmarshal(message, &properties); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal([]string{ingestByValue})
	if err != nil {
		t.Fatal(err)
	}
	if properties.AdditionalProperties.IngestIfNotExists != string(want) {
		t.Fatalf(
			"queued ingestIfNotExists=%q, want JSON array %q",
			properties.AdditionalProperties.IngestIfNotExists,
			want,
		)
	}
}

func TestSDKADXQueuedDiscoveryUsesIngestionEndpoint(t *testing.T) {
	var discoveryHost string
	baseTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}
		if strings.Contains(request.URL.Path, "/v1/rest/auth/metadata") {
			response.Header.Set("Content-Type", "application/json")
			response.Body = io.NopCloser(strings.NewReader(`{"AzureAD":{"LoginEndpoint":"https://login.microsoftonline.com","LoginMfaRequired":false,"KustoClientAppId":"client-id","KustoClientRedirectUri":"https://microsoft/kusto","KustoServiceResourceId":"https://kusto.windows.net","FirstPartyAuthorityUrl":"https://login.microsoftonline.com/tenant"},"dSTS":{"CloudEndpointSuffix":"windows.net","DstsRealm":"realm","DstsInstance":"dsts.core.windows.net","KustoDnsHostName":"kusto.windows.net","ServiceName":"kusto"}}`))
			return response, nil
		}
		if request.URL.Path == "/v1/rest/mgmt" {
			if request.URL.Host != "ingest-cluster.kusto.windows.net" {
				return nil, fmt.Errorf("management request used wrong host %q", request.URL.Host)
			}
			if !isADXIngestionResourcesRequest(request) {
				return nil, fmt.Errorf("unexpected ingestion management request")
			}
			discoveryHost = request.URL.Host
			response.Header.Set("Content-Type", "application/json")
			response.Body = io.NopCloser(strings.NewReader(`{"Tables":[{"TableName":"Table_0","Columns":[{"ColumnName":"ResourceTypeName","DataType":"String","ColumnType":"string"},{"ColumnName":"StorageRoot","DataType":"String","ColumnType":"string"}],"Rows":[]}]}`))
			return response, nil
		}
		return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL)
	})
	httpClient := &http.Client{Transport: &adxResourceCachingTransport{base: baseTransport}}
	kcsb := azkustodata.NewConnectionStringBuilder("https://cluster.kusto.windows.net").
		WithTokenCredential(staticTokenCredential{})
	client, err := newSDKADXQueuedClient(
		kcsb,
		ADXQueuedConfig{Database: "metrics", Table: "MetricEvents"},
		httpClient,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.discovery.Close(); err != nil {
			t.Error(err)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err = client.Ingest(ctx, []byte("{}\n"), adxIngestRequest{
		Database: "metrics", Table: "MetricEvents", Mapping: "MetricEventChunkNDJSON",
		IngestByValue: "taugrid-metric-chunk-" + strings.Repeat("c", 64),
	})
	if err == nil || discoveryHost != "ingest-cluster.kusto.windows.net" {
		t.Fatalf("discovery host=%q err=%v", discoveryHost, err)
	}
}

func TestSDKADXQueuedClientRefreshesSignedIngestionResources(t *testing.T) {
	var mu sync.Mutex
	resourceGeneration := 0
	var blobGenerations, queueGenerations []string
	baseTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		var body []byte
		if request.Body != nil {
			var err error
			body, err = io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
		}
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}
		switch {
		case strings.Contains(request.URL.Path, "/v1/rest/auth/metadata"):
			response.Header.Set("Content-Type", "application/json")
			response.Body = io.NopCloser(strings.NewReader(`{"AzureAD":{"LoginEndpoint":"https://login.microsoftonline.com","LoginMfaRequired":false,"KustoClientAppId":"client-id","KustoClientRedirectUri":"https://microsoft/kusto","KustoServiceResourceId":"https://kusto.windows.net","FirstPartyAuthorityUrl":"https://login.microsoftonline.com/tenant"},"dSTS":{"CloudEndpointSuffix":"windows.net","DstsRealm":"realm","DstsInstance":"dsts.core.windows.net","KustoDnsHostName":"kusto.windows.net","ServiceName":"kusto"}}`))
		case strings.Contains(request.URL.Path, "/v1/rest/mgmt"):
			var command struct {
				CSL string `json:"csl"`
			}
			if err := json.Unmarshal(body, &command); err != nil {
				return nil, err
			}
			response.Header.Set("Content-Type", "application/json")
			switch command.CSL {
			case ".get kusto identity token":
				response.Body = io.NopCloser(strings.NewReader(`{"Tables":[{"TableName":"Table_0","Columns":[{"ColumnName":"AuthorizationContext","DataType":"String","ColumnType":"string"}],"Rows":[["auth-context"]]}]}`))
			case ".get ingestion resources":
				mu.Lock()
				resourceGeneration++
				generation := resourceGeneration
				mu.Unlock()
				response.Body = io.NopCloser(strings.NewReader(fmt.Sprintf(
					`{"Tables":[{"TableName":"Table_0","Columns":[{"ColumnName":"ResourceTypeName","DataType":"String","ColumnType":"string"},{"ColumnName":"StorageRoot","DataType":"String","ColumnType":"string"}],"Rows":[["TempStorage","https://storage.blob.core.windows.net/container?sv=2024-01-01&sig=blob-%[1]d&generation=%[1]d"],["SecuredReadyForAggregationQueue","https://storage.queue.core.windows.net/queue?sv=2024-01-01&sig=queue-%[1]d&generation=%[1]d"],["IngestionsStatusTable","https://storage.table.core.windows.net/status?sv=2024-01-01&sig=table-%[1]d&generation=%[1]d"]]}]}`,
					generation,
				)))
			default:
				return nil, fmt.Errorf("unexpected management command %q", command.CSL)
			}
		case request.URL.Host == "storage.blob.core.windows.net":
			mu.Lock()
			blobGenerations = append(blobGenerations, request.URL.Query().Get("generation"))
			mu.Unlock()
			response.StatusCode = http.StatusCreated
			response.Header.Set("ETag", `"test-etag"`)
		case request.URL.Host == "storage.table.core.windows.net":
			response.StatusCode = http.StatusNoContent
		case request.URL.Host == "storage.queue.core.windows.net":
			mu.Lock()
			queueGenerations = append(queueGenerations, request.URL.Query().Get("generation"))
			mu.Unlock()
			response.StatusCode = http.StatusCreated
			response.Header.Set("Content-Type", "application/xml")
			response.Body = io.NopCloser(strings.NewReader(`<QueueMessagesList><QueueMessage><MessageId>message-id</MessageId><InsertionTime>Mon, 21 Sep 2026 00:00:00 GMT</InsertionTime><ExpirationTime>Tue, 22 Sep 2026 00:00:00 GMT</ExpirationTime><PopReceipt>receipt</PopReceipt><TimeNextVisible>Mon, 21 Sep 2026 00:00:00 GMT</TimeNextVisible></QueueMessage></QueueMessagesList>`))
		default:
			return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL)
		}
		return response, nil
	})
	httpClient := &http.Client{Transport: &adxResourceCachingTransport{base: baseTransport}}
	kcsb := azkustodata.NewConnectionStringBuilder("https://cluster.kusto.windows.net").
		WithTokenCredential(staticTokenCredential{})
	client, err := newSDKADXQueuedClient(
		kcsb,
		ADXQueuedConfig{Database: "metrics", Table: "MetricEvents"},
		httpClient,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := client.discovery.Close(); err != nil {
			t.Error(err)
		}
	})

	for generation := 1; generation <= 2; generation++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := client.Ingest(ctx, []byte("{}\n"), adxIngestRequest{
			Database: "metrics", Table: "MetricEvents", Mapping: "MetricEventChunkNDJSON",
			IngestByValue: fmt.Sprintf("taugrid-metric-chunk-%064d", generation),
		})
		cancel()
		if err != nil {
			t.Fatalf("generation %d ingest: %v", generation, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if resourceGeneration != 2 {
		t.Fatalf("bounded resource discoveries=%d, want 2", resourceGeneration)
	}
	if strings.Join(blobGenerations, ",") != "1,2" {
		t.Fatalf("blob resource generations=%v, want refreshed 1,2", blobGenerations)
	}
	if strings.Join(queueGenerations, ",") != "1,2" {
		t.Fatalf("queue resource generations=%v, want refreshed 1,2", queueGenerations)
	}
}

func TestSDKADXQueuedColdResourceDiscoveryHonorsAttemptDeadline(t *testing.T) {
	discoveryStarted := make(chan struct{})
	var once sync.Once
	baseTransport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		response := &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("")),
			Request:    request,
		}
		if strings.Contains(request.URL.Path, "/v1/rest/auth/metadata") {
			response.Header.Set("Content-Type", "application/json")
			response.Body = io.NopCloser(strings.NewReader(`{"AzureAD":{"LoginEndpoint":"https://login.microsoftonline.com","LoginMfaRequired":false,"KustoClientAppId":"client-id","KustoClientRedirectUri":"https://microsoft/kusto","KustoServiceResourceId":"https://kusto.windows.net","FirstPartyAuthorityUrl":"https://login.microsoftonline.com/tenant"},"dSTS":{"CloudEndpointSuffix":"windows.net","DstsRealm":"realm","DstsInstance":"dsts.core.windows.net","KustoDnsHostName":"kusto.windows.net","ServiceName":"kusto"}}`))
			return response, nil
		}
		if isADXIngestionResourcesRequest(request) {
			once.Do(func() { close(discoveryStarted) })
			<-request.Context().Done()
			return nil, request.Context().Err()
		}
		return nil, fmt.Errorf("unexpected request %s %s", request.Method, request.URL)
	})
	httpClient := &http.Client{Transport: &adxResourceCachingTransport{base: baseTransport}}
	kcsb := azkustodata.NewConnectionStringBuilder("https://cluster.kusto.windows.net").
		WithTokenCredential(staticTokenCredential{})
	client, err := newSDKADXQueuedClient(
		kcsb,
		ADXQueuedConfig{Database: "metrics", Table: "MetricEvents"},
		httpClient,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		client.discovery.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err = client.Ingest(ctx, []byte("{}\n"), adxIngestRequest{
		Database: "metrics", Table: "MetricEvents", Mapping: "MetricEventChunkNDJSON",
		IngestByValue: "taugrid-metric-chunk-" + strings.Repeat("b", 64),
	})
	if err == nil {
		t.Fatal("cold-cache resource discovery unexpectedly succeeded")
	}
	select {
	case <-discoveryStarted:
	default:
		t.Fatal("real SDK did not attempt cold-cache ingestion resource discovery")
	}
	if elapsed := time.Since(start); elapsed < 900*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("cold-cache discovery elapsed %s, want the configured 1s deadline", elapsed)
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

func TestADXQueuedSubmissionTimeoutIsBoundedAndRetried(t *testing.T) {
	client := &hangingADXClient{}
	sink := adxTestSink(client)
	sink.Config.MaxAttempts = 2
	sink.Config.FinalStatusTimeout = time.Millisecond

	start := time.Now()
	if _, err := sink.Deliver(context.Background(), adxTestChunk(t)); err == nil {
		t.Fatal("hung ADX submission unexpectedly succeeded")
	}
	client.mu.Lock()
	attempts := client.attempts
	client.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("submission attempts=%d, want 2", attempts)
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("submission retries exceeded delivery budget: %s", elapsed)
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

func TestADXQueuedConfigIdentity(t *testing.T) {
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

}

func TestADXQueuedTerminalDrainTimeoutUsesSharedContract(t *testing.T) {
	sink := adxTestSink(&fakeADXClient{})
	sink.Config.MaxAttempts = 2
	sink.Config.RetryBackoff = 3 * time.Second
	sink.Config.FinalStatusTimeout = 4 * time.Second
	got, err := sink.TerminalDrainTimeout()
	if err != nil {
		t.Fatal(err)
	}
	want, err := metricsoffload.TerminalDrainTimeout(2, 3*time.Second, 4*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("terminal drain timeout=%s, want shared contract %s", got, want)
	}
}

func TestADXQueuedMissingReceiptReplayAcceptsSDKSkipped(t *testing.T) {
	skipped := azkustoingest.StatusFromMapForTests(map[string]interface{}{
		"Status":        "Skipped",
		"FailureStatus": "Permanent",
		"ErrorCode":     "IngestByTagAlreadyExists",
	})
	client := &fakeADXClient{results: []adxQueuedResult{
		&fakeADXResult{status: adxStatusSucceeded},
		sdkStatusADXResult{err: skipped},
	}}
	sink := adxTestSink(client)
	root := t.TempDir()
	history := filepath.Join(root, "history.jsonl")
	writeFile(t, history, historyRow)
	options := baseOptions(root, history, sink)
	crash := errors.New("simulated crash before receipt persistence")
	options.fault = func(point faultPoint) error {
		if point == faultAfterSinkAccept {
			return crash
		}
		return nil
	}
	runner, err := New(options)
	if err != nil {
		t.Fatal(err)
	}
	_, err = runner.Run(context.Background())
	if !errors.Is(err, crash) {
		t.Fatalf("initial missing-receipt delivery error=%v, want %v", err, crash)
	}
	pending, err := filepath.Glob(filepath.Join(options.Out, "pending", "*.json"))
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending ADX delivery=%v err=%v, want one durable record", pending, err)
	}
	options.fault = nil
	runCollector(t, options)
	runCollector(t, options)
	if len(client.requests) != 2 {
		t.Fatalf("missing-receipt replay submitted %d requests, want 2", len(client.requests))
	}
	pending, err = filepath.Glob(filepath.Join(options.Out, "pending", "*.json"))
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending ADX delivery after replay=%v err=%v", pending, err)
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
	management503 := kustoerrors.HTTP(
		kustoerrors.OpMgmt,
		"503 Service Unavailable",
		http.StatusServiceUnavailable,
		io.NopCloser(strings.NewReader(`{"error":{"code":"ServiceUnavailable","@permanent":false}}`)),
		"management request failed",
	)
	flattenedManagement503 := fmt.Errorf(
		"problem getting authorization context from Kusto via Mgmt: %s",
		management503,
	)
	if !adxRetryable(normalizeADXSDKError(flattenedManagement503)) {
		t.Fatal("flattened Kusto management 503 was not normalized as retryable")
	}
	management400 := kustoerrors.HTTP(
		kustoerrors.OpMgmt,
		"400 Bad Request",
		http.StatusBadRequest,
		io.NopCloser(strings.NewReader(`{"error":{"code":"BadRequest","@permanent":true}}`)),
		"management request failed",
	)
	flattenedManagement400 := fmt.Errorf(
		"problem getting ingestion resources from Kusto: %s",
		management400,
	)
	if adxRetryable(normalizeADXSDKError(flattenedManagement400)) {
		t.Fatal("flattened permanent Kusto management 400 was retryable")
	}
	exhaustedStorage := kustoerrors.ES(
		kustoerrors.OpFileIngest,
		kustoerrors.KBlobstore,
		"could not upload file to any queue",
	)
	if !adxRetryable(normalizeADXSDKError(exhaustedStorage)) {
		t.Fatal("exhausted SDK storage transport was not normalized as retryable")
	}
	permanentClientError := kustoerrors.ES(
		kustoerrors.OpFileIngest,
		kustoerrors.KClientArgs,
		"invalid ingestion arguments",
	).SetNoRetry()
	if adxRetryable(normalizeADXSDKError(permanentClientError)) {
		t.Fatal("permanent SDK client error was retryable")
	}
}

func TestADXDiagnosticsDoNotExposeSASCredentials(t *testing.T) {
	const secret = "super-secret-signature"
	status := azkustoingest.StatusFromMapForTests(map[string]interface{}{
		"Status":        "Failed",
		"FailureStatus": "Permanent",
		"ErrorCode":     "BadRequest",
		"Details":       "source https://storage.blob.core.windows.net/container/blob?sv=2024-01-01&sp=r&sig=" + secret,
	})
	client := &fakeADXClient{results: []adxQueuedResult{
		&fakeADXResult{status: adxStatusFailed, err: status},
	}}
	sink := adxTestSink(client)
	_, err := sink.Deliver(context.Background(), adxTestChunk(t))
	if err == nil {
		t.Fatal("SAS-bearing ADX failure unexpectedly succeeded")
	}
	for _, forbidden := range []string{secret, "sig=", "sv=2024-01-01", "storage.blob.core.windows.net"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("diagnostic leaked %q: %s", forbidden, err)
		}
	}
	if !strings.Contains(err.Error(), "status=Failed") ||
		!strings.Contains(err.Error(), "failure=Permanent") ||
		!strings.Contains(err.Error(), "code=BadRequest") {
		t.Fatalf("diagnostic omitted allowlisted status fields: %s", err)
	}

	redacted := redactADXDiagnosticText(
		"request https://storage.queue.core.windows.net/q?sv=2024-01-01&sig=" + secret + " failed; sig=" + secret,
	)
	if strings.Contains(redacted, secret) || strings.Contains(redacted, "sv=2024-01-01") {
		t.Fatalf("redaction retained SAS credentials: %s", redacted)
	}
}

func TestADXQueuedRejectsUnboundedAttempts(t *testing.T) {
	sink := adxTestSink(&fakeADXClient{})
	sink.Config.MaxAttempts = metricsoffload.MaxADXAttempts + 1
	if err := sink.validate(); err == nil || !strings.Contains(err.Error(), "adx_max_attempts") {
		t.Fatalf("validate max attempts error = %v", err)
	}
}
