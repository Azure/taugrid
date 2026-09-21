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
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustoingest"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	adxQueuedSinkName    = "adx-queued-v1"
	MaxADXQueuedAttempts = 10
)

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

type sdkADXQueuedClient struct {
	ingestor *azkustoingest.Ingestion
}

type sdkADXQueuedResult struct {
	result *azkustoingest.Result
}

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
	ingestor, err := azkustoingest.New(
		kcsb,
		azkustoingest.WithDefaultDatabase(strings.TrimSpace(config.Database)),
		azkustoingest.WithDefaultTable(strings.TrimSpace(config.Table)),
	)
	if err != nil {
		return nil, fmt.Errorf("create ADX queued ingestor: %w", err)
	}
	sink.client = &sdkADXQueuedClient{ingestor: ingestor}
	return sink, nil
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
		result, err := s.client.Ingest(ctx, chunk.NDJSON, request)
		if err == nil {
			waitCtx, cancel := context.WithTimeout(ctx, s.finalTimeout())
			status, waitErr := result.Wait(waitCtx, s.pollInterval())
			cancel()
			if waitErr == nil && status == adxStatusSucceeded {
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
			err = fmt.Errorf("ADX queue submission: %w", err)
		}
		last = err
		if ctx.Err() != nil {
			return DeliveryAck{}, errors.New(boundedText(fmt.Sprintf("ADX ingestion canceled: %v", ctx.Err())))
		}
		if !adxRetryable(err) || attempt == s.attempts() {
			return DeliveryAck{}, errors.New(boundedText(fmt.Sprintf(
				"ADX ingestion failed after %d attempt(s): %v", attempt, err,
			)))
		}
		timer := time.NewTimer(s.backoff() * time.Duration(1<<(attempt-1)))
		select {
		case <-ctx.Done():
			timer.Stop()
			return DeliveryAck{}, errors.New(boundedText(fmt.Sprintf("ADX ingestion canceled: %v", ctx.Err())))
		case <-timer.C:
		}
	}
	return DeliveryAck{}, errors.New(boundedText(fmt.Sprintf("ADX ingestion failed: %v", last)))
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
	if s.Config.MaxAttempts < 0 || s.Config.MaxAttempts > MaxADXQueuedAttempts || s.Config.RetryBackoff < 0 ||
		s.Config.FinalStatusTimeout < 0 || s.Config.StatusPollInterval < 0 {
		return fmt.Errorf("ADX max attempts must be between 0 and %d and retry/timeout settings must be nonnegative", MaxADXQueuedAttempts)
	}
	return nil
}

func (s *ADXQueuedSink) attempts() int {
	if s.Config.MaxAttempts <= 0 {
		return 3
	}
	return s.Config.MaxAttempts
}

func (s *ADXQueuedSink) backoff() time.Duration {
	if s.Config.RetryBackoff <= 0 {
		return time.Second
	}
	return s.Config.RetryBackoff
}

func (s *ADXQueuedSink) finalTimeout() time.Duration {
	if s.Config.FinalStatusTimeout <= 0 {
		return 10 * time.Minute
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
	result, err := c.ingestor.FromReader(
		ctx,
		bytes.NewReader(payload),
		azkustoingest.Database(request.Database),
		azkustoingest.Table(request.Table),
		azkustoingest.IngestionMappingRef(request.Mapping, azkustoingest.JSON),
		azkustoingest.FileFormat(azkustoingest.JSON),
		azkustoingest.RawDataSize(int64(len(payload))),
		azkustoingest.IfNotExists(request.IngestByValue),
		azkustoingest.Tags([]string{"ingest-by:" + request.IngestByValue}),
		azkustoingest.ReportResultToTable(),
	)
	if err != nil {
		return nil, err
	}
	return &sdkADXQueuedResult{result: result}, nil
}

func (r *sdkADXQueuedResult) Wait(ctx context.Context, pollInterval time.Duration) (adxFinalStatus, error) {
	err := <-r.result.Wait(ctx, azkustoingest.WithImmediateFirst(), azkustoingest.WithInterval(pollInterval))
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
	case azkustoingest.Queued:
		return adxStatusQueued, fmt.Errorf("ADX ingestion is only queued, not final")
	default:
		return adxStatusFailed, err
	}
}

func adxRetryable(err error) bool {
	if err == nil {
		return false
	}
	var transient adxTransientError
	if errors.As(err, &transient) {
		return true
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var authErr *azidentity.AuthenticationFailedError
	if errors.As(err, &authErr) {
		return false
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
	for current := err; current != nil; current = errors.Unwrap(current) {
		if azkustoingest.IsStatusRecord(current) {
			return azkustoingest.IsRetryable(current)
		}
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, permanent := range []string{
		"authentication", "authorization", "unauthorized", "forbidden",
		"mapping", "schema", "database does not exist", "table does not exist",
		"bad request", "invalid",
	} {
		if strings.Contains(text, permanent) {
			return false
		}
	}
	return false
}
