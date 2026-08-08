// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jsonlutil

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
)

// FileCheckpointSet stores per-file offsets for newline-delimited JSON inputs.
type FileCheckpointSet struct {
	SchemaVersion string                    `json:"schema_version"`
	Files         map[string]FileCheckpoint `json:"files"`
	UpdatedAt     string                    `json:"updated_at"`
}

// ReadFileCheckpointSet reads a versioned JSONL checkpoint set. Missing files
// initialize an empty checkpoint with the caller's schema version.
func ReadFileCheckpointSet(path, schemaVersion string) (FileCheckpointSet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return FileCheckpointSet{
				SchemaVersion: schemaVersion,
				Files:         map[string]FileCheckpoint{},
			}, nil
		}
		return FileCheckpointSet{}, err
	}
	var checkpoint FileCheckpointSet
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return FileCheckpointSet{}, fmt.Errorf("read JSONL checkpoint %s: %w", path, err)
	}
	if checkpoint.Files == nil {
		checkpoint.Files = map[string]FileCheckpoint{}
	}
	return checkpoint, nil
}

// WriteFileCheckpointSet writes a versioned JSONL checkpoint set atomically.
func WriteFileCheckpointSet(path, schemaVersion string, checkpoint FileCheckpointSet) error {
	if schemaVersion == "" {
		return fmt.Errorf("JSONL checkpoint schema version is required")
	}
	checkpoint.SchemaVersion = schemaVersion
	checkpoint.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if checkpoint.Files == nil {
		checkpoint.Files = map[string]FileCheckpoint{}
	}
	return fileutil.WriteJSONFileAtomic(path, checkpoint)
}

// InitializeFileCheckpointSetAtEnd creates a checkpoint set at the current end
// of every matching file. Existing checkpoint sets are left unchanged so a
// retry of the same consumer session resumes rather than re-baselining.
func InitializeFileCheckpointSetAtEnd(path, schemaVersion string, inputs []string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		existing, err := ReadFileCheckpointSet(path, schemaVersion)
		if err != nil {
			return false, err
		}
		if existing.SchemaVersion != schemaVersion {
			return false, fmt.Errorf(
				"JSONL checkpoint %s uses schema %q, expected %q",
				path,
				existing.SchemaVersion,
				schemaVersion,
			)
		}
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	files, err := ExpandInputs(inputs)
	if err != nil {
		return false, err
	}
	checkpoint := FileCheckpointSet{Files: make(map[string]FileCheckpoint, len(files))}
	for _, file := range files {
		current, err := CurrentFileCheckpoint(file)
		if err != nil {
			return false, err
		}
		checkpoint.Files[file] = current
	}
	if err := WriteFileCheckpointSet(path, schemaVersion, checkpoint); err != nil {
		return false, err
	}
	return true, nil
}
