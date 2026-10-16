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

type Ingestor interface {
	FromReader(ctx context.Context, reader io.Reader, options ...azkustoingest.FileOption) (*azkustoingest.Result, error)
	Close() error
}

type KustoSink struct {
	ingestor Ingestor
	mu       sync.Mutex
	buf      []Outcome
}

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

func newKustoSinkForTest(ingestor Ingestor) *KustoSink {
	return &KustoSink{ingestor: ingestor}
}

func (k *KustoSink) Record(_ context.Context, o Outcome) error {
	if o.RunID == 0 {
		return nil
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.buf = append(k.buf, o)
	return nil
}

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
