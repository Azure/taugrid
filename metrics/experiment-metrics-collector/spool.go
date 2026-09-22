// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/fileutil"
)

type checkpointSet struct {
	SchemaVersion string                      `json:"schema_version"`
	NextSequence  uint64                      `json:"next_sequence"`
	Sources       map[string]SourceCheckpoint `json:"sources"`
	Terminal      *terminalCheckpoint         `json:"terminal,omitempty"`
	UpdatedAt     string                      `json:"updated_at"`
}

type terminalCheckpoint struct {
	Sequence    uint64 `json:"sequence"`
	ChunkDigest string `json:"chunk_digest"`
}

// chunkManifest is the single durable pending-delivery record. It contains
// enough source metadata and payload to resume after a crash.
type chunkManifest struct {
	SchemaVersion     string `json:"schema_version"`
	ManifestDigest    string `json:"manifest_digest"`
	ChunkDigest       string `json:"chunk_digest"`
	ConfigIdentity    string `json:"config_identity"`
	Kind              string `json:"kind"`
	Sequence          uint64 `json:"sequence"`
	SourcePath        string `json:"source_path"`
	SourceFileID      string `json:"source_file_id"`
	StartOffset       int64  `json:"start_offset"`
	EndOffset         int64  `json:"end_offset"`
	StartLines        int    `json:"start_lines"`
	EndLines          int    `json:"end_lines"`
	StartPrefixSHA256 string `json:"start_prefix_sha256,omitempty"`
	PrefixSHA256      string `json:"prefix_sha256,omitempty"`
	EventCount        int    `json:"event_count"`
	NDJSON            []byte `json:"ndjson"`
}

type storageOps struct {
	syncFile func(*os.File) error
	syncDir  func(string) error
}

type faultPoint string

const (
	faultAfterChunkWrite              faultPoint = "after_chunk_write"
	faultAfterCheckpointWrite         faultPoint = "after_checkpoint_write"
	faultAfterSinkAccept              faultPoint = "after_sink_accept"
	faultAfterTerminalChunkWrite      faultPoint = "after_terminal_chunk_write"
	faultAfterTerminalCheckpointWrite faultPoint = "after_terminal_checkpoint_write"
	faultAfterTerminalSinkAccept      faultPoint = "after_terminal_sink_accept"
)

func loadCheckpoints(path string) (checkpointSet, bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return checkpointSet{SchemaVersion: CheckpointSchemaV1, NextSequence: 1, Sources: map[string]SourceCheckpoint{}}, false, nil
		}
		return checkpointSet{}, false, err
	}
	var result checkpointSet
	if err := decodeOneJSON(raw, &result); err != nil {
		return checkpointSet{}, true, fmt.Errorf("read checkpoint %s: %w", path, err)
	}
	if result.SchemaVersion != CheckpointSchemaV1 || result.NextSequence == 0 || result.Sources == nil {
		return checkpointSet{}, true, fmt.Errorf("checkpoint %s has invalid schema or fields", path)
	}
	if result.Terminal != nil && (result.Terminal.Sequence == 0 || !validDigest(result.Terminal.ChunkDigest)) {
		return checkpointSet{}, true, fmt.Errorf("checkpoint %s has invalid terminal reference", path)
	}
	for source, checkpoint := range result.Sources {
		if checkpoint.Path != source || checkpoint.Offset < 0 || checkpoint.Lines < 0 ||
			(checkpoint.Sequence == 0 && checkpoint.ChunkDigest != "") ||
			(checkpoint.Sequence > 0 && !validDigest(checkpoint.ChunkDigest)) {
			return checkpointSet{}, true, fmt.Errorf("checkpoint %s has invalid source %q", path, source)
		}
	}
	return result, true, nil
}

func cloneCheckpoints(checkpoints checkpointSet) checkpointSet {
	clone := checkpoints
	clone.Sources = make(map[string]SourceCheckpoint, len(checkpoints.Sources))
	for path, checkpoint := range checkpoints.Sources {
		clone.Sources[path] = checkpoint
	}
	if checkpoints.Terminal != nil {
		terminal := *checkpoints.Terminal
		clone.Terminal = &terminal
	}
	return clone
}

func (r *Runner) storage() storageOps {
	return storageOps{syncFile: r.options.syncFile, syncDir: r.options.syncDir}
}

func defaultStorage() storageOps {
	return storageOps{
		syncFile: func(file *os.File) error { return file.Sync() },
		syncDir:  syncDirectory,
	}
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func ensureDirectory(path string, ops storageOps) error {
	var missing []string
	current := filepath.Clean(path)
	for {
		info, err := os.Stat(current)
		if err == nil {
			if !info.IsDir() {
				return fmt.Errorf("storage path %s is not a directory", current)
			}
			break
		}
		if !os.IsNotExist(err) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			return fmt.Errorf("no existing ancestor for storage directory %s", path)
		}
		current = parent
	}
	for index := len(missing) - 1; index >= 0; index-- {
		dir := missing[index]
		if err := os.Mkdir(dir, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		if err := ops.syncDir(filepath.Dir(dir)); err != nil {
			return fmt.Errorf("sync parent directory after creating %s: %w", dir, err)
		}
	}
	return nil
}

func writeFileDurable(path string, raw []byte, perm os.FileMode, ops storageOps) error {
	dir := filepath.Dir(path)
	if err := ensureDirectory(dir, ops); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && !fileutil.ChmodUnsupported(err) {
		_ = tmp.Close()
		return err
	}
	if err := ops.syncFile(tmp); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary file for %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	cleanup = false
	if err := ops.syncDir(dir); err != nil {
		return fmt.Errorf("sync directory after publishing %s: %w", path, err)
	}
	return nil
}

func writeJSONDurable(path string, value any, ops storageOps) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return writeFileDurable(path, append(raw, '\n'), 0o644, ops)
}

func removeDurable(path string, ops storageOps) error {
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if err := ops.syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync directory after removing %s: %w", path, err)
	}
	return nil
}

func writeCheckpoints(path string, checkpoint checkpointSet, stores ...storageOps) error {
	checkpoint.SchemaVersion = CheckpointSchemaV1
	checkpoint.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return writeJSONDurable(path, checkpoint, selectStorage(stores))
}

func selectStorage(stores []storageOps) storageOps {
	if len(stores) == 0 {
		return defaultStorage()
	}
	return stores[0]
}

func pendingPath(out string, sequence uint64, digest string) string {
	return filepath.Join(out, "pending", fmt.Sprintf("%020d-%s.json", sequence, digest))
}

func writePending(out string, chunk MetricEventChunk, manifest *chunkManifest, stores ...storageOps) (MetricEventChunk, string, error) {
	ops := selectStorage(stores)
	sum := sha256.Sum256(chunk.NDJSON)
	chunk.Digest = hex.EncodeToString(sum[:])
	chunk.SchemaVersion = ChunkSchemaV1
	path := pendingPath(out, chunk.Sequence, chunk.Digest)
	chunk.Path = path

	persisted := *manifest
	persisted.SchemaVersion = ChunkManifestSchemaV1
	persisted.ChunkDigest = chunk.Digest
	persisted.Sequence = chunk.Sequence
	persisted.SourcePath = chunk.SourcePath
	persisted.SourceFileID = chunk.SourceFileID
	persisted.StartOffset = chunk.StartOffset
	persisted.EndOffset = chunk.EndOffset
	persisted.EventCount = len(chunk.Events)
	persisted.NDJSON = chunk.NDJSON
	persisted.ManifestDigest = manifestDigest(persisted)
	*manifest = persisted

	if raw, err := os.ReadFile(path); err == nil {
		var existing chunkManifest
		if err := decodeOneJSON(raw, &existing); err != nil || existing.ManifestDigest != persisted.ManifestDigest {
			return MetricEventChunk{}, "", fmt.Errorf("immutable pending chunk collision at %s", path)
		}
	} else if !os.IsNotExist(err) {
		return MetricEventChunk{}, "", err
	} else if err := writeJSONDurable(path, persisted, ops); err != nil {
		return MetricEventChunk{}, "", err
	}
	return chunk, path, nil
}

func loadPending(out, configIdentity string, ops storageOps) (string, *chunkManifest, error) {
	root := filepath.Join(out, "pending")
	paths, err := spoolFiles(root, ops)
	if err != nil {
		return "", nil, err
	}
	for _, path := range paths {
		if filepath.Dir(path) != root || !validPendingName(filepath.Base(path)) {
			return "", nil, fmt.Errorf("unexpected file in pending spool: %s", path)
		}
	}
	if len(paths) > 1 {
		return "", nil, fmt.Errorf("pending spool contains multiple chunks")
	}
	if len(paths) == 0 {
		return "", nil, nil
	}
	path := paths[0]
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", nil, err
	}
	var manifest chunkManifest
	if err := decodeOneJSON(raw, &manifest); err != nil {
		return "", nil, fmt.Errorf("read pending chunk %s: %w", path, err)
	}
	if err := validateManifest(path, manifest, configIdentity); err != nil {
		return "", nil, err
	}
	return path, &manifest, nil
}

func newHistoryManifest(configIdentity string, checkpoint SourceCheckpoint, read sourceRead, eventCount int) chunkManifest {
	return chunkManifest{
		ConfigIdentity:    configIdentity,
		Kind:              "history",
		SourcePath:        read.path,
		SourceFileID:      read.fileID,
		StartOffset:       read.start,
		EndOffset:         read.end,
		StartLines:        checkpoint.Lines,
		EndLines:          checkpoint.Lines + sourceLineCount(read.data),
		StartPrefixSHA256: checkpoint.PrefixSHA256,
		PrefixSHA256:      read.prefix,
		EventCount:        eventCount,
	}
}

func newTerminalManifest(configIdentity string, sequence uint64, completionPath string, raw []byte) chunkManifest {
	return chunkManifest{
		ConfigIdentity: configIdentity,
		Kind:           "terminal",
		Sequence:       sequence,
		SourcePath:     completionPath,
		SourceFileID:   "completion-status",
		EventCount:     1,
		NDJSON:         raw,
	}
}

func manifestDigest(manifest chunkManifest) string {
	manifest.ManifestDigest = ""
	raw, _ := json.Marshal(manifest)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func validateManifest(path string, manifest chunkManifest, configIdentity string) error {
	if manifest.SchemaVersion != ChunkManifestSchemaV1 ||
		manifest.Sequence == 0 ||
		!validDigest(manifest.ChunkDigest) ||
		!validDigest(manifest.ManifestDigest) ||
		manifest.ManifestDigest != manifestDigest(manifest) ||
		manifest.ConfigIdentity != configIdentity ||
		manifest.EventCount <= 0 {
		return fmt.Errorf("pending chunk %s has invalid schema, digest, or configuration", path)
	}
	wantFile := fmt.Sprintf("%020d-%s.json", manifest.Sequence, manifest.ChunkDigest)
	if filepath.Base(path) != wantFile {
		return fmt.Errorf("pending chunk %s has invalid filename", path)
	}
	sum := sha256.Sum256(manifest.NDJSON)
	if hex.EncodeToString(sum[:]) != manifest.ChunkDigest {
		return fmt.Errorf("pending chunk %s payload digest mismatch", path)
	}
	events, err := decodeCanonicalEvents(manifest.NDJSON)
	if err != nil || len(events) != manifest.EventCount {
		return fmt.Errorf("pending chunk %s has invalid events: count=%d err=%v", path, len(events), err)
	}
	switch manifest.Kind {
	case "history":
		if manifest.SourcePath == "" || manifest.SourceFileID == "" ||
			manifest.StartOffset < 0 || manifest.EndOffset <= manifest.StartOffset ||
			manifest.StartLines < 0 || manifest.EndLines <= manifest.StartLines ||
			manifest.PrefixSHA256 == "" {
			return fmt.Errorf("pending chunk %s has invalid history range", path)
		}
	case "terminal":
		if manifest.SourcePath == "" || manifest.SourceFileID != "completion-status" ||
			manifest.StartOffset != 0 || manifest.EndOffset != 0 ||
			manifest.StartLines != 0 || manifest.EndLines != 0 {
			return fmt.Errorf("pending chunk %s has invalid terminal metadata", path)
		}
	default:
		return fmt.Errorf("pending chunk %s has unknown kind %q", path, manifest.Kind)
	}
	return nil
}

func spoolFiles(root string, ops storageOps) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		if path == root {
			return nil
		}
		if entry.IsDir() {
			return fmt.Errorf("unexpected directory in pending spool: %s", path)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("spool symlink is not allowed: %s", path)
		}
		if target, ok := recognizedAtomicTemp(entry.Name()); ok {
			if !validPendingName(target) {
				return fmt.Errorf("unexpected writer temp file in pending spool: %s", path)
			}
			return removeDurable(path, ops)
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func recognizedAtomicTemp(name string) (string, bool) {
	if !strings.HasPrefix(name, ".") {
		return "", false
	}
	index := strings.LastIndex(name, ".tmp-")
	if index <= 1 || index+len(".tmp-") == len(name) {
		return "", false
	}
	target := name[1:index]
	return target, validPendingName(target)
}

func validPendingName(name string) bool {
	if len(name) != 20+1+sha256.Size*2+len(".json") || !strings.HasSuffix(name, ".json") {
		return false
	}
	stem := strings.TrimSuffix(name, ".json")
	if stem[20] != '-' {
		return false
	}
	for _, char := range stem[:20] {
		if char < '0' || char > '9' {
			return false
		}
	}
	return validDigest(stem[21:])
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func decodeCanonicalEvents(raw []byte) ([]exptelemetry.MetricEvent, error) {
	if len(raw) == 0 || raw[len(raw)-1] != '\n' {
		return nil, fmt.Errorf("chunk must end with a newline")
	}
	lines := bytes.Split(raw[:len(raw)-1], []byte{'\n'})
	events := make([]exptelemetry.MetricEvent, 0, len(lines))
	for i, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			return nil, fmt.Errorf("chunk line %d is empty", i+1)
		}
		var event exptelemetry.MetricEvent
		if err := decodeOneJSON(line, &event); err != nil {
			return nil, fmt.Errorf("decode chunk line %d: %w", i+1, err)
		}
		if err := event.Validate(); err != nil {
			return nil, fmt.Errorf("validate chunk line %d: %w", i+1, err)
		}
		canonical, err := event.MarshalNDJSON()
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(canonical[:len(canonical)-1], line) {
			return nil, fmt.Errorf("chunk line %d is not canonical", i+1)
		}
		events = append(events, event)
	}
	return events, nil
}

func decodeOneJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("expected exactly one JSON value")
		}
		return fmt.Errorf("trailing JSON data: %w", err)
	}
	return nil
}
