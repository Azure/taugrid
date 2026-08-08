// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kustoquery

import (
	"context"
	"errors"
	"testing"
)

func TestParseRowsV1Tables(t *testing.T) {
	// Kusto v1 REST shape: {"Tables":[{TableName, Columns, Rows}]}. Rows are
	// positional arrays mapped to column names.
	raw := []byte(`{"Tables":[
		{"TableName":"Table_0",
		 "Columns":[{"ColumnName":"instance"},{"ColumnName":"gpu"},{"ColumnName":"Value"}],
		 "Rows":[["node-a","0",42.5],["node-b","1","7"]]}
	]}`)
	rows, err := ParseRows(raw)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %#v", len(rows), rows)
	}
	if got := rows[0].Str("instance"); got != "node-a" {
		t.Fatalf("row0 instance = %q, want node-a", got)
	}
	if v, ok := rows[0].Num("Value"); !ok || v != 42.5 {
		t.Fatalf("row0 Value = %v (ok=%v), want 42.5", v, ok)
	}
	// Numeric-string column parses via Num (Kusto tostring()).
	if v, ok := rows[1].Num("Value"); !ok || v != 7 {
		t.Fatalf("row1 Value = %v (ok=%v), want 7", v, ok)
	}
}

func TestParseRowsV1TablesPrimaryResultSelected(t *testing.T) {
	// When multiple tables exist, the PrimaryResult table wins over others
	// (e.g. QueryProperties / QueryStatus tables in real v1 responses).
	raw := []byte(`{"Tables":[
		{"TableName":"PrimaryResult","Columns":[{"ColumnName":"gpu"}],"Rows":[["0"],["1"]]},
		{"TableName":"QueryStatus","Columns":[{"ColumnName":"x"}],"Rows":[["ignored"]]}
	]}`)
	rows, err := ParseRows(raw)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 from PrimaryResult", len(rows))
	}
}

func TestParseRowsV2FrameArray(t *testing.T) {
	// Kusto v2 REST shape: a frame array; only the PrimaryResult DataTable frame
	// carries results.
	raw := []byte(`[
		{"FrameType":"DataSetHeader","IsProgressive":false,"Version":"v2.0"},
		{"FrameType":"DataTable","TableKind":"PrimaryResult",
		 "Columns":[{"ColumnName":"instance"},{"ColumnName":"Value"}],
		 "Rows":[["node-a",1.5]]},
		{"FrameType":"DataTable","TableKind":"QueryProperties",
		 "Columns":[{"ColumnName":"x"}],"Rows":[["ignored"]]},
		{"FrameType":"DataSetCompletion","HasErrors":false}
	]`)
	rows, err := ParseRows(raw)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %#v", len(rows), rows)
	}
	if got := rows[0].Str("instance"); got != "node-a" {
		t.Fatalf("instance = %q, want node-a", got)
	}
	if v, ok := rows[0].Num("Value"); !ok || v != 1.5 {
		t.Fatalf("Value = %v (ok=%v), want 1.5", v, ok)
	}
}

func TestParseRowsV2FragmentedFrameArray(t *testing.T) {
	// Real azure-kusto-go QueryToJson shape (IsFragmented=true): the
	// PrimaryResult table is split across a TableHeader (Columns + TableId) and
	// TableFragment frames (Rows for the matching TableId). Only the
	// QueryProperties/QueryCompletionInformation tables arrive as DataTable
	// frames, so the DataTable-only path would silently return 0 rows.
	raw := []byte(`[
		{"FrameType":"DataSetHeader","IsProgressive":false,"Version":"v2.0"},
		{"FrameType":"DataTable","TableId":0,"TableKind":"QueryProperties",
		 "Columns":[{"ColumnName":"x"}],"Rows":[["ignored"]]},
		{"FrameType":"TableHeader","TableId":1,"TableKind":"PrimaryResult",
		 "Columns":[{"ColumnName":"instance"},{"ColumnName":"Value"}]},
		{"FrameType":"TableFragment","TableFragmentType":"DataAppend","TableId":1,
		 "Rows":[["node-a",1.5]]},
		{"FrameType":"TableFragment","TableFragmentType":"DataAppend","TableId":1,
		 "Rows":[["node-b",2.5]]},
		{"FrameType":"TableCompletion","TableId":1,"RowCount":2},
		{"FrameType":"DataTable","TableId":2,"TableKind":"QueryCompletionInformation",
		 "Columns":[{"ColumnName":"y"}],"Rows":[["ignored"]]},
		{"FrameType":"DataSetCompletion","HasErrors":false}
	]`)
	rows, err := ParseRows(raw)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2: %#v", len(rows), rows)
	}
	if got := rows[0].Str("instance"); got != "node-a" {
		t.Fatalf("row0 instance = %q, want node-a", got)
	}
	if v, ok := rows[1].Num("Value"); !ok || v != 2.5 {
		t.Fatalf("row1 Value = %v (ok=%v), want 2.5", v, ok)
	}
}

func TestParseRowsColumnsRowsObject(t *testing.T) {
	// {"Columns":[...],"Rows":[...]} shape with string column names.
	raw := []byte(`{"Columns":["team","gpuHours"],"Rows":[["research",12.0],["infra",3.5]]}`)
	rows, err := ParseRows(raw)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if got := rows[0].Str("team"); got != "research" {
		t.Fatalf("team = %q, want research", got)
	}
	if v, ok := rows[1].Num("gpuHours"); !ok || v != 3.5 {
		t.Fatalf("gpuHours = %v (ok=%v), want 3.5", v, ok)
	}
}

func TestParseRowsArrayOfObjects(t *testing.T) {
	raw := []byte(`[{"instance":"node-a","Value":10},{"instance":"node-b","Value":20}]`)
	rows, err := ParseRows(raw)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if got := rows[1].Str("instance"); got != "node-b" {
		t.Fatalf("instance = %q, want node-b", got)
	}
}

func TestParseRowsJSONL(t *testing.T) {
	raw := []byte("{\"instance\":\"node-a\",\"Value\":1}\n{\"instance\":\"node-b\",\"Value\":2}\n")
	rows, err := ParseRows(raw)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	if v, ok := rows[0].Num("Value"); !ok || v != 1 {
		t.Fatalf("row0 Value = %v (ok=%v), want 1", v, ok)
	}
}

func TestParseRowsEmpty(t *testing.T) {
	rows, err := ParseRows([]byte("   \n"))
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows = %d, want 0", len(rows))
	}
}

func TestParseRowsSingleObject(t *testing.T) {
	// A bare object with no Tables/Columns/Rows keys is treated as one row.
	raw := []byte(`{"instance":"node-a","Value":5}`)
	rows, err := ParseRows(raw)
	if err != nil {
		t.Fatalf("ParseRows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if got := rows[0].Str("instance"); got != "node-a" {
		t.Fatalf("instance = %q, want node-a", got)
	}
}

func TestRowStr(t *testing.T) {
	r := Row{"a": "text", "b": 3.0, "c": nil}
	if got := r.Str("a"); got != "text" {
		t.Fatalf("Str(a) = %q, want text", got)
	}
	if got := r.Str("b"); got != "3" {
		t.Fatalf("Str(b) = %q, want 3", got)
	}
	if got := r.Str("c"); got != "" {
		t.Fatalf("Str(c) = %q, want empty", got)
	}
	if got := r.Str("missing"); got != "" {
		t.Fatalf("Str(missing) = %q, want empty", got)
	}
}

func TestRowNum(t *testing.T) {
	r := Row{"f": 2.5, "s": "4.25", "bad": "nan-ish", "nul": nil}
	if v, ok := r.Num("f"); !ok || v != 2.5 {
		t.Fatalf("Num(f) = %v (ok=%v), want 2.5", v, ok)
	}
	if v, ok := r.Num("s"); !ok || v != 4.25 {
		t.Fatalf("Num(s) = %v (ok=%v), want 4.25", v, ok)
	}
	if _, ok := r.Num("bad"); ok {
		t.Fatal("Num(bad) ok = true, want false")
	}
	if _, ok := r.Num("nul"); ok {
		t.Fatal("Num(nul) ok = true, want false")
	}
	if _, ok := r.Num("missing"); ok {
		t.Fatal("Num(missing) ok = true, want false")
	}
}

func TestQuoteString(t *testing.T) {
	cases := map[string]string{
		"plain":   "@'plain'",
		"o'brien": "@'o''brien'",
		"":        "@''",
		"a'b'c":   "@'a''b''c'",
		// Verbatim form: a trailing backslash stays literal and cannot escape the
		// closing quote (the regular-literal break-out this guards against).
		`node\`:   "@'node\\'",
		`a\' | x`: "@'a\\'' | x'",
	}
	for in, want := range cases {
		if got := QuoteString(in); got != want {
			t.Fatalf("QuoteString(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClientQueryNoCommand(t *testing.T) {
	// An unconfigured Client reports ErrNoQueryCommand so the portal can treat
	// the board as disabled.
	_, err := Client{}.Query(context.Background(), "GpuHealth()")
	if !errors.Is(err, ErrNoQueryCommand) {
		t.Fatalf("err = %v, want ErrNoQueryCommand", err)
	}
}

func TestClientQueryExpandsArgsAndParses(t *testing.T) {
	// Use a real shell-out to /bin/echo-like behavior via `cat`-of-arg is awkward;
	// instead use a tiny sh script that echoes canned JSON, exercising the full
	// Query path (arg expansion + stdout parse).
	c := Client{
		Command: "sh",
		Args:    []string{"-c", `printf '{"Columns":["gpu"],"Rows":[["0"],["1"]]}'`},
	}
	rows, err := c.Query(context.Background(), "GpuHealth()")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
}

func TestClientQueryStderrFolded(t *testing.T) {
	c := Client{
		Command: "sh",
		Args:    []string{"-c", `echo "boom" >&2; exit 3`},
	}
	_, err := c.Query(context.Background(), "GpuHealth()")
	if err == nil {
		t.Fatal("Query err = nil, want failure")
	}
	if got := err.Error(); !contains(got, "boom") {
		t.Fatalf("err = %q, want it to include stderr 'boom'", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
