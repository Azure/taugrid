// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// SDKClient is the portal's native Azure Kusto access path. Unlike Client — which
// shells out to an external --kusto-query-command (outsourcing auth + transport,
// which forces IMDS-token and JSON-escaping hacks in the adapter script) —
// SDKClient talks to ADX directly through azure-kusto-go with
// DefaultAzureCredential (workload identity, managed identity, az login, ...).
// It reuses ParseRows so board code sees the same generic Rows.
package kustoquery

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Azure/azure-kusto-go/azkustodata"
	"github.com/Azure/azure-kusto-go/azkustodata/kql"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"

	"github.com/Azure/taugrid/core/expkusto"
)

// SDKClient executes KQL against ADX using the native azure-kusto-go SDK. The
// zero endpoint disables the client (Query returns ErrNoQueryCommand); the zero
// database falls back to the Metrics ADX default. queryJSON is an injectable
// transport seam: nil means "use the real DefaultAzureCredential-backed client",
// and tests fake it to exercise the parse path without a live cluster.
type SDKClient struct {
	Endpoint  string
	Database  string
	queryJSON func(ctx context.Context, database, kql string) (string, error)
	newQuery  func(endpoint string) (func(context.Context, string, string) (string, error), error)
	initOnce  sync.Once
	run       func(context.Context, string, string) (string, error)
	initErr   error
}

// Query runs kql against ADX and parses the JSON response into generic Rows.
func (c *SDKClient) Query(ctx context.Context, query string) ([]Row, error) {
	raw, err := c.RawQuery(ctx, query)
	if err != nil {
		return nil, err
	}
	rows, err := ParseRows([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("parse kusto query output: %w", err)
	}
	return rows, nil
}

// RawQuery runs kql through a lazily initialized, reusable SDK transport.
func (c *SDKClient) RawQuery(ctx context.Context, query string) (string, error) {
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		return "", ErrNoQueryCommand
	}
	database := firstNonEmpty(c.Database, expkusto.DefaultDatabase)
	run := c.queryJSON
	if run == nil {
		c.initOnce.Do(func() {
			factory := c.newQuery
			if factory == nil {
				factory = newADXQuery
			}
			c.run, c.initErr = factory(endpoint)
		})
		if c.initErr != nil {
			return "", c.initErr
		}
		run = c.run
	}
	raw, err := run(ctx, database, query)
	if err != nil {
		return "", fmt.Errorf("execute kusto query (endpoint=%s database=%s): %w", endpoint, database, err)
	}
	return raw, nil
}

// NewRawSDKQuery returns a raw ADX query function backed by one reusable SDK
// client. Stellar parses its own result schema, so it uses this instead of Query.
func NewRawSDKQuery(endpoint, database string) func(context.Context, string) (string, error) {
	client := &SDKClient{Endpoint: endpoint, Database: database}
	return client.RawQuery
}

// newADXQuery initializes the production transport once. The returned function
// reuses the credential, HTTP connections, and SDK client for concurrent queries.
func newADXQuery(endpoint string) (func(context.Context, string, string) (string, error), error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("create Azure credential: %w", err)
	}
	client, err := azkustodata.New(
		azkustodata.NewConnectionStringBuilder(endpoint).WithTokenCredential(cred),
	)
	if err != nil {
		return nil, fmt.Errorf("create ADX client: %w", err)
	}
	return func(ctx context.Context, database, query string) (string, error) {
		return client.QueryToJson(ctx, database, kql.New("").AddUnsafe(query))
	}, nil
}
