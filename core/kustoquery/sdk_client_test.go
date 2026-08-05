package kustoquery

import (
	"context"
	"errors"
	"testing"
)

func TestSDKClientQueryParsesJSON(t *testing.T) {
	// The SDK client delegates transport to an injectable queryJSON hook and
	// reuses ParseRows for the response, so we can exercise the full Query path
	// (endpoint/database defaulting + parse) without a live ADX cluster.
	var gotDB, gotKQL string
	c := SDKClient{
		Endpoint: "https://example.kusto.windows.net",
		Database: "Metrics",
		queryJSON: func(_ context.Context, db, query string) (string, error) {
			gotDB, gotKQL = db, query
			// Mirror the real azure-kusto-go QueryToJson wire shape: a fragmented
			// v2 stream where PrimaryResult arrives as TableHeader+TableFragment,
			// not a single {Columns,Rows} object the SDK never emits.
			return `[
				{"FrameType":"DataSetHeader","Version":"v2.0"},
				{"FrameType":"TableHeader","TableId":1,"TableKind":"PrimaryResult",
				 "Columns":[{"ColumnName":"gpu"}]},
				{"FrameType":"TableFragment","TableId":1,"Rows":[["0"],["1"]]},
				{"FrameType":"TableCompletion","TableId":1,"RowCount":2},
				{"FrameType":"DataSetCompletion","HasErrors":false}
			]`, nil
		},
	}
	rows, err := c.Query(context.Background(), "GpuHealth()")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if gotDB != "Metrics" {
		t.Fatalf("db = %q, want Metrics", gotDB)
	}
	if gotKQL != "GpuHealth()" {
		t.Fatalf("kql = %q, want GpuHealth()", gotKQL)
	}
}

func TestSDKClientQueryDefaultsDatabase(t *testing.T) {
	// A zero Database falls back to the Metrics ADX default, matching the
	// shell-out Client's behavior.
	var gotDB string
	c := SDKClient{
		Endpoint: "https://example.kusto.windows.net",
		queryJSON: func(_ context.Context, db, _ string) (string, error) {
			gotDB = db
			return "", nil
		},
	}
	if _, err := c.Query(context.Background(), "GpuHealth()"); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotDB == "" {
		t.Fatal("db was not defaulted")
	}
}

func TestSDKClientQueryNoEndpoint(t *testing.T) {
	// An unconfigured SDKClient (no endpoint) reports ErrNoQueryCommand so the
	// portal can treat the board as disabled, mirroring Client.
	_, err := SDKClient{}.Query(context.Background(), "GpuHealth()")
	if !errors.Is(err, ErrNoQueryCommand) {
		t.Fatalf("err = %v, want ErrNoQueryCommand", err)
	}
}

func TestSDKClientQueryFoldsError(t *testing.T) {
	c := SDKClient{
		Endpoint: "https://example.kusto.windows.net",
		queryJSON: func(_ context.Context, _, _ string) (string, error) {
			return "", errors.New("boom")
		},
	}
	_, err := c.Query(context.Background(), "GpuHealth()")
	if err == nil {
		t.Fatal("Query err = nil, want failure")
	}
	if !contains(err.Error(), "boom") {
		t.Fatalf("err = %q, want it to include 'boom'", err.Error())
	}
}
