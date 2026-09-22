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
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata"
	kustoerrors "github.com/Azure/azure-kusto-go/azkustodata/errors"
	"github.com/Azure/azure-kusto-go/azkustodata/kql"
	"github.com/Azure/azure-kusto-go/azkustoingest"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/taugrid/core/metricsoffload"
)

const adxQueuedSinkName = "adx-queued-v1"

type ADXQueuedConfig struct {
	ClusterURI         string
	Database           string
	Table              string
	IngestionMapping   string
	ClientID           string
	MaxAttempts        int
	RetryBackoff       time.Duration
	FinalStatusTimeout time.Duration
	StatusPollInterval time.Duration
}

// ADXQueuedSink durably delivers canonical MetricEvent NDJSON through ADX
// queued ingestion and acknowledges only terminal successful ingestion.
type ADXQueuedSink struct {
	Config     ADXQueuedConfig
	Credential azcore.TokenCredential
	client     adxQueuedClient
}

type adxFinalStatus string

const (
	adxStatusSucceeded adxFinalStatus = "Succeeded"
	adxStatusSkipped   adxFinalStatus = "Skipped"
	adxStatusQueued    adxFinalStatus = "Queued"
	adxStatusFailed    adxFinalStatus = "Failed"
)

type adxQueuedClient interface {
	Ingest(context.Context, []byte, adxIngestRequest) (adxQueuedResult, error)
}

type adxQueuedResult interface {
	Wait(context.Context, time.Duration) (adxFinalStatus, error)
}

type adxIngestRequest struct {
	Database      string
	Table         string
	Mapping       string
	IngestByValue string
}

type adxTransientError struct {
	err error
}

func (e adxTransientError) Error() string { return e.err.Error() }
func (e adxTransientError) Unwrap() error { return e.err }

var (
	adxURLPattern             = regexp.MustCompile(`https?://[^\s"'<>]+`)
	adxSASQueryPattern        = regexp.MustCompile(`(?i)([?&]?(?:sig|se|sp|spr|st|srt|ss|skoid|sktid|skt|ske|sks|skv)=)[^&\s]+`)
	adxFlattenedHTTPStatus    = regexp.MustCompile(`Kind\(KHTTPError\): [^(]*\(([45][0-9]{2})(?: [^)]*)?\):`)
	adxFlattenedKustoPrefixes = []string{
		"problem getting authorization context from Kusto via Mgmt: ",
		"problem getting ingestion resources from Kusto: ",
	}
)

type sdkADXQueuedClient struct {
	discovery  *azkustodata.Client
	kcsb       *azkustodata.ConnectionStringBuilder
	config     ADXQueuedConfig
	httpClient *http.Client
}

type sdkADXQueuedResult struct {
	result *azkustoingest.Result
}

type adxCachedHTTPResponse struct {
	statusCode int
	header     http.Header
	body       []byte
}

type adxResourceCachingTransport struct {
	base      http.RoundTripper
	mu        sync.RWMutex
	resources *adxCachedHTTPResponse
}

type adxResourceDiscoveryContextKey struct{}

func NewADXQueuedSink(config ADXQueuedConfig, credential azcore.TokenCredential) (*ADXQueuedSink, error) {
	sink := &ADXQueuedSink{Config: config, Credential: credential}
	if err := sink.validate(); err != nil {
		return nil, err
	}
	if credential == nil {
		var err error
		credential, err = newADXCredential(strings.TrimSpace(config.ClientID))
		if err != nil {
			return nil, fmt.Errorf("create Azure credential: %w", err)
		}
		sink.Credential = credential
	}
	kcsb := azkustodata.NewConnectionStringBuilder(strings.TrimSpace(config.ClusterURI)).WithTokenCredential(credential)
	transport := &adxResourceCachingTransport{base: http.DefaultTransport}
	httpClient := &http.Client{
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	client, err := newSDKADXQueuedClient(kcsb, config, httpClient)
	if err != nil {
		return nil, err
	}
	sink.client = client
	return sink, nil
}

func newSDKADXQueuedClient(
	kcsb *azkustodata.ConnectionStringBuilder,
	config ADXQueuedConfig,
	httpClient *http.Client,
) (*sdkADXQueuedClient, error) {
	discoveryKCSB, err := adxIngestionConnectionString(kcsb)
	if err != nil {
		return nil, fmt.Errorf("resolve ADX ingestion endpoint: %w", err)
	}
	discovery, err := azkustodata.New(discoveryKCSB, azkustodata.WithHttpClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("create ADX resource discovery client: %w", err)
	}
	ingestor, err := newSDKADXIngestor(kcsb, config, httpClient)
	if err != nil {
		discovery.Close()
		return nil, err
	}
	if err := ingestor.Close(); err != nil {
		discovery.Close()
		return nil, fmt.Errorf("close ADX queued ingestor validation client: %w", err)
	}
	return &sdkADXQueuedClient{
		discovery:  discovery,
		kcsb:       kcsb,
		config:     config,
		httpClient: httpClient,
	}, nil
}

func adxIngestionConnectionString(
	kcsb *azkustodata.ConnectionStringBuilder,
) (*azkustodata.ConnectionStringBuilder, error) {
	endpoint, err := url.Parse(kcsb.DataSource)
	if err != nil {
		return nil, err
	}
	host := endpoint.Hostname()
	if host == "" {
		return nil, fmt.Errorf("ADX cluster URI has no hostname")
	}
	if net.ParseIP(host) == nil &&
		!strings.EqualFold(host, "localhost") &&
		!strings.EqualFold(host, "onebox.dev.kusto.windows.net") &&
		!strings.HasPrefix(strings.ToLower(host), "ingest-") {
		endpoint.Host = "ingest-" + endpoint.Host
	}
	copy := *kcsb
	copy.DataSource = endpoint.String()
	return &copy, nil
}

func newSDKADXIngestor(
	kcsb *azkustodata.ConnectionStringBuilder,
	config ADXQueuedConfig,
	httpClient *http.Client,
) (*azkustoingest.Ingestion, error) {
	ingestor, err := azkustoingest.New(
		kcsb,
		azkustoingest.WithHttpClient(httpClient),
		azkustoingest.WithDefaultDatabase(strings.TrimSpace(config.Database)),
		azkustoingest.WithDefaultTable(strings.TrimSpace(config.Table)),
	)
	if err != nil {
		return nil, fmt.Errorf("create ADX queued ingestor: %w", err)
	}
	return ingestor, nil
}

func newADXCredential(clientID string) (azcore.TokenCredential, error) {
	if clientID == "" {
		return azidentity.NewDefaultAzureCredential(nil)
	}
	if strings.TrimSpace(os.Getenv("AZURE_FEDERATED_TOKEN_FILE")) != "" {
		return azidentity.NewWorkloadIdentityCredential(&azidentity.WorkloadIdentityCredentialOptions{
			ClientID: clientID,
		})
	}
	return azidentity.NewManagedIdentityCredential(&azidentity.ManagedIdentityCredentialOptions{
		ID: azidentity.ClientID(clientID),
	})
}

func (s *ADXQueuedSink) Name() string { return adxQueuedSinkName }

func (s *ADXQueuedSink) TerminalDrainTimeout() (time.Duration, error) {
	return metricsoffload.TerminalDrainTimeout(s.Config.MaxAttempts, s.Config.RetryBackoff, s.Config.FinalStatusTimeout)
}

func (s *ADXQueuedSink) ConfigIdentity() string {
	config := struct {
		Version            string `json:"version"`
		ClusterURI         string `json:"cluster_uri"`
		Database           string `json:"database"`
		Table              string `json:"table"`
		IngestionMapping   string `json:"ingestion_mapping"`
		ClientID           string `json:"client_id"`
		Identity           string `json:"identity"`
		MaxAttempts        int    `json:"max_attempts"`
		RetryBackoffNS     int64  `json:"retry_backoff_ns"`
		FinalTimeoutNS     int64  `json:"final_timeout_ns"`
		StatusPollInterval int64  `json:"status_poll_interval_ns"`
	}{
		Version: adxQueuedSinkName, ClusterURI: strings.TrimRight(strings.TrimSpace(s.Config.ClusterURI), "/"),
		Database: strings.TrimSpace(s.Config.Database), Table: strings.TrimSpace(s.Config.Table),
		IngestionMapping: strings.TrimSpace(s.Config.IngestionMapping), Identity: "azidentity-token-credential-v1",
		ClientID:    strings.TrimSpace(s.Config.ClientID),
		MaxAttempts: s.attempts(), RetryBackoffNS: int64(s.backoff()),
		FinalTimeoutNS: int64(s.finalTimeout()), StatusPollInterval: int64(s.pollInterval()),
	}
	raw, _ := json.Marshal(config)
	sum := sha256.Sum256(raw)
	return adxQueuedSinkName + ":" + hex.EncodeToString(sum[:])
}

func (s *ADXQueuedSink) Deliver(ctx context.Context, chunk MetricEventChunk) (DeliveryAck, error) {
	if err := s.validate(); err != nil {
		return DeliveryAck{}, err
	}
	if s.client == nil {
		return DeliveryAck{}, fmt.Errorf("ADX queued ingestor is not initialized")
	}
	if !validDigest(chunk.Digest) {
		return DeliveryAck{}, fmt.Errorf("ADX chunk digest is invalid")
	}
	events, err := decodeCanonicalEvents(chunk.NDJSON)
	if err != nil {
		return DeliveryAck{}, fmt.Errorf("ADX canonical NDJSON: %w", err)
	}
	if len(events) != len(chunk.Events) {
		return DeliveryAck{}, fmt.Errorf("ADX chunk event count mismatch")
	}
	ingestByValue := "taugrid-metric-chunk-" + chunk.Digest
	ingestByTag := "ingest-by:" + ingestByValue
	request := adxIngestRequest{
		Database: strings.TrimSpace(s.Config.Database), Table: strings.TrimSpace(s.Config.Table),
		Mapping: strings.TrimSpace(s.Config.IngestionMapping), IngestByValue: ingestByValue,
	}
	var last error
	for attempt := 1; attempt <= s.attempts(); attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, s.finalTimeout())
		result, err := s.client.Ingest(attemptCtx, chunk.NDJSON, request)
		if err == nil {
			status, waitErr := result.Wait(attemptCtx, s.pollInterval())
			if waitErr == nil && (status == adxStatusSucceeded || status == adxStatusSkipped) {
				cancel()
				return DeliveryAck{
					Samples: len(chunk.Events), Requests: attempt, Retries: attempt - 1,
					Metadata: map[string]string{
						"database": request.Database, "table": request.Table,
						"mapping": request.Mapping, "ingest_by_tag": ingestByTag, "final_status": string(status),
					},
				}, nil
			}
			if waitErr == nil {
				waitErr = fmt.Errorf("ADX ingestion ended without terminal success: status=%s", status)
			}
			if ctx.Err() == nil && (errors.Is(waitErr, context.DeadlineExceeded) || status == adxStatusQueued) {
				waitErr = adxTransientError{err: waitErr}
			}
			err = fmt.Errorf("ADX final ingestion status: %w", waitErr)
		} else {
			if ctx.Err() == nil && errors.Is(attemptCtx.Err(), context.DeadlineExceeded) {
				err = adxTransientError{err: err}
			}
			err = fmt.Errorf("ADX queue submission: %w", err)
		}
		cancel()
		last = err
		if ctx.Err() != nil {
			return DeliveryAck{}, errors.New(boundedText(fmt.Sprintf("ADX ingestion canceled: %s", adxDiagnostic(ctx.Err()))))
		}
		if !adxRetryable(err) || attempt == s.attempts() {
			return DeliveryAck{}, errors.New(boundedText(fmt.Sprintf(
				"ADX ingestion failed after %d attempt(s): %s", attempt, adxDiagnostic(err),
			)))
		}
		timer := time.NewTimer(s.backoff() * time.Duration(1<<(attempt-1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return DeliveryAck{}, errors.New(boundedText(fmt.Sprintf("ADX ingestion canceled: %s", adxDiagnostic(ctx.Err()))))
		case <-timer.C:
		}
	}
	return DeliveryAck{}, errors.New(boundedText(fmt.Sprintf("ADX ingestion failed: %s", adxDiagnostic(last))))
}

func (s *ADXQueuedSink) validate() error {
	for name, value := range map[string]string{
		"cluster URI": s.Config.ClusterURI, "database": s.Config.Database,
		"table": s.Config.Table, "ingestion mapping": s.Config.IngestionMapping,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("ADX %s is required", name)
		}
	}
	if s.Config.StatusPollInterval < 0 {
		return fmt.Errorf("ADX status poll interval must not be negative")
	}
	if _, err := metricsoffload.TerminalDrainTimeout(
		s.Config.MaxAttempts,
		s.Config.RetryBackoff,
		s.Config.FinalStatusTimeout,
	); err != nil {
		return fmt.Errorf("ADX delivery budget: %w", err)
	}
	return nil
}

func (s *ADXQueuedSink) attempts() int {
	if s.Config.MaxAttempts <= 0 {
		return metricsoffload.DefaultADXAttempts
	}
	return s.Config.MaxAttempts
}

func (s *ADXQueuedSink) backoff() time.Duration {
	if s.Config.RetryBackoff <= 0 {
		return metricsoffload.DefaultADXRetryBackoff
	}
	return s.Config.RetryBackoff
}

func (s *ADXQueuedSink) finalTimeout() time.Duration {
	if s.Config.FinalStatusTimeout <= 0 {
		return metricsoffload.DefaultADXFinalStatusTimeout
	}
	return s.Config.FinalStatusTimeout
}

func (s *ADXQueuedSink) pollInterval() time.Duration {
	if s.Config.StatusPollInterval <= 0 {
		return 5 * time.Second
	}
	return s.Config.StatusPollInterval
}

func (c *sdkADXQueuedClient) Ingest(ctx context.Context, payload []byte, request adxIngestRequest) (adxQueuedResult, error) {
	if c.discovery == nil {
		return nil, fmt.Errorf("ADX resource discovery client is not initialized")
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, fmt.Errorf("ADX queued ingestion requires an attempt deadline")
	}
	discoveryCtx := context.WithValue(ctx, adxResourceDiscoveryContextKey{}, true)
	if _, err := c.discovery.Mgmt(discoveryCtx, "NetDefaultDB", kql.New(".get ingestion resources")); err != nil {
		return nil, normalizeADXSDKError(fmt.Errorf("discover ADX ingestion resources: %w", err))
	}
	ingestor, err := newSDKADXIngestor(c.kcsb, c.config, c.httpClient)
	if err != nil {
		return nil, err
	}
	defer ingestor.Close()
	ingestIfNotExists, err := json.Marshal([]string{request.IngestByValue})
	if err != nil {
		return nil, fmt.Errorf("marshal ADX ingest-if-not-exists tags: %w", err)
	}
	result, err := ingestor.FromReader(
		ctx,
		bytes.NewReader(payload),
		azkustoingest.Database(request.Database),
		azkustoingest.Table(request.Table),
		azkustoingest.IngestionMappingRef(request.Mapping, azkustoingest.JSON),
		azkustoingest.FileFormat(azkustoingest.JSON),
		azkustoingest.RawDataSize(int64(len(payload))),
		azkustoingest.IfNotExists(string(ingestIfNotExists)),
		azkustoingest.Tags([]string{"ingest-by:" + request.IngestByValue}),
		azkustoingest.ReportResultToTable(),
	)
	if err != nil {
		return nil, normalizeADXSDKError(err)
	}
	return &sdkADXQueuedResult{result: result}, nil
}

func (r *sdkADXQueuedResult) Wait(ctx context.Context, pollInterval time.Duration) (adxFinalStatus, error) {
	err := <-r.result.Wait(ctx, azkustoingest.WithImmediateFirst(), azkustoingest.WithInterval(pollInterval))
	return adxFinalStatusFromSDKError(err)
}

func adxFinalStatusFromSDKError(err error) (adxFinalStatus, error) {
	if err == nil {
		return adxStatusSucceeded, nil
	}
	status, statusErr := azkustoingest.GetIngestionStatus(err)
	if statusErr != nil {
		return adxStatusFailed, err
	}
	switch status {
	case azkustoingest.Succeeded:
		return adxStatusSucceeded, nil
	case azkustoingest.Skipped:
		return adxStatusSkipped, nil
	case azkustoingest.Queued:
		return adxStatusQueued, fmt.Errorf("ADX ingestion is only queued, not final")
	default:
		return adxStatusFailed, err
	}
}

func (t *adxResourceCachingTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if !isADXIngestionResourcesRequest(request) {
		return t.base.RoundTrip(request)
	}
	t.mu.RLock()
	cached := t.resources
	t.mu.RUnlock()
	isPreflight, _ := request.Context().Value(adxResourceDiscoveryContextKey{}).(bool)
	if cached != nil && !isPreflight {
		return cached.response(request), nil
	}
	if !isPreflight {
		return nil, fmt.Errorf("ADX ingestion resources must be prefetched for this attempt")
	}
	response, err := t.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return response, nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		_ = response.Body.Close()
		return nil, err
	}
	if err := response.Body.Close(); err != nil {
		return nil, err
	}
	cached = &adxCachedHTTPResponse{
		statusCode: response.StatusCode,
		header:     response.Header.Clone(),
		body:       body,
	}
	t.mu.Lock()
	t.resources = cached
	t.mu.Unlock()
	return cached.response(request), nil
}

func (r *adxCachedHTTPResponse) response(request *http.Request) *http.Response {
	return &http.Response{
		StatusCode: r.statusCode,
		Status:     fmt.Sprintf("%d %s", r.statusCode, http.StatusText(r.statusCode)),
		Header:     r.header.Clone(),
		Body:       io.NopCloser(bytes.NewReader(r.body)),
		Request:    request,
	}
}

func isADXIngestionResourcesRequest(request *http.Request) bool {
	if request.Body == nil || !strings.Contains(request.URL.Path, "/v1/rest/mgmt") {
		return false
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return false
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	var command struct {
		CSL string `json:"csl"`
	}
	return json.Unmarshal(body, &command) == nil && command.CSL == ".get ingestion resources"
}

func adxRetryable(err error) bool {
	if err == nil {
		return false
	}
	var transient adxTransientError
	if errors.As(err, &transient) {
		return true
	}
	return adxSDKRetryable(err)
}

func normalizeADXSDKError(err error) error {
	if err == nil || !adxSDKRetryable(err) {
		return err
	}
	return adxTransientError{err: err}
}

func adxSDKRetryable(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var authErr *azidentity.AuthenticationFailedError
	if errors.As(err, &authErr) {
		return false
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if azkustoingest.IsStatusRecord(current) {
			return azkustoingest.IsRetryable(current)
		}
	}
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) {
		if responseErr.StatusCode == http.StatusUnauthorized || responseErr.StatusCode == http.StatusForbidden ||
			responseErr.StatusCode == http.StatusBadRequest || responseErr.StatusCode == http.StatusNotFound {
			return false
		}
		return responseErr.StatusCode == http.StatusRequestTimeout ||
			responseErr.StatusCode == http.StatusConflict ||
			responseErr.StatusCode == http.StatusTooManyRequests || responseErr.StatusCode >= 500
	}
	var httpErr *kustoerrors.HttpError
	if errors.As(err, &httpErr) {
		return httpErr.StatusCode == http.StatusRequestTimeout ||
			httpErr.StatusCode == http.StatusConflict ||
			httpErr.StatusCode == http.StatusTooManyRequests || httpErr.StatusCode >= 500
	}
	if status, ok := flattenedKustoHTTPStatus(err); ok {
		return status == http.StatusRequestTimeout ||
			status == http.StatusConflict ||
			status == http.StatusTooManyRequests || status >= 500
	}
	if kustoerrors.Retry(err) {
		return true
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func adxDiagnostic(err error) string {
	if err == nil {
		return "no error"
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	var authErr *azidentity.AuthenticationFailedError
	if errors.As(err, &authErr) {
		return "Azure authentication failed"
	}
	for current := err; current != nil; current = errors.Unwrap(current) {
		if !azkustoingest.IsStatusRecord(current) {
			continue
		}
		status, _ := azkustoingest.GetIngestionStatus(current)
		failure, _ := azkustoingest.GetIngestionFailureStatus(current)
		code, _ := azkustoingest.GetErrorCode(current)
		return fmt.Sprintf(
			"ingestion status=%s failure=%s code=%s",
			redactADXDiagnosticText(string(status)),
			redactADXDiagnosticText(string(failure)),
			redactADXDiagnosticText(code),
		)
	}
	var responseErr *azcore.ResponseError
	if errors.As(err, &responseErr) {
		return fmt.Sprintf(
			"Azure HTTP status=%d code=%s",
			responseErr.StatusCode,
			redactADXDiagnosticText(responseErr.ErrorCode),
		)
	}
	var httpErr *kustoerrors.HttpError
	if errors.As(err, &httpErr) {
		return fmt.Sprintf("Kusto HTTP status=%d", httpErr.StatusCode)
	}
	if status, ok := flattenedKustoHTTPStatus(err); ok {
		return fmt.Sprintf("Kusto HTTP status=%d", status)
	}
	var kustoErr *kustoerrors.Error
	if errors.As(err, &kustoErr) {
		return fmt.Sprintf("Kusto operation=%s kind=%s", kustoErr.Op.String(), kustoErr.Kind.String())
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return fmt.Sprintf("network error timeout=%t", networkErr.Timeout())
	}
	return fmt.Sprintf("error type=%T", err)
}

func flattenedKustoHTTPStatus(err error) (int, bool) {
	text := err.Error()
	matchedPrefix := false
	for _, prefix := range adxFlattenedKustoPrefixes {
		if strings.HasPrefix(text, prefix) {
			text = strings.TrimPrefix(text, prefix)
			matchedPrefix = true
			break
		}
	}
	if !matchedPrefix {
		return 0, false
	}
	match := adxFlattenedHTTPStatus.FindStringSubmatch(text)
	if len(match) != 2 {
		return 0, false
	}
	status := 0
	for _, digit := range match[1] {
		status = status*10 + int(digit-'0')
	}
	return status, true
}

func redactADXDiagnosticText(text string) string {
	text = adxURLPattern.ReplaceAllStringFunc(text, func(raw string) string {
		if index := strings.IndexByte(raw, '?'); index >= 0 {
			return raw[:index] + "?REDACTED"
		}
		return raw
	})
	return adxSASQueryPattern.ReplaceAllString(text, "${1}REDACTED")
}
