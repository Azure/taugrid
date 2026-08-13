// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runhistory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustoingest"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/taugrid/core/expkusto"
)

type kustoWriter struct {
	endpoint   string
	database   string
	table      string
	timeout    time.Duration
	credential azcore.TokenCredential
}

func NewKustoWriter(config WriterConfig) (Writer, error) {
	if config.Endpoint == "" {
		return nil, fmt.Errorf("Kusto endpoint is required")
	}
	if config.Database == "" {
		return nil, fmt.Errorf("Kusto database is required")
	}
	if config.Table == "" {
		return nil, fmt.Errorf("Kusto table is required")
	}
	if config.Timeout <= 0 {
		config.Timeout = 2 * time.Minute
	}
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}
	return &kustoWriter{endpoint: config.Endpoint, database: config.Database, table: config.Table, timeout: config.Timeout, credential: credential}, nil
}

func (w *kustoWriter) Write(ctx context.Context, records []Record) error {
	data, err := marshalLifecycleRecords(records)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	// Use queued ingestion directly. Managed ingestion may attempt streaming
	// first and then fall back after consuming an io.Reader, which makes a
	// lifecycle batch non-replayable. The recorder needs one durable path.
	ingestor, err := azkustoingest.New(
		azkustodata.NewConnectionStringBuilder(w.endpoint).WithTokenCredential(w.credential),
		azkustoingest.WithDefaultDatabase(w.database),
		azkustoingest.WithDefaultTable(w.table),
	)
	if err != nil {
		return fmt.Errorf("create queued Kusto ingestor: %w", err)
	}
	writeCtx, cancel := context.WithTimeout(ctx, w.timeout)
	defer cancel()
	tag := ingestByTag(records)
	result, err := ingestor.FromReader(writeCtx, bytes.NewReader(data),
		azkustoingest.Database(w.database),
		azkustoingest.Table(w.table),
		// Keep the mapping inline for direct upgrades from releases that created
		// only the table. The schema command also creates the same named mapping
		// for operators and other ADX clients; both derive from expkusto.
		azkustoingest.IngestionMapping(expkusto.RunLifecycleIngestionMapping(), azkustoingest.JSON),
		azkustoingest.FlushImmediately(),
		azkustoingest.ReportResultToTable(),
		azkustoingest.Tags([]string{"ingest-by:" + tag}),
		azkustoingest.IfNotExists(tag),
	)
	if err != nil {
		_ = ingestor.Close()
		return fmt.Errorf("start queued ingestion: %w", err)
	}
	if err := <-result.Wait(writeCtx, azkustoingest.WithImmediateFirst()); err != nil && !isSuccessfulIngestionStatus(err) {
		_ = ingestor.Close()
		return fmt.Errorf("wait for queued ingestion acknowledgement: %w", err)
	}
	if err := ingestor.Close(); err != nil {
		return fmt.Errorf("close queued Kusto ingestor: %w", err)
	}
	return nil
}

// isSuccessfulIngestionStatus recognizes final ADX statuses that complete a
// lifecycle write successfully. ADX reports an ingestIfNotExists duplicate as
// Skipped; the original batch is already durable, so retrying it is incorrect.
func isSuccessfulIngestionStatus(err error) bool {
	status, statusErr := azkustoingest.GetIngestionStatus(err)
	return statusErr == nil && isSuccessfulIngestionStatusCode(status)
}

func isSuccessfulIngestionStatusCode(status azkustoingest.StatusCode) bool {
	return status == azkustoingest.Skipped
}

// marshalLifecycleRecords encodes the queued-ingestion payload as NDJSON: one
// lifecycle observation per line. ADX JSON mappings consume each line as a
// separate source record; a JSON array would require MultiJSON semantics.
func marshalLifecycleRecords(records []Record) ([]byte, error) {
	var data bytes.Buffer
	encoder := json.NewEncoder(&data)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			return nil, fmt.Errorf("marshal lifecycle record: %w", err)
		}
	}
	return data.Bytes(), nil
}

func ingestByTag(records []Record) string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.ObservationID)
	}
	sort.Strings(ids)
	sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
	return "tau-lifecycle-" + hex.EncodeToString(sum[:])
}
