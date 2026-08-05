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
}

// Query runs kql against ADX and parses the JSON response into generic Rows.
func (c SDKClient) Query(ctx context.Context, query string) ([]Row, error) {
	endpoint := strings.TrimSpace(c.Endpoint)
	if endpoint == "" {
		return nil, ErrNoQueryCommand
	}
	database := firstNonEmpty(c.Database, expkusto.DefaultDatabase)
	run := c.queryJSON
	if run == nil {
		run = func(ctx context.Context, database, query string) (string, error) {
			return runADXQuery(ctx, endpoint, database, query)
		}
	}
	raw, err := run(ctx, database, query)
	if err != nil {
		return nil, fmt.Errorf("execute kusto query (endpoint=%s database=%s): %w", endpoint, database, err)
	}
	rows, err := ParseRows([]byte(raw))
	if err != nil {
		return nil, fmt.Errorf("parse kusto query output: %w", err)
	}
	return rows, nil
}

// RunADXQuery exposes the native transport to callers that parse the ADX JSON
// response themselves. Stellar's Kusto source decodes metric rows with its own
// parser, so it needs the raw QueryToJson payload rather than generic Rows;
// going through this wrapper keeps azure-kusto-go out of the portal packages.
func RunADXQuery(ctx context.Context, endpoint, database, query string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return "", ErrNoQueryCommand
	}
	return runADXQuery(ctx, endpoint, firstNonEmpty(database, expkusto.DefaultDatabase), query)
}

// runADXQuery is the production transport: DefaultAzureCredential →
// azkustodata client → QueryToJson. AddUnsafe passes the generated KQL through
// verbatim (the portal builds its own KQL and escapes filter values via
// QuoteString), avoiding the multi-line JSON-escaping hacks of the shell adapter.
func runADXQuery(ctx context.Context, endpoint, database, query string) (string, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return "", fmt.Errorf("create Azure credential: %w", err)
	}
	client, err := azkustodata.New(
		azkustodata.NewConnectionStringBuilder(endpoint).WithTokenCredential(cred),
	)
	if err != nil {
		return "", fmt.Errorf("create ADX client: %w", err)
	}
	defer client.Close()
	return client.QueryToJson(ctx, database, kql.New("").AddUnsafe(query))
}
