// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jsonlutil

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestReadFileCheckpointSetMissingFileInitializesSchemaAndFiles(t *testing.T) {
	checkpoint, err := ReadFileCheckpointSet(filepath.Join(t.TempDir(), "missing.json"), "tau.test.checkpoint.v1")
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.SchemaVersion != "tau.test.checkpoint.v1" {
		t.Fatalf("schema version = %q, want tau.test.checkpoint.v1", checkpoint.SchemaVersion)
	}
	if checkpoint.Files == nil {
		t.Fatal("files map is nil")
	}
	if len(checkpoint.Files) != 0 {
		t.Fatalf("files length = %d, want 0", len(checkpoint.Files))
	}
}

func TestWriteFileCheckpointSetPersistsVersionedAtomicJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "checkpoint.json")
	checkpoint := FileCheckpointSet{
		Files: map[string]FileCheckpoint{
			"/tmp/history.jsonl": {
				Path:         "/tmp/history.jsonl",
				Offset:       42,
				SizeBytes:    128,
				ModTime:      "2026-06-05T00:00:00Z",
				PrefixSHA256: "abc123",
				UpdatedAt:    "2026-06-05T00:00:01Z",
			},
		},
	}
	if err := WriteFileCheckpointSet(path, "tau.test.checkpoint.v1", checkpoint); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if raw[len(raw)-1] != '\n' {
		t.Fatalf("checkpoint file does not end with newline: %q", raw)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatalf("parse checkpoint JSON: %v\n%s", err, raw)
	}
	if len(top) != 3 {
		t.Fatalf("top-level field count = %d, want 3 in %s", len(top), raw)
	}
	var schema string
	if err := json.Unmarshal(top["schema_version"], &schema); err != nil {
		t.Fatal(err)
	}
	if schema != "tau.test.checkpoint.v1" {
		t.Fatalf("schema version = %q, want tau.test.checkpoint.v1", schema)
	}
	var files map[string]FileCheckpoint
	if err := json.Unmarshal(top["files"], &files); err != nil {
		t.Fatal(err)
	}
	entry, ok := files["/tmp/history.jsonl"]
	if !ok {
		t.Fatalf("checkpoint missing /tmp/history.jsonl entry: %+v", files)
	}
	if entry.Offset != 42 || entry.Path != "/tmp/history.jsonl" || entry.PrefixSHA256 != "abc123" {
		t.Fatalf("unexpected checkpoint entry: %+v", entry)
	}
	var updatedAt string
	if err := json.Unmarshal(top["updated_at"], &updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt == "" {
		t.Fatal("updated_at is empty")
	}
}

func TestInitializeFileCheckpointSetAtEndRejectsCorruptExistingState(t *testing.T) {
	dir := t.TempDir()
	checkpointPath := filepath.Join(dir, "checkpoint.json")
	if err := os.WriteFile(checkpointPath, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := InitializeFileCheckpointSetAtEnd(
		checkpointPath,
		"tau.test.checkpoint.v1",
		[]string{filepath.Join(dir, "metrics-history.jsonl")},
	)
	if err == nil {
		t.Fatal("corrupt existing checkpoint was accepted")
	}
	if created {
		t.Fatal("corrupt existing checkpoint was replaced")
	}
}
