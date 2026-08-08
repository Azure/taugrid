// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/parquet-go/parquet-go"
)

type MetricSummaryBackfillResult struct {
	Files     int      `json:"files"`
	Summaries int      `json:"summaries"`
	Warnings  []string `json:"warnings,omitempty"`
}

type metricSummaryOrder struct {
	step       *int64
	wallTime   *int64
	updatedAt  string
	fileID     string
	rowOrdinal int64
}

type metricSummaryBuilder struct {
	record MetricSummaryRecord
	order  metricSummaryOrder
}

func SummarizeMetricRows(metricFile MetricFileRecord, rows []MetricRow) []MetricSummaryRecord {
	if len(rows) == 0 {
		return nil
	}
	builders := map[string]*metricSummaryBuilder{}
	for i, row := range rows {
		metricName := strings.TrimSpace(row.MetricName)
		if metricName == "" {
			continue
		}
		builder := builders[metricName]
		if builder == nil {
			builder = &metricSummaryBuilder{
				record: MetricSummaryRecord{
					FileID:     metricFile.FileID,
					RunID:      firstNonEmpty(row.RunID, metricFile.RunID),
					Project:    firstNonEmpty(row.Project, metricFile.Project),
					RunGroupID: firstNonEmpty(row.RunGroupID, metricFile.RunGroupID),
					MetricName: metricName,
					UpdatedAt:  metricFile.CreatedAt,
				},
			}
			builders[metricName] = builder
		}
		builder.record.Count++
		updateStepRange(&builder.record.MinStep, &builder.record.MaxStep, row.Step)
		if math.IsNaN(row.Value) || math.IsInf(row.Value, 0) {
			builder.record.NonFiniteCount++
			continue
		}
		builder.record.FiniteCount++
		if builder.record.FiniteCount == 1 {
			builder.record.MinValue = row.Value
			builder.record.MaxValue = row.Value
		} else {
			if row.Value < builder.record.MinValue {
				builder.record.MinValue = row.Value
			}
			if row.Value > builder.record.MaxValue {
				builder.record.MaxValue = row.Value
			}
		}
		order := metricSummaryOrder{
			step:       cloneInt64(row.Step),
			wallTime:   cloneInt64(row.WallTime),
			updatedAt:  metricFile.CreatedAt,
			fileID:     metricFile.FileID,
			rowOrdinal: int64(i),
		}
		if builder.record.FiniteCount == 1 || metricSummaryOrderAfter(order, builder.order) {
			builder.order = order
			builder.record.LatestStep = cloneInt64(row.Step)
			builder.record.LatestWallTime = cloneInt64(row.WallTime)
			builder.record.LatestValue = row.Value
			builder.record.LatestFileID = metricFile.FileID
		}
	}
	names := make([]string, 0, len(builders))
	for name := range builders {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]MetricSummaryRecord, 0, len(names))
	for _, name := range names {
		out = append(out, builders[name].record)
	}
	return out
}

func (s *Store) EnsureMetricSummaries(ctx context.Context) (MetricSummaryBackfillResult, error) {
	var result MetricSummaryBackfillResult
	lock, err := acquireStoreWriteLock(ctx, s.Root)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			result.Warnings = append(result.Warnings, "metric summary backfill skipped because the experiment store writer lock is busy")
			return result, nil
		}
		return result, err
	}
	defer lock.release()
	files, err := s.metricFilesMissingSummaries(ctx)
	if err != nil {
		return result, err
	}
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if strings.ToLower(file.Format) != "parquet" {
			result.Warnings = append(result.Warnings, fmt.Sprintf("metric summary backfill skipped non-parquet metric file %s", file.Path))
			continue
		}
		path := filepath.Join(s.Root, filepath.FromSlash(file.Path))
		if _, err := os.Stat(path); err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("metric summary backfill skipped unreadable metric file %s: %v", file.Path, err))
			continue
		}
		rows, err := parquet.ReadFile[MetricRow](path)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("metric summary backfill skipped unparsable metric file %s: %v", file.Path, err))
			continue
		}
		summaries := SummarizeMetricRows(file, rows)
		created, err := s.ensureMetricSummaryRecords(ctx, summaries, []MetricFileRecord{file})
		if err != nil {
			return result, err
		}
		result.Files++
		result.Summaries += created
	}
	return result, nil
}

func (s *Store) metricFilesMissingSummaries(ctx context.Context) ([]MetricFileRecord, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT file_id, path, format, schema_version, coalesce(schema_hash, ''), coalesce(project, ''),
       coalesce(run_group_id, ''),
       coalesce(run_id, ''), row_count, coalesce(digest, ''), min_step, max_step, created_at
FROM metric_files
WHERE NOT EXISTS (
  SELECT 1 FROM metric_summary_files WHERE metric_summary_files.file_id = metric_files.file_id
)
ORDER BY created_at, file_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MetricFileRecord
	for rows.Next() {
		record, err := scanMetricFileRecord(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	return out, rows.Err()
}

func scanMetricFileRecord(scanner interface {
	Scan(dest ...any) error
}) (MetricFileRecord, error) {
	var record MetricFileRecord
	var minStep, maxStep sql.NullInt64
	err := scanner.Scan(
		&record.FileID,
		&record.Path,
		&record.Format,
		&record.SchemaVersion,
		&record.SchemaHash,
		&record.Project,
		&record.RunGroupID,
		&record.RunID,
		&record.RowCount,
		&record.Digest,
		&minStep,
		&maxStep,
		&record.CreatedAt,
	)
	if err != nil {
		return MetricFileRecord{}, err
	}
	if minStep.Valid {
		record.MinStep = &minStep.Int64
	}
	if maxStep.Valid {
		record.MaxStep = &maxStep.Int64
	}
	return record, nil
}

func (s *Store) ensureMetricSummaryRecords(ctx context.Context, summaries []MetricSummaryRecord, metricFiles []MetricFileRecord) (int, error) {
	if len(metricFiles) == 0 && len(summaries) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	created, err := ensureMetricSummaryRecordsTx(ctx, tx, summaries, metricFiles)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return created, nil
}

func metricSummaryFileProcessed(ctx context.Context, tx *sql.Tx, fileID string) (bool, error) {
	var existing string
	err := tx.QueryRowContext(ctx, "SELECT file_id FROM metric_summary_files WHERE file_id = ?", fileID).Scan(&existing)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func mergeMetricSummaryRecord(ctx context.Context, tx *sql.Tx, incoming MetricSummaryRecord) error {
	if strings.TrimSpace(incoming.RunID) == "" || strings.TrimSpace(incoming.MetricName) == "" {
		return fmt.Errorf("metric summary run_id and metric_name are required")
	}
	existing, ok, err := metricSummaryRecord(ctx, tx, incoming.RunID, incoming.MetricName)
	if err != nil {
		return err
	}
	merged := incoming
	if ok {
		merged = mergeMetricSummaries(existing, incoming)
	}
	_, err = tx.ExecContext(ctx, `
INSERT OR REPLACE INTO metric_summaries(
  run_id, metric_name, project, run_group_id, count, finite_count, non_finite_count,
  min_step, max_step, latest_step, latest_wall_time, latest_value, min_value, max_value,
  updated_at, latest_file_id
) VALUES (?, ?, nullif(?, ''), nullif(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, nullif(?, ''))`,
		merged.RunID,
		merged.MetricName,
		merged.Project,
		merged.RunGroupID,
		merged.Count,
		merged.FiniteCount,
		merged.NonFiniteCount,
		nullableInt64(merged.MinStep),
		nullableInt64(merged.MaxStep),
		nullableInt64(merged.LatestStep),
		nullableInt64(merged.LatestWallTime),
		nullableFloat64Value(merged.LatestValue, merged.FiniteCount > 0),
		nullableFloat64Value(merged.MinValue, merged.FiniteCount > 0),
		nullableFloat64Value(merged.MaxValue, merged.FiniteCount > 0),
		merged.UpdatedAt,
		merged.LatestFileID,
	)
	return err
}

func metricSummaryRecord(ctx context.Context, tx *sql.Tx, runID, metricName string) (MetricSummaryRecord, bool, error) {
	var summary MetricSummaryRecord
	var minStep, maxStep, latestStep, latestWallTime sql.NullInt64
	var latestValue, minValue, maxValue sql.NullFloat64
	err := tx.QueryRowContext(ctx, `
SELECT run_id, coalesce(project, ''), coalesce(run_group_id, ''), metric_name,
       count, finite_count, non_finite_count, min_step, max_step, latest_step, latest_wall_time,
       latest_value, min_value, max_value, updated_at, coalesce(latest_file_id, '')
FROM metric_summaries WHERE run_id = ? AND metric_name = ?`, runID, metricName).Scan(
		&summary.RunID,
		&summary.Project,
		&summary.RunGroupID,
		&summary.MetricName,
		&summary.Count,
		&summary.FiniteCount,
		&summary.NonFiniteCount,
		&minStep,
		&maxStep,
		&latestStep,
		&latestWallTime,
		&latestValue,
		&minValue,
		&maxValue,
		&summary.UpdatedAt,
		&summary.LatestFileID,
	)
	if err == sql.ErrNoRows {
		return MetricSummaryRecord{}, false, nil
	}
	if err != nil {
		return MetricSummaryRecord{}, false, err
	}
	applyMetricSummaryNulls(&summary, minStep, maxStep, latestStep, latestWallTime, latestValue, minValue, maxValue)
	return summary, true, nil
}

func mergeMetricSummaries(existing, incoming MetricSummaryRecord) MetricSummaryRecord {
	merged := existing
	if merged.Project == "" {
		merged.Project = incoming.Project
	}
	if merged.RunGroupID == "" {
		merged.RunGroupID = incoming.RunGroupID
	}
	merged.Count += incoming.Count
	merged.FiniteCount += incoming.FiniteCount
	merged.NonFiniteCount += incoming.NonFiniteCount
	merged.MinStep = minInt64Ptr(merged.MinStep, incoming.MinStep)
	merged.MaxStep = maxInt64Ptr(merged.MaxStep, incoming.MaxStep)
	if incoming.FiniteCount > 0 {
		if existing.FiniteCount == 0 {
			merged.LatestStep = cloneInt64(incoming.LatestStep)
			merged.LatestWallTime = cloneInt64(incoming.LatestWallTime)
			merged.LatestValue = incoming.LatestValue
			merged.MinValue = incoming.MinValue
			merged.MaxValue = incoming.MaxValue
			merged.LatestFileID = incoming.LatestFileID
		} else {
			if incoming.MinValue < merged.MinValue {
				merged.MinValue = incoming.MinValue
			}
			if incoming.MaxValue > merged.MaxValue {
				merged.MaxValue = incoming.MaxValue
			}
			if metricSummaryRecordAfter(incoming, existing) {
				merged.LatestStep = cloneInt64(incoming.LatestStep)
				merged.LatestWallTime = cloneInt64(incoming.LatestWallTime)
				merged.LatestValue = incoming.LatestValue
				merged.LatestFileID = incoming.LatestFileID
			}
		}
	}
	if incoming.UpdatedAt > merged.UpdatedAt {
		merged.UpdatedAt = incoming.UpdatedAt
	}
	return merged
}

func metricSummaryRecordAfter(left, right MetricSummaryRecord) bool {
	return metricSummaryOrderAfter(
		metricSummaryOrder{step: left.LatestStep, wallTime: left.LatestWallTime, updatedAt: left.UpdatedAt, fileID: left.LatestFileID},
		metricSummaryOrder{step: right.LatestStep, wallTime: right.LatestWallTime, updatedAt: right.UpdatedAt, fileID: right.LatestFileID},
	)
}

func metricSummaryOrderAfter(left, right metricSummaryOrder) bool {
	switch {
	case left.step != nil && right.step != nil:
		if *left.step != *right.step {
			return *left.step > *right.step
		}
	case left.step != nil:
		return true
	case right.step != nil:
		return false
	}
	switch {
	case left.wallTime != nil && right.wallTime != nil:
		if *left.wallTime != *right.wallTime {
			return *left.wallTime > *right.wallTime
		}
	case left.wallTime != nil:
		return true
	case right.wallTime != nil:
		return false
	}
	if left.updatedAt != right.updatedAt {
		return left.updatedAt > right.updatedAt
	}
	if left.fileID != right.fileID {
		return left.fileID > right.fileID
	}
	return left.rowOrdinal > right.rowOrdinal
}

func updateStepRange(minStep, maxStep **int64, step *int64) {
	if step == nil {
		return
	}
	if *minStep == nil || *step < **minStep {
		*minStep = cloneInt64(step)
	}
	if *maxStep == nil || *step > **maxStep {
		*maxStep = cloneInt64(step)
	}
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func minInt64Ptr(left, right *int64) *int64 {
	if left == nil {
		return cloneInt64(right)
	}
	if right == nil || *left <= *right {
		return cloneInt64(left)
	}
	return cloneInt64(right)
}

func maxInt64Ptr(left, right *int64) *int64 {
	if left == nil {
		return cloneInt64(right)
	}
	if right == nil || *left >= *right {
		return cloneInt64(left)
	}
	return cloneInt64(right)
}

func nullableFloat64Value(value float64, valid bool) any {
	if !valid {
		return nil
	}
	return value
}
