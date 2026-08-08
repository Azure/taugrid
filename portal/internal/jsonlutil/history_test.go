// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jsonlutil

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadHistoryChunkOnlyCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	firstLine := `{"_timestamp":1.5,"loss":0.1}` + "\n"
	if err := os.WriteFile(path, []byte(firstLine+`{"_timestamp":2.0,"loss":`), 0o644); err != nil {
		t.Fatal(err)
	}
	chunk, err := ReadHistoryChunk(path, FileCheckpoint{})
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk.Data) != firstLine {
		t.Fatalf("chunk data = %q, want %q", chunk.Data, firstLine)
	}
	if chunk.EndOffset != int64(len(firstLine)) {
		t.Fatalf("end offset = %d, want %d", chunk.EndOffset, len(firstLine))
	}
	wantExportedAt := time.UnixMicro(1_500_000).UTC()
	if !chunk.ExportedAt.Equal(wantExportedAt) {
		t.Fatalf("exportedAt = %s, want %s", chunk.ExportedAt, wantExportedAt)
	}
}

func TestReadFinalHistoryChunkIncludesUnterminatedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	raw := []byte(`{"_step":1,"_timestamp":1770000000,"loss":0.1}`)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	chunk, err := ReadFinalHistoryChunk(path, FileCheckpoint{})
	if err != nil {
		t.Fatal(err)
	}
	if string(chunk.Data) != string(raw) {
		t.Fatalf("final chunk data = %q, want %q", chunk.Data, raw)
	}
	if chunk.EndOffset != int64(len(raw)) {
		t.Fatalf("final end offset = %d, want %d", chunk.EndOffset, len(raw))
	}
}

func TestHistoryCheckpointAndReadSyncSourceBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.jsonl")
	if err := os.WriteFile(path, []byte(`{"loss":0.1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	checkpointSynced := false
	if _, err := currentFileCheckpoint(path, func(*os.File) error {
		checkpointSynced = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !checkpointSynced {
		t.Fatal("fresh checkpoint did not sync its source")
	}

	readSynced := false
	if _, err := readHistoryChunk(path, FileCheckpoint{}, func(*os.File) error {
		readSynced = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !readSynced {
		t.Fatal("history read did not sync its source")
	}

	syncErr := errors.New("sync failed")
	if _, err := readHistoryChunk(path, FileCheckpoint{}, func(*os.File) error {
		return syncErr
	}); !errors.Is(err, syncErr) {
		t.Fatalf("error = %v, want %v", err, syncErr)
	}
}

func TestWriteHistoryChunkUsesStableKey(t *testing.T) {
	chunk := HistoryChunk{
		Path:        "/tmp/history.jsonl",
		Data:        []byte(`{"loss":0.1}` + "\n"),
		StartOffset: 0,
		EndOffset:   13,
		ExportedAt:  time.Unix(10, 0).UTC(),
	}
	path, key, err := WriteHistoryChunk(t.TempDir(), "run/id", chunk)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(path, key+".jsonl") {
		t.Fatalf("path %q does not end with chunk key %q", path, key)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != string(chunk.Data) {
		t.Fatalf("written chunk = %q, want %q", raw, chunk.Data)
	}
}

func TestInitializeFileCheckpointSetAtEndTracksPreexistingFileAfterRename(t *testing.T) {
	dir := t.TempDir()
	history := filepath.Join(dir, "metrics-history-attempt-0.jsonl")
	old := []byte(`{"_step":99,"loss":9.9}` + "\n")
	if err := os.WriteFile(history, old, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpointPath := filepath.Join(dir, "state", "checkpoint.json")
	created, err := InitializeFileCheckpointSetAtEnd(
		checkpointPath,
		"test.checkpoint.v1",
		[]string{filepath.Join(dir, "metrics-history-*.jsonl")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !created {
		t.Fatal("fresh session did not create its initial checkpoint")
	}
	checkpoints, err := ReadFileCheckpointSet(checkpointPath, "test.checkpoint.v1")
	if err != nil {
		t.Fatal(err)
	}
	if got := checkpoints.Files[history].Offset; got != int64(len(old)) {
		t.Fatalf("initial offset = %d, want %d", got, len(old))
	}

	archived := filepath.Join(dir, "metrics-history-archived.jsonl")
	if err := os.Rename(history, archived); err != nil {
		t.Fatal(err)
	}
	resolved, found, err := ResolveFileCheckpoint(archived, checkpoints.Files)
	if err != nil {
		t.Fatal(err)
	}
	if !found || resolved.Offset != int64(len(old)) {
		t.Fatalf("renamed preexisting file checkpoint = %+v, found=%v", resolved, found)
	}

	created, err = InitializeFileCheckpointSetAtEnd(
		checkpointPath,
		"test.checkpoint.v1",
		[]string{filepath.Join(dir, "metrics-history-*.jsonl")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if created {
		t.Fatal("retry replaced the existing session checkpoint")
	}
}

func TestResolveFileCheckpointPreservesOffsetAcrossSamePathIdentityChange(t *testing.T) {
	dir := t.TempDir()
	history := filepath.Join(dir, "metrics-history.jsonl")
	old := []byte(`{"_step":1,"loss":1.0}` + "\n")
	appended := []byte(`{"_step":2,"loss":0.5}` + "\n")
	if err := os.WriteFile(history, old, 0o644); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := CurrentFileCheckpoint(history)
	if err != nil {
		t.Fatal(err)
	}
	checkpoint.Device++
	checkpoint.Inode++
	if err := os.WriteFile(history, append(old, appended...), 0o644); err != nil {
		t.Fatal(err)
	}

	resolved, found, err := ResolveFileCheckpoint(history, map[string]FileCheckpoint{
		history: checkpoint,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("same-path checkpoint was not resolved by its consumed prefix")
	}
	if resolved.Device == checkpoint.Device && resolved.Inode == checkpoint.Inode {
		t.Fatal("resolved checkpoint kept the stale mount identity")
	}
	chunk, err := ReadHistoryChunk(history, resolved)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(chunk.Data); got != string(appended) {
		t.Fatalf("chunk data = %q, want only appended row %q", got, appended)
	}
}
