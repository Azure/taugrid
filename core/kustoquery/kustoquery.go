// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

var ErrNoQueryCommand = errors.New("no kusto query command configured")

type Querier interface {
	Query(ctx context.Context, kql string) ([]Row, error)
}

type Row map[string]any

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

type Client struct {
	Command  string
	Args     []string
	Endpoint string
	Database string
}

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

func QuoteString(s string) string {
	return "@'" + strings.ReplaceAll(s, "'", "''") + "'"
}

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
