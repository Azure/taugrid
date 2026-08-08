// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustoingest"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	kustoTable   = "TestOutcomes"
	kustoMapping = "TestOutcomesMapping"
)

// Ingestor abstracts *azkustoingest.Managed for testing.
// Production uses the real Managed client; tests inject a mock.
type Ingestor interface {
	FromReader(ctx context.Context, reader io.Reader, options ...azkustoingest.FileOption) (*azkustoingest.Result, error)
	Close() error
}

// KustoSink writes test outcomes to ADX via managed ingestion (streaming
// with automatic queued fallback). This replaces the previous .ingest inline
// approach, which consumed control-command slots and was subject to
// CapacityPolicy/Ingestion throttling (HTTP 429).
//
// The managed client handles retry and fallback internally — no custom
// retry logic is needed. Ingestion errors are logged to stderr but never
// fail the test.
type KustoSink struct {
	ingestor Ingestor
	mu       sync.Mutex
	buf      []Outcome
}

// NewKustoSink creates a KustoSink that ingests to the given ADX endpoint and database.
// Auth uses DefaultAzureCredential (OIDC in CI, az login locally).
func NewKustoSink(endpoint, database string) (*KustoSink, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("results: kusto credential: %w", err)
	}

	kcsb := azkustodata.NewConnectionStringBuilder(endpoint).WithTokenCredential(cred)

	ingestor, err := azkustoingest.NewManaged(kcsb,
		azkustoingest.WithDefaultDatabase(database),
		azkustoingest.WithDefaultTable(kustoTable),
	)
	if err != nil {
		return nil, fmt.Errorf("results: kusto managed ingestor: %w", err)
	}

	return &KustoSink{ingestor: ingestor}, nil
}

// newKustoSinkForTest creates a KustoSink with an injected ingestor (for testing).
func newKustoSinkForTest(ingestor Ingestor) *KustoSink {
	return &KustoSink{ingestor: ingestor}
}

// Record buffers an outcome for later ingestion. Skips outcomes with RunID == 0
// (local runs have no CI context — ingesting them would be noise).
// Safe for concurrent use.
func (k *KustoSink) Record(_ context.Context, o Outcome) error {
	if o.RunID == 0 {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.buf = append(k.buf, o)
	return nil
}

// Flush serializes buffered outcomes as NDJSON and ingests them to ADX using
// managed ingestion (streaming with queued fallback). On error: logs to stderr
// but returns nil — ingestion failures must never fail the test run. Always
// closes the ingestor to release resources.
func (k *KustoSink) Flush() error {
	k.mu.Lock()
	outcomes := k.buf
	k.buf = nil
	k.mu.Unlock()

	defer func() {
		if err := k.ingestor.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "results: kusto ingestor close: %v\n", err)
		}
	}()

	if len(outcomes) == 0 {
		return nil
	}

	// Build NDJSON payload for ingestion.
	var buf strings.Builder
	for i, o := range outcomes {
		b, err := json.Marshal(o)
		if err != nil {
			fmt.Fprintf(os.Stderr, "results: kusto marshal: %v\n", err)
			return nil
		}
		if i > 0 {
			buf.WriteByte('\n')
		}
		buf.Write(b)
	}

	// Ingest via managed client — streaming first, queued fallback on transient errors.
	// 30s timeout prevents indefinite blocking if the queued fallback path stalls
	// (blob upload or Service Bus enqueue). Tests have already passed at this point;
	// losing telemetry is acceptable, but hanging the runner for 90 min is not.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_, err := k.ingestor.FromReader(ctx, strings.NewReader(buf.String()),
		azkustoingest.FileFormat(azkustoingest.MultiJSON),
		azkustoingest.IngestionMappingRef(kustoMapping, azkustoingest.MultiJSON),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "results: kusto ingestion failed (outcomes lost): %v\n", err)
	}
	return nil
}
