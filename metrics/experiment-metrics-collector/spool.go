// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"bytes"
	"context"
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

// chunkManifest is the durable transaction record: it is written before the
// checkpoint advances, contains enough data to reconstruct the chunk, and is
// replayed to every configured sink before any new source bytes are read.
type chunkManifest struct {
	SchemaVersion     string `json:"schema_version"`
	ManifestDigest    string `json:"manifest_digest"`
	ChunkDigest       string `json:"chunk_digest"`
	ChunkFile         string `json:"chunk_file"`
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

type faultPoint string

const (
	faultAfterChunkWrite              faultPoint = "after_chunk_write"
	faultAfterCheckpointWrite         faultPoint = "after_checkpoint_write"
	faultAfterSinkAccept              faultPoint = "after_sink_accept"
	faultAfterSinkReceipt             faultPoint = "after_sink_receipt"
	faultAfterTerminalChunkWrite      faultPoint = "after_terminal_chunk_write"
	faultAfterTerminalCheckpointWrite faultPoint = "after_terminal_checkpoint_write"
	faultAfterTerminalSinkAccept      faultPoint = "after_terminal_sink_accept"
	faultAfterTerminalSinkReceipt     faultPoint = "after_terminal_sink_receipt"
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

func writeCheckpoints(path string, checkpoint checkpointSet) error {
	checkpoint.SchemaVersion = CheckpointSchemaV1
	checkpoint.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	return fileutil.WriteJSONFileAtomic(path, checkpoint)
}

func writeChunk(out string, chunk MetricEventChunk, manifests ...chunkManifest) (MetricEventChunk, error) {
	sum := sha256.Sum256(chunk.NDJSON)
	chunk.Digest = hex.EncodeToString(sum[:])
	chunk.SchemaVersion = ChunkSchemaV1
	dir := filepath.Join(out, "chunks")
	chunk.Path = filepath.Join(dir, fmt.Sprintf("%020d-%s.ndjson", chunk.Sequence, chunk.Digest))
	if len(manifests) > 1 {
		return MetricEventChunk{}, fmt.Errorf("writeChunk accepts at most one manifest")
	}
	if len(manifests) == 1 {
		manifest := manifests[0]
		manifest.SchemaVersion = ChunkManifestSchemaV1
		manifest.ChunkDigest = chunk.Digest
		manifest.ChunkFile = filepath.Base(chunk.Path)
		manifest.Sequence = chunk.Sequence
		manifest.SourcePath = chunk.SourcePath
		manifest.SourceFileID = chunk.SourceFileID
		manifest.StartOffset = chunk.StartOffset
		manifest.EndOffset = chunk.EndOffset
		manifest.EventCount = len(chunk.Events)
		manifest.NDJSON = chunk.NDJSON
		manifest.ManifestDigest = manifestDigest(manifest)
		manifestPath := filepath.Join(out, "manifests", fmt.Sprintf("%020d-%s.json", chunk.Sequence, chunk.Digest))
		if existing, err := os.ReadFile(manifestPath); err == nil {
			var persisted chunkManifest
			if err := decodeOneJSON(existing, &persisted); err != nil || persisted.ManifestDigest != manifest.ManifestDigest {
				return MetricEventChunk{}, fmt.Errorf("immutable manifest collision at %s", manifestPath)
			}
		} else if !os.IsNotExist(err) {
			return MetricEventChunk{}, err
		} else if err := fileutil.WriteJSONFileAtomic(manifestPath, manifest); err != nil {
			return MetricEventChunk{}, err
		}
	}
	if existing, err := os.ReadFile(chunk.Path); err == nil {
		if !bytes.Equal(existing, chunk.NDJSON) {
			return MetricEventChunk{}, fmt.Errorf("immutable chunk collision at %s", chunk.Path)
		}
	} else if !os.IsNotExist(err) {
		return MetricEventChunk{}, err
	} else if err := fileutil.WriteFileAtomic(chunk.Path, chunk.NDJSON, 0o644); err != nil {
		return MetricEventChunk{}, err
	}
	return chunk, nil
}

func loadSpool(out, configIdentity string, _ []Sink) ([]chunkManifest, error) {
	manifestRoot := filepath.Join(out, "manifests")
	paths, err := spoolFiles(manifestRoot)
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	manifests := make([]chunkManifest, 0, len(paths))
	terminalSeen := false
	for _, path := range paths {
		if filepath.Dir(path) != manifestRoot || filepath.Ext(path) != ".json" {
			return nil, fmt.Errorf("unexpected file in manifest spool: %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var manifest chunkManifest
		if err := decodeOneJSON(raw, &manifest); err != nil {
			return nil, fmt.Errorf("read spool manifest %s: %w", path, err)
		}
		if err := validateManifest(path, manifest, configIdentity); err != nil {
			return nil, err
		}
		if manifest.Sequence != uint64(len(manifests))+1 {
			return nil, fmt.Errorf("spool manifest sequence gap at %s", path)
		}
		if terminalSeen {
			return nil, fmt.Errorf("spool manifest follows terminal manifest: %s", path)
		}
		terminalSeen = manifest.Kind == "terminal"
		chunkPath := filepath.Join(out, "chunks", manifest.ChunkFile)
		if raw, err := os.ReadFile(chunkPath); err == nil {
			if !bytes.Equal(raw, manifest.NDJSON) {
				return nil, fmt.Errorf("spool chunk does not match manifest: %s", chunkPath)
			}
		} else if !os.IsNotExist(err) {
			return nil, err
		} else if err := fileutil.WriteFileAtomic(chunkPath, manifest.NDJSON, 0o644); err != nil {
			return nil, fmt.Errorf("restore spool chunk %s: %w", chunkPath, err)
		}
		manifests = append(manifests, manifest)
	}
	chunkRoot := filepath.Join(out, "chunks")
	chunks, err := spoolFiles(chunkRoot)
	if err != nil {
		return nil, err
	}
	for _, path := range chunks {
		if filepath.Dir(path) != chunkRoot || filepath.Ext(path) != ".ndjson" {
			return nil, fmt.Errorf("unexpected file in chunk spool: %s", path)
		}
	}
	if len(chunks) != len(manifests) {
		return nil, fmt.Errorf("spool contains chunk without a matching manifest")
	}
	if err := validateReceipts(out, manifests); err != nil {
		return nil, err
	}
	return manifests, nil
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
		EndLines:          checkpoint.Lines + bytes.Count(read.data, []byte{'\n'}),
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
		return fmt.Errorf("spool manifest %s has invalid schema, digest, or configuration", path)
	}
	wantFile := fmt.Sprintf("%020d-%s.ndjson", manifest.Sequence, manifest.ChunkDigest)
	if manifest.ChunkFile != wantFile || filepath.Base(path) != strings.TrimSuffix(wantFile, ".ndjson")+".json" {
		return fmt.Errorf("spool manifest %s has invalid chunk reference", path)
	}
	sum := sha256.Sum256(manifest.NDJSON)
	if hex.EncodeToString(sum[:]) != manifest.ChunkDigest {
		return fmt.Errorf("spool manifest %s payload digest mismatch", path)
	}
	events, err := decodeCanonicalEvents(manifest.NDJSON)
	if err != nil || len(events) != manifest.EventCount {
		return fmt.Errorf("spool manifest %s has invalid events: count=%d err=%v", path, len(events), err)
	}
	switch manifest.Kind {
	case "history":
		if manifest.SourcePath == "" || manifest.SourceFileID == "" ||
			manifest.StartOffset < 0 || manifest.EndOffset <= manifest.StartOffset ||
			manifest.StartLines < 0 || manifest.EndLines <= manifest.StartLines ||
			manifest.PrefixSHA256 == "" {
			return fmt.Errorf("spool manifest %s has invalid history range", path)
		}
	case "terminal":
		if manifest.SourcePath == "" || manifest.SourceFileID != "completion-status" ||
			manifest.StartOffset != 0 || manifest.EndOffset != 0 ||
			manifest.StartLines != 0 || manifest.EndLines != 0 {
			return fmt.Errorf("spool manifest %s has invalid terminal metadata", path)
		}
	default:
		return fmt.Errorf("spool manifest %s has unknown kind %q", path, manifest.Kind)
	}
	return nil
}

func validateReceipts(out string, manifests []chunkManifest) error {
	root := filepath.Join(out, "receipts")
	paths, err := spoolFiles(root)
	if err != nil {
		return err
	}
	byDigest := make(map[string]chunkManifest, len(manifests))
	for _, manifest := range manifests {
		byDigest[manifest.ChunkDigest] = manifest
	}
	for _, path := range paths {
		if filepath.Ext(path) != ".json" {
			return fmt.Errorf("unexpected file in receipt spool: %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var ack DeliveryAck
		if err := decodeOneJSON(raw, &ack); err != nil {
			return fmt.Errorf("read delivery receipt %s: %w", path, err)
		}
		if err := validateReceipt(path, ack); err != nil {
			return err
		}
		_, ok := byDigest[ack.ChunkDigest]
		if !ok {
			return fmt.Errorf("delivery receipt %s has no matching durable chunk", path)
		}
	}
	return nil
}

func spoolFiles(root string) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) && path == root {
				return nil
			}
			return err
		}
		if path == root || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("spool symlink is not allowed: %s", path)
		}
		paths = append(paths, path)
		return nil
	})
	sort.Strings(paths)
	return paths, err
}

func validateReceipt(path string, ack DeliveryAck) error {
	if ack.SchemaVersion != ReceiptSchemaV1 ||
		!validDigest(ack.ChunkDigest) ||
		strings.TrimSpace(ack.ConfigIdentity) == "" ||
		strings.TrimSpace(ack.Sink) == "" ||
		ack.Samples < 0 || ack.Requests < 0 || ack.Retries < 0 ||
		ack.DeliveredAt.IsZero() {
		return fmt.Errorf("delivery receipt %s has invalid schema or fields", path)
	}
	for key, value := range ack.Metadata {
		if strings.TrimSpace(key) == "" || len(key) > 128 || len(value) > 1024 {
			return fmt.Errorf("delivery receipt %s has invalid metadata", path)
		}
	}
	configHash := sha256.Sum256([]byte(ack.ConfigIdentity))
	wantSuffix := filepath.Join(
		fileutil.SafePathComponent(ack.Sink),
		hex.EncodeToString(configHash[:]),
		ack.ChunkDigest+".json",
	)
	if !strings.HasSuffix(path, wantSuffix) {
		return fmt.Errorf("delivery receipt %s path does not match its contents", path)
	}
	return nil
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

func receiptPath(out string, sink Sink, digest string) string {
	configHash := sha256.Sum256([]byte(sink.ConfigIdentity()))
	return filepath.Join(out, "receipts", fileutil.SafePathComponent(sink.Name()), hex.EncodeToString(configHash[:]), digest+".json")
}

func deliverWithReceipt(ctx context.Context, out string, sink Sink, chunk MetricEventChunk, injectors ...func(faultPoint) error) (DeliveryAck, bool, error) {
	var inject func(faultPoint) error
	if len(injectors) > 1 {
		return DeliveryAck{}, false, fmt.Errorf("deliverWithReceipt accepts at most one fault injector")
	}
	if len(injectors) == 1 {
		inject = injectors[0]
	}
	path := receiptPath(out, sink, chunk.Digest)
	if raw, err := os.ReadFile(path); err == nil {
		var ack DeliveryAck
		if err := decodeOneJSON(raw, &ack); err != nil {
			return DeliveryAck{}, false, fmt.Errorf("read delivery receipt %s: %w", path, err)
		}
		if err := validateReceipt(path, ack); err != nil {
			return DeliveryAck{}, false, err
		}
		if ack.ChunkDigest != chunk.Digest || ack.ConfigIdentity != sink.ConfigIdentity() || ack.Sink != sink.Name() {
			return DeliveryAck{}, false, fmt.Errorf("delivery receipt %s does not match chunk and sink configuration", path)
		}
		if ack.Samples != len(chunk.Events) {
			return DeliveryAck{}, false, fmt.Errorf("delivery receipt %s sample count does not match chunk", path)
		}
		return ack, true, nil
	} else if !os.IsNotExist(err) {
		return DeliveryAck{}, false, err
	}
	ack, err := sink.Deliver(ctx, chunk)
	if err != nil {
		return DeliveryAck{}, false, err
	}
	acceptPoint := faultAfterSinkAccept
	receiptPoint := faultAfterSinkReceipt
	if chunk.SourceFileID == "completion-status" {
		acceptPoint = faultAfterTerminalSinkAccept
		receiptPoint = faultAfterTerminalSinkReceipt
	}
	if inject != nil {
		if err := inject(acceptPoint); err != nil {
			return DeliveryAck{}, false, err
		}
	}
	ack.SchemaVersion = ReceiptSchemaV1
	ack.ChunkDigest = chunk.Digest
	ack.ConfigIdentity = sink.ConfigIdentity()
	ack.Sink = sink.Name()
	if ack.DeliveredAt.IsZero() {
		ack.DeliveredAt = time.Now().UTC()
	}
	if err := validateReceipt(path, ack); err != nil {
		return DeliveryAck{}, false, err
	}
	if err := fileutil.WriteJSONFileAtomic(path, ack); err != nil {
		return DeliveryAck{}, false, err
	}
	if inject != nil {
		if err := inject(receiptPoint); err != nil {
			return DeliveryAck{}, false, err
		}
	}
	return ack, false, nil
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
