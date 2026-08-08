// Package kustoquery is the portal's generic Kusto access seam for the boards
// that read the Metrics ADX database (Cluster Health, Cost).
//
// Unlike internal/expcockpit's KustoSource — which parses into the fixed
// KustoMetricRow schema for Stellar — these boards run ad-hoc analytical KQL
// (GpuHealth(), GpuHours()) whose result columns vary per query. So the parser
// here returns generic tabular Rows ([]map[string]any) and the executor reuses
// Stellar's exact shell-out contract: a --kusto-query-command with {endpoint},
// {database}, and {query} placeholders (KQL on stdin when {query} is absent).
// Keeping the same contract means the portal needs no new auth surface and no
// Kusto SDK dependency.
package kustoquery

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/expkusto"
)

// ErrNoQueryCommand is returned when a Client has no command configured. The
// portal only builds a Client when --kusto-query-command is set, so a board
// backed by an unconfigured Client is treated as disabled.
var ErrNoQueryCommand = errors.New("no kusto query command configured")

// Querier runs a KQL string and returns the primary result table as rows. The
// cluster and cost boards depend on this interface so tests can inject a fake
// with no live Kusto. Client is the production implementation.
type Querier interface {
	Query(ctx context.Context, kql string) ([]Row, error)
}

// Row is one result row keyed by column name. Values keep their JSON-native
// types (float64 for numbers, string for text); use Str/Num for typed access.
type Row map[string]any

// Str returns the column as a string ("" when absent/null; non-strings are
// formatted with fmt.Sprint).
func (r Row) Str(col string) string {
	switch v := r[col].(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return fmt.Sprint(v)
	}
}

// Num returns the column as a float64 and whether it parsed. It accepts JSON
// numbers, json.Number, and numeric strings (Kusto's tostring()).
func (r Row) Num(col string) (float64, bool) {
	switch v := r[col].(type) {
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

// Client executes KQL through an external command, matching Stellar's
// --kusto-query-command contract. The zero endpoint/database fall back to the
// Metrics ADX defaults (expkusto.DefaultEndpoint/DefaultDatabase), which is
// where GpuHealth() and GpuHours() live.
type Client struct {
	Command  string
	Args     []string
	Endpoint string
	Database string
}

// Query runs kql via the configured command and parses its output. KQL is
// passed on stdin unless an arg contains {query}. stderr is folded into the
// error so misconfigured commands are diagnosable.
func (c Client) Query(ctx context.Context, kql string) ([]Row, error) {
	if strings.TrimSpace(c.Command) == "" {
		return nil, ErrNoQueryCommand
	}
	endpoint := firstNonEmpty(c.Endpoint, expkusto.DefaultEndpoint)
	database := firstNonEmpty(c.Database, expkusto.DefaultDatabase)
	args, queryInArgs := expandArgs(c.Args, endpoint, database, kql)
	var stdin io.Reader
	if !queryInArgs {
		stdin = strings.NewReader(kql)
	}
	out, stderr, err := RunCommand(ctx, c.Command, args, stdin)
	if err != nil {
		if stderr != "" {
			return nil, fmt.Errorf("execute kusto query command: %w: %s", err, stderr)
		}
		return nil, fmt.Errorf("execute kusto query command: %w", err)
	}
	rows, err := ParseRows(out)
	if err != nil {
		return nil, fmt.Errorf("parse kusto query output: %w", err)
	}
	return rows, nil
}

// expandArgs substitutes {endpoint}, {database}, and {query} placeholders and
// reports whether {query} appeared (so the caller can skip stdin).
func expandArgs(args []string, endpoint, database, query string) ([]string, bool) {
	out := make([]string, 0, len(args))
	queryInArgs := false
	for _, arg := range args {
		replaced := strings.ReplaceAll(arg, "{endpoint}", endpoint)
		replaced = strings.ReplaceAll(replaced, "{database}", database)
		if strings.Contains(replaced, "{query}") {
			queryInArgs = true
			replaced = strings.ReplaceAll(replaced, "{query}", query)
		}
		out = append(out, replaced)
	}
	return out, queryInArgs
}

// QuoteString renders s as a KQL verbatim string literal (@'...'), doubling
// embedded quotes. Boards use it to interpolate user-facing filters safely.
//
// The verbatim form is deliberate: in a regular Kusto literal ('...') a
// backslash is an escape character, so quote-doubling alone leaves a
// break-out — a value ending in a backslash (e.g. `node\`) escapes the closing
// quote and lets the trailing KQL run as code. In a verbatim literal a
// backslash is a literal character and the only escape is a doubled quote
// (”), so doubling quotes is provably sufficient to keep s inside the string.
func QuoteString(s string) string {
	return "@'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// ParseRows parses Kusto command output into generic rows. It accepts the same
// shapes Stellar's parser handles, but column-agnostic: Kusto v1 REST
// ({"Tables":[...]}), v2 REST (a frame array), a {"Columns","Rows"} object, a
// plain array of row objects, and JSONL.
func ParseRows(raw []byte) ([]Row, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, nil
	}
	switch raw[0] {
	case '[':
		if rows, ok, err := parseFrameArray(raw); ok || err != nil {
			return rows, err
		}
		var rows []Row
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, err
		}
		return rows, nil
	case '{':
		if json.Valid(raw) {
			return parseObject(raw)
		}
	}
	return parseJSONL(raw)
}

func parseObject(raw []byte) ([]Row, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, err
	}
	for _, key := range []string{"Tables", "tables"} {
		if value, ok := obj[key]; ok {
			return parseTables(value)
		}
	}
	for _, pair := range []struct{ rowsKey, columnsKey string }{
		{rowsKey: "Rows", columnsKey: "Columns"},
		{rowsKey: "rows", columnsKey: "columns"},
	} {
		rowsRaw, ok := obj[pair.rowsKey]
		if !ok {
			continue
		}
		if columnsRaw, hasColumns := obj[pair.columnsKey]; hasColumns {
			columns, err := parseColumns(columnsRaw)
			if err != nil {
				return nil, err
			}
			var rawRows []json.RawMessage
			if err := json.Unmarshal(rowsRaw, &rawRows); err != nil {
				return nil, err
			}
			return parseTableRows(columns, rawRows)
		}
		var rows []Row
		if err := json.Unmarshal(rowsRaw, &rows); err != nil {
			return nil, err
		}
		return rows, nil
	}
	var row Row
	if err := json.Unmarshal(raw, &row); err != nil {
		return nil, err
	}
	return []Row{row}, nil
}

func parseTables(raw json.RawMessage) ([]Row, error) {
	var tables []struct {
		TableName string            `json:"TableName"`
		Columns   []columnSpec      `json:"Columns"`
		Rows      []json.RawMessage `json:"Rows"`
	}
	if err := json.Unmarshal(raw, &tables); err != nil {
		return nil, err
	}
	if len(tables) == 0 {
		return nil, nil
	}
	for _, table := range tables {
		switch table.TableName {
		case "", "PrimaryResult", "Table_0":
			return parseTableRows(table.Columns, table.Rows)
		}
	}
	return parseTableRows(tables[0].Columns, tables[0].Rows)
}

func parseFrameArray(raw []byte) ([]Row, bool, error) {
	var frames []struct {
		FrameType string            `json:"FrameType"`
		TableKind string            `json:"TableKind"`
		TableId   *int              `json:"TableId"`
		Columns   []columnSpec      `json:"Columns"`
		Rows      []json.RawMessage `json:"Rows"`
	}
	if err := json.Unmarshal(raw, &frames); err != nil {
		return nil, false, nil
	}
	if len(frames) == 0 || frames[0].FrameType == "" {
		return nil, false, nil
	}
	// Single-shot v2 REST: the PrimaryResult arrives as a whole DataTable frame.
	for _, frame := range frames {
		if frame.FrameType != "DataTable" {
			continue
		}
		if frame.TableKind != "" && frame.TableKind != "PrimaryResult" {
			continue
		}
		rows, err := parseTableRows(frame.Columns, frame.Rows)
		return rows, true, err
	}
	// Fragmented v2 REST stream: the PrimaryResult table is split across a
	// TableHeader (carries Columns + TableId) and one or more TableFragment
	// frames (carry Rows for a matching TableId). This is what the real
	// azure-kusto-go QueryToJson emits (IsFragmented=true); the DataTable branch
	// above only ever sees the QueryProperties/QueryCompletionInformation tables.
	for i, frame := range frames {
		if frame.FrameType != "TableHeader" {
			continue
		}
		if frame.TableKind != "" && frame.TableKind != "PrimaryResult" {
			continue
		}
		var rawRows []json.RawMessage
		for _, next := range frames[i+1:] {
			if next.FrameType == "TableFragment" && sameTableID(next.TableId, frame.TableId) {
				rawRows = append(rawRows, next.Rows...)
				continue
			}
			if next.FrameType == "TableCompletion" && sameTableID(next.TableId, frame.TableId) {
				break
			}
		}
		rows, err := parseTableRows(frame.Columns, rawRows)
		return rows, true, err
	}
	return nil, true, nil
}

func sameTableID(a, b *int) bool {
	if a == nil || b == nil {
		return true
	}
	return *a == *b
}

type columnSpec struct {
	ColumnName string `json:"ColumnName"`
	Name       string `json:"name"`
}

func (c columnSpec) name() string {
	if c.ColumnName != "" {
		return c.ColumnName
	}
	return c.Name
}

func parseColumns(raw json.RawMessage) ([]columnSpec, error) {
	var specs []columnSpec
	if err := json.Unmarshal(raw, &specs); err == nil {
		for _, spec := range specs {
			if spec.name() != "" {
				return specs, nil
			}
		}
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err != nil {
		return nil, err
	}
	specs = make([]columnSpec, 0, len(names))
	for _, name := range names {
		specs = append(specs, columnSpec{ColumnName: name})
	}
	return specs, nil
}

func parseTableRows(columns []columnSpec, rawRows []json.RawMessage) ([]Row, error) {
	names := make([]string, len(columns))
	for i, column := range columns {
		names[i] = column.name()
	}
	rows := make([]Row, 0, len(rawRows))
	for _, raw := range rawRows {
		raw = bytes.TrimSpace(raw)
		if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
			continue
		}
		if raw[0] == '{' {
			var row Row
			if err := json.Unmarshal(raw, &row); err != nil {
				return nil, err
			}
			rows = append(rows, row)
			continue
		}
		var values []any
		if err := json.Unmarshal(raw, &values); err != nil {
			return nil, err
		}
		if len(values) > len(names) {
			return nil, fmt.Errorf("kusto row has %d values but only %d columns", len(values), len(names))
		}
		row := make(Row, len(values))
		for i, value := range values {
			if i >= len(names) || names[i] == "" {
				continue
			}
			row[names[i]] = value
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func parseJSONL(raw []byte) ([]Row, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	rows := []Row{}
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var row Row
		if err := json.Unmarshal(line, &row); err != nil {
			return nil, err
		}
		rows = append(rows, row)
	}
	return rows, scanner.Err()
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
