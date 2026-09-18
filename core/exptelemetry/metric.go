// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package exptelemetry

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"
)

const (
	MetricEventSchemaV1 = "tau.experiment.metric.v1"
	MetricSchemaV1      = MetricEventSchemaV1
)

var protectedMetricTagKeys = map[string]struct{}{
	"schema_version":   {},
	"event_id":         {},
	"workspace_id":     {},
	"cluster":          {},
	"namespace":        {},
	"source_store_id":  {},
	"project":          {},
	"experiment_id":    {},
	"run_group_id":     {},
	"run_id":           {},
	"metric_name":      {},
	"step":             {},
	"wall_time":        {},
	"value":            {},
	"unit":             {},
	"source":           {},
	"split":            {},
	"metric_file_id":   {},
	"metric_file_path": {},
	"exported_at":      {},
}

// MetricEvent is the canonical typed metric point exchanged by Tau components.
type MetricEvent struct {
	SchemaVersion  string            `json:"schema_version"`
	EventID        string            `json:"event_id"`
	WorkspaceID    string            `json:"workspace_id,omitempty"`
	Cluster        string            `json:"cluster,omitempty"`
	Namespace      string            `json:"namespace,omitempty"`
	SourceStoreID  string            `json:"source_store_id,omitempty"`
	Project        string            `json:"project"`
	ExperimentID   string            `json:"experiment_id"`
	RunGroupID     string            `json:"run_group_id"`
	RunID          string            `json:"run_id"`
	MetricName     string            `json:"metric_name"`
	Step           int64             `json:"step"`
	WallTime       time.Time         `json:"wall_time"`
	Value          float64           `json:"value"`
	Unit           string            `json:"unit,omitempty"`
	Source         string            `json:"source,omitempty"`
	Split          string            `json:"split,omitempty"`
	MetricFileID   string            `json:"metric_file_id,omitempty"`
	MetricFilePath string            `json:"metric_file_path,omitempty"`
	Tags           map[string]string `json:"tags,omitempty"`
	ExportedAt     time.Time         `json:"exported_at"`
}

// NewMetricEvent applies the v1 schema and deterministic event identity.
func NewMetricEvent(event MetricEvent) (MetricEvent, error) {
	if event.SchemaVersion == "" {
		event.SchemaVersion = MetricEventSchemaV1
	}
	if event.EventID == "" {
		event.EventID = event.DeterministicEventID()
	}
	if err := event.Validate(); err != nil {
		return MetricEvent{}, err
	}
	return event, nil
}

// ComputeEventID is an alias for DeterministicEventID.
func (e MetricEvent) ComputeEventID() string {
	return e.DeterministicEventID()
}

// DeterministicEventID returns a SHA-256 identity for the raw chart point.
// Export time and tags are deliberately excluded because they are delivery metadata.
func (e MetricEvent) DeterministicEventID() string {
	identity := struct {
		WorkspaceID   string    `json:"workspace_id"`
		Cluster       string    `json:"cluster"`
		Namespace     string    `json:"namespace"`
		SourceStoreID string    `json:"source_store_id"`
		MetricFileID  string    `json:"metric_file_id"`
		Project       string    `json:"project"`
		ExperimentID  string    `json:"experiment_id"`
		RunGroupID    string    `json:"run_group_id"`
		RunID         string    `json:"run_id"`
		MetricName    string    `json:"metric_name"`
		Step          int64     `json:"step"`
		WallTime      time.Time `json:"wall_time"`
	}{
		WorkspaceID:   e.WorkspaceID,
		Cluster:       e.Cluster,
		Namespace:     e.Namespace,
		SourceStoreID: e.SourceStoreID,
		MetricFileID:  e.MetricFileID,
		Project:       e.Project,
		ExperimentID:  e.ExperimentID,
		RunGroupID:    e.RunGroupID,
		RunID:         e.RunID,
		MetricName:    e.MetricName,
		Step:          e.Step,
		WallTime:      e.WallTime.UTC(),
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	return "metric-" + hex.EncodeToString(sum[:])
}

// Validate verifies the v1 metric contract.
func (e MetricEvent) Validate() error {
	if e.SchemaVersion != MetricEventSchemaV1 {
		return fmt.Errorf("unknown metric schema %q", e.SchemaVersion)
	}
	for name, value := range map[string]string{
		"project":       e.Project,
		"experiment_id": e.ExperimentID,
		"run_group_id":  e.RunGroupID,
		"run_id":        e.RunID,
		"metric_name":   e.MetricName,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s is required", name)
		}
	}
	if e.Step < 0 {
		return fmt.Errorf("step must be nonnegative")
	}
	if err := validateMetricTime("wall_time", e.WallTime); err != nil {
		return err
	}
	if err := validateMetricTime("exported_at", e.ExportedAt); err != nil {
		return err
	}
	if math.IsNaN(e.Value) || math.IsInf(e.Value, 0) {
		return fmt.Errorf("value must be finite")
	}
	for key := range e.Tags {
		if _, protected := protectedMetricTagKeys[strings.TrimSpace(key)]; protected {
			return fmt.Errorf("tag %q cannot override a protected metric field", key)
		}
	}
	if e.EventID == "" {
		return fmt.Errorf("event_id is required")
	}
	if want := e.DeterministicEventID(); e.EventID != want {
		return fmt.Errorf("event_id %q does not match deterministic identity %q", e.EventID, want)
	}
	return nil
}

func validateMetricTime(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is required", name)
	}
	if _, err := value.MarshalJSON(); err != nil {
		return fmt.Errorf("%s is invalid: %w", name, err)
	}
	return nil
}

// MarshalNDJSON returns the event's canonical one-line NDJSON representation.
func (e MetricEvent) MarshalNDJSON() ([]byte, error) {
	return EncodeMetricNDJSON(e)
}

// MarshalMetricEventNDJSON returns the event's canonical one-line NDJSON representation.
func MarshalMetricEventNDJSON(event MetricEvent) ([]byte, error) {
	return EncodeMetricNDJSON(event)
}

// EncodeMetricNDJSON returns one canonical JSON object followed by one newline.
func EncodeMetricNDJSON(event MetricEvent) ([]byte, error) {
	normalized, err := NewMetricEvent(event)
	if err != nil {
		return nil, err
	}
	normalized.WallTime = normalized.WallTime.UTC()
	normalized.ExportedAt = normalized.ExportedAt.UTC()

	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(normalized); err != nil {
		return nil, fmt.Errorf("encode metric event: %w", err)
	}
	return out.Bytes(), nil
}
