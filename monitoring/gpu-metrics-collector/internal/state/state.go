// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package state

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"
)

// Sample is a timestamped metric value for JSON serialization.
type Sample struct {
	Time  time.Time `json:"t"`
	Value float64   `json:"v"`
}

// Availability captures one required scrape target's debounce state.
//
// Established records whether the writing process had proven the target's state
// and was therefore publishing this condition. A snapshot written before this
// field existed decodes as false, which is the conservative reading: the
// restored process stays silent until it proves the state itself.
type Availability struct {
	FailingSince time.Time `json:"failingSince,omitempty"`
	HealthySince time.Time `json:"healthySince,omitempty"`
	Firing       bool      `json:"firing,omitempty"`
	Established  bool      `json:"established,omitempty"`
}

// Snapshot captures all in-memory state for persistence across restarts.
type Snapshot struct {
	History      map[string][]Sample     `json:"history"`
	Pending      map[string]time.Time    `json:"pending"`
	LastStatus   map[string]string       `json:"lastStatus"`
	Availability map[string]Availability `json:"availability,omitempty"`
	SavedAt      time.Time               `json:"savedAt"`
}

const stateFile = "state.json"

// Save writes the snapshot to disk atomically (write tmp → rename).
func Save(dir string, snap *Snapshot) error {
	snap.SavedAt = time.Now()
	data, err := json.Marshal(snap)
	if err != nil {
		return fmt.Errorf("marshaling state: %w", err)
	}

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating state dir: %w", err)
	}

	tmp := filepath.Join(dir, stateFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("writing tmp state: %w", err)
	}

	final := filepath.Join(dir, stateFile)
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("renaming state file: %w", err)
	}

	return nil
}

// Load reads the snapshot from disk. Returns nil (no error) if the file
// doesn't exist — the collector will start fresh.
func Load(dir string, maxAge time.Duration) (*Snapshot, error) {
	path := filepath.Join(dir, stateFile)
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		slog.Info("no previous state found, starting fresh", "path", path)
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading state file: %w", err)
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		slog.Warn("corrupt state file, starting fresh", "error", err)
		return nil, nil
	}

	age := time.Since(snap.SavedAt)
	if age > maxAge {
		slog.Info("state file too old, starting fresh", "age", age.String(), "maxAge", maxAge.String())
		return nil, nil
	}

	slog.Info("restored state from disk", "age", age.String(), "historyKeys", len(snap.History), "pending", len(snap.Pending))
	return &snap, nil
}
