// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package exptelemetry

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const JSONLImporterVersion = "tau.jsonl.import.v1"

var protectedJSONLTagKeys = map[string]struct{}{
	"jsonl.raw_key":       {},
	"jsonl.history_file":  {},
	"jsonl.history_path":  {},
	"jsonl.history_line":  {},
	"jsonl.importer":      {},
	"tau.metric.card":     {},
	"tau.metric.standard": {},
}

// JSONLHistoryOptions identifies and annotates scalar metrics projected from one history row.
type JSONLHistoryOptions struct {
	WorkspaceID    string
	Cluster        string
	Namespace      string
	SourceStoreID  string
	Project        string
	ExperimentID   string
	RunGroupID     string
	RunID          string
	MetricPrefix   string
	Source         string
	Tags           map[string]string
	StepField      string
	TimeField      string
	HistoryPath    string
	HistoryLine    int
	ExportedAt     time.Time
	MetricFileID   string
	MetricFilePath string
}

// ProjectJSONLHistoryRow applies the portal JSONL importer projection to one decoded object.
func ProjectJSONLHistoryRow(row map[string]any, opts JSONLHistoryOptions) ([]MetricEvent, error) {
	if opts.StepField == "" {
		opts.StepField = "_step"
	}
	if opts.TimeField == "" {
		opts.TimeField = "_timestamp"
	}
	if opts.Source == "" {
		opts.Source = "jsonl"
	}
	if opts.HistoryLine < 1 {
		return nil, fmt.Errorf("history line must be positive")
	}
	for key := range opts.Tags {
		if _, protected := protectedJSONLTagKeys[strings.TrimSpace(key)]; protected {
			return nil, fmt.Errorf("tag %q cannot override protected JSONL metadata", key)
		}
	}

	step, err := historyStep(row[opts.StepField])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opts.StepField, err)
	}
	wallTime, err := historyTime(row[opts.TimeField])
	if err != nil {
		return nil, fmt.Errorf("%s: %w", opts.TimeField, err)
	}

	keys := make([]string, 0, len(row))
	for key := range row {
		if key == opts.StepField || key == opts.TimeField || strings.HasPrefix(key, "_") {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	events := make([]MetricEvent, 0, len(keys))
	for _, rawKey := range keys {
		value, numeric, invalidNumber := historyNumber(row[rawKey])
		if invalidNumber {
			return nil, fmt.Errorf("metric %q must be a finite number", rawKey)
		}
		if !numeric {
			continue
		}
		metricName := strings.TrimSpace(rawKey)
		if metricName == "" {
			continue
		}
		if opts.MetricPrefix != "" {
			metricName = strings.TrimSuffix(opts.MetricPrefix, "/") + "/" + metricName
		}
		tags := compactMetricTags(opts.Tags)
		for key, tagValue := range map[string]string{
			"jsonl.raw_key":       rawKey,
			"jsonl.history_file":  filepath.Base(opts.HistoryPath),
			"jsonl.history_path":  opts.HistoryPath,
			"jsonl.history_line":  strconv.Itoa(opts.HistoryLine),
			"jsonl.importer":      JSONLImporterVersion,
			"tau.metric.card":     ResearchMetricCard(metricName),
			"tau.metric.standard": strconv.FormatBool(IsStandardResearchMetric(metricName)),
		} {
			tags[key] = tagValue
		}
		event, err := NewMetricEvent(MetricEvent{
			WorkspaceID:    opts.WorkspaceID,
			Cluster:        opts.Cluster,
			Namespace:      opts.Namespace,
			SourceStoreID:  opts.SourceStoreID,
			Project:        opts.Project,
			ExperimentID:   opts.ExperimentID,
			RunGroupID:     opts.RunGroupID,
			RunID:          opts.RunID,
			MetricName:     metricName,
			Step:           step,
			WallTime:       wallTime,
			Value:          value,
			Source:         opts.Source,
			MetricFileID:   opts.MetricFileID,
			MetricFilePath: opts.MetricFilePath,
			Tags:           tags,
			ExportedAt:     opts.ExportedAt,
		})
		if err != nil {
			return nil, fmt.Errorf("project metric %q: %w", rawKey, err)
		}
		events = append(events, event)
	}
	return events, nil
}

func historyStep(value any) (int64, error) {
	if number, ok := value.(json.Number); ok {
		step, err := strconv.ParseInt(number.String(), 10, 64)
		if err == nil {
			if step < 0 {
				return 0, fmt.Errorf("must be nonnegative")
			}
			return step, nil
		}
	}
	switch value := value.(type) {
	case int:
		return nonnegativeHistoryStep(int64(value))
	case int8:
		return nonnegativeHistoryStep(int64(value))
	case int16:
		return nonnegativeHistoryStep(int64(value))
	case int32:
		return nonnegativeHistoryStep(int64(value))
	case int64:
		return nonnegativeHistoryStep(value)
	case uint:
		if uint64(value) > math.MaxInt64 {
			return 0, fmt.Errorf("must be numeric and representable as int64")
		}
		return int64(value), nil
	case uint8:
		return int64(value), nil
	case uint16:
		return int64(value), nil
	case uint32:
		return int64(value), nil
	case uint64:
		if value > math.MaxInt64 {
			return 0, fmt.Errorf("must be numeric and representable as int64")
		}
		return int64(value), nil
	}
	number, numeric, invalid := historyNumber(value)
	if invalid || !numeric || number > math.MaxInt64 || number < math.MinInt64 {
		return 0, fmt.Errorf("must be numeric and representable as int64")
	}
	step := int64(number)
	if step < 0 {
		return 0, fmt.Errorf("must be nonnegative")
	}
	return step, nil
}

func nonnegativeHistoryStep(step int64) (int64, error) {
	if step < 0 {
		return 0, fmt.Errorf("must be nonnegative")
	}
	return step, nil
}

func historyTime(value any) (time.Time, error) {
	number, numeric, invalid := historyNumber(value)
	if invalid || !numeric {
		return time.Time{}, fmt.Errorf("must be finite epoch seconds")
	}
	return time.UnixMicro(int64(number * 1_000_000)).UTC(), nil
}

func historyNumber(value any) (float64, bool, bool) {
	var number float64
	switch value := value.(type) {
	case json.Number:
		parsed, err := value.Float64()
		if err != nil {
			return 0, false, true
		}
		number = parsed
	case float64:
		number = value
	case float32:
		number = float64(value)
	case int:
		number = float64(value)
	case int8:
		number = float64(value)
	case int16:
		number = float64(value)
	case int32:
		number = float64(value)
	case int64:
		number = float64(value)
	case uint:
		number = float64(value)
	case uint8:
		number = float64(value)
	case uint16:
		number = float64(value)
	case uint32:
		number = float64(value)
	case uint64:
		number = float64(value)
	default:
		return 0, false, false
	}
	if math.IsNaN(number) || math.IsInf(number, 0) {
		return 0, false, true
	}
	return number, true, false
}

func compactMetricTags(tags map[string]string) map[string]string {
	out := make(map[string]string, len(tags)+len(protectedJSONLTagKeys))
	for key, value := range tags {
		key = strings.TrimSpace(key)
		if key != "" {
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}
