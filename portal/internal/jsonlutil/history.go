// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jsonlutil

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
)

type FileCheckpoint struct {
	Path         string `json:"path"`
	Offset       int64  `json:"offset"`
	SizeBytes    int64  `json:"size_bytes"`
	ModTime      string `json:"mod_time"`
	Device       uint64 `json:"device,omitempty"`
	Inode        uint64 `json:"inode,omitempty"`
	PrefixSHA256 string `json:"prefix_sha256,omitempty"`
	UpdatedAt    string `json:"updated_at"`
}

type HistoryChunk struct {
	Path         string
	Data         []byte
	StartOffset  int64
	EndOffset    int64
	SizeBytes    int64
	ModTime      time.Time
	Device       uint64
	Inode        uint64
	PrefixSHA256 string
	ExportedAt   time.Time
}

func HasJSONL(chunk HistoryChunk) bool {
	return len(bytes.TrimSpace(chunk.Data)) > 0
}

func CheckpointForChunk(chunk HistoryChunk) FileCheckpoint {
	return FileCheckpoint{
		Path:         chunk.Path,
		Offset:       chunk.EndOffset,
		SizeBytes:    chunk.SizeBytes,
		ModTime:      chunk.ModTime.UTC().Format(time.RFC3339),
		Device:       chunk.Device,
		Inode:        chunk.Inode,
		PrefixSHA256: chunk.PrefixSHA256,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
}

// CurrentFileCheckpoint snapshots the current end and identity of a JSONL file.
// It is used to establish a fresh consumer baseline without reading old rows.
func CurrentFileCheckpoint(path string) (FileCheckpoint, error) {
	return currentFileCheckpoint(path, syncHistoryFile)
}

func currentFileCheckpoint(path string, syncFile func(*os.File) error) (FileCheckpoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileCheckpoint{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return FileCheckpoint{}, err
	}
	if info.IsDir() {
		return FileCheckpoint{}, fmt.Errorf("JSONL history path %s is a directory", path)
	}
	device, inode := fileIdentity(f)
	prefix, err := filePrefixSHA256(f, info.Size())
	if err != nil {
		return FileCheckpoint{}, err
	}
	if err := syncFile(f); err != nil {
		return FileCheckpoint{}, fmt.Errorf("sync JSONL history %s: %w", path, err)
	}
	return FileCheckpoint{
		Path:         path,
		Offset:       info.Size(),
		SizeBytes:    info.Size(),
		ModTime:      info.ModTime().UTC().Format(time.RFC3339),
		Device:       device,
		Inode:        inode,
		PrefixSHA256: prefix,
		UpdatedAt:    time.Now().UTC().Format(time.RFC3339),
	}, nil
}

// ResolveFileCheckpoint returns the checkpoint for path, including a checkpoint
// recorded before the same file was renamed to another matching history path.
func ResolveFileCheckpoint(path string, checkpoints map[string]FileCheckpoint) (FileCheckpoint, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return FileCheckpoint{}, false, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return FileCheckpoint{}, false, err
	}
	device, inode := fileIdentity(f)
	if checkpoint, ok := checkpoints[path]; ok {
		identityKnown := checkpoint.Inode != 0 && inode != 0
		if !identityKnown ||
			checkpoint.Offset == 0 ||
			(checkpoint.Device == device && checkpoint.Inode == inode) {
			return checkpoint, true, nil
		}
		if checkpoint.Offset <= info.Size() && checkpoint.PrefixSHA256 != "" {
			prefix, err := filePrefixSHA256(f, checkpoint.Offset)
			if err != nil {
				return FileCheckpoint{}, false, err
			}
			if prefix == checkpoint.PrefixSHA256 {
				checkpoint.Device = device
				checkpoint.Inode = inode
			}
		}
		return checkpoint, true, nil
	}
	var best FileCheckpoint
	found := false
	for _, checkpoint := range checkpoints {
		identityKnown := checkpoint.Inode != 0 && inode != 0
		matches := identityKnown &&
			checkpoint.Device == device && checkpoint.Inode == inode
		matchedByPrefix := false
		if checkpoint.Offset > 0 && checkpoint.PrefixSHA256 != "" {
			if checkpoint.Offset > info.Size() {
				matches = false
				continue
			}
			prefix, err := filePrefixSHA256(f, checkpoint.Offset)
			if err != nil {
				return FileCheckpoint{}, false, err
			}
			prefixMatches := prefix == checkpoint.PrefixSHA256
			matchedByPrefix = prefixMatches && !matches
			matches = prefixMatches
		}
		if matches && (!found || checkpoint.Offset > best.Offset) {
			best = checkpoint
			best.Path = path
			if matchedByPrefix {
				best.Device = device
				best.Inode = inode
			}
			found = true
		}
	}
	return best, found, nil
}

func ExpandInputs(patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		var matches []string
		if strings.ContainsAny(pattern, "*?[") {
			globbed, err := filepath.Glob(pattern)
			if err != nil {
				return nil, err
			}
			matches = globbed
		} else {
			matches = []string{pattern}
		}
		for _, match := range matches {
			info, err := os.Stat(match)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return nil, err
			}
			if info.IsDir() {
				continue
			}
			abs, err := filepath.Abs(match)
			if err != nil {
				return nil, err
			}
			abs = filepath.Clean(abs)
			if seen[abs] {
				continue
			}
			seen[abs] = true
			out = append(out, abs)
		}
	}
	sort.Strings(out)
	return out, nil
}

func ReadHistoryChunk(path string, checkpoint FileCheckpoint) (HistoryChunk, error) {
	return readHistoryChunk(path, checkpoint, syncHistoryFile)
}

func ReadFinalHistoryChunk(path string, checkpoint FileCheckpoint) (HistoryChunk, error) {
	return readHistoryChunkMode(path, checkpoint, syncHistoryFile, true)
}

func readHistoryChunk(path string, checkpoint FileCheckpoint, syncFile func(*os.File) error) (HistoryChunk, error) {
	return readHistoryChunkMode(path, checkpoint, syncFile, false)
}

func readHistoryChunkMode(path string, checkpoint FileCheckpoint, syncFile func(*os.File) error, final bool) (HistoryChunk, error) {
	f, err := os.Open(path)
	if err != nil {
		return HistoryChunk{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return HistoryChunk{}, err
	}
	if info.IsDir() {
		return HistoryChunk{}, fmt.Errorf("JSONL history path %s is a directory", path)
	}
	device, inode := fileIdentity(f)
	offset := checkpoint.Offset
	if checkpoint.Inode != 0 && (checkpoint.Device != device || checkpoint.Inode != inode) {
		offset = 0
	}
	if offset < 0 || info.Size() < offset {
		offset = 0
	}
	if offset > 0 && checkpoint.PrefixSHA256 != "" {
		prefix, err := filePrefixSHA256(f, offset)
		if err != nil {
			return HistoryChunk{}, err
		}
		if prefix != checkpoint.PrefixSHA256 {
			offset = 0
		}
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return HistoryChunk{}, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return HistoryChunk{}, err
	}
	if len(raw) > 0 {
		if err := syncFile(f); err != nil {
			return HistoryChunk{}, fmt.Errorf("sync JSONL history %s: %w", path, err)
		}
	}
	lastNewline := bytes.LastIndexByte(raw, '\n')
	if lastNewline < 0 && !final {
		prefix, err := filePrefixSHA256(f, offset)
		if err != nil {
			return HistoryChunk{}, err
		}
		return HistoryChunk{Path: path, StartOffset: offset, EndOffset: offset, SizeBytes: info.Size(), ModTime: info.ModTime().UTC(), Device: device, Inode: inode, PrefixSHA256: prefix}, nil
	}
	data := append([]byte(nil), raw...)
	if !final {
		data = data[:lastNewline+1]
	}
	endOffset := offset + int64(len(data))
	prefix, err := filePrefixSHA256(f, endOffset)
	if err != nil {
		return HistoryChunk{}, err
	}
	return HistoryChunk{
		Path:         path,
		Data:         data,
		StartOffset:  offset,
		EndOffset:    endOffset,
		SizeBytes:    info.Size(),
		ModTime:      info.ModTime().UTC(),
		Device:       device,
		Inode:        inode,
		PrefixSHA256: prefix,
		ExportedAt:   chunkExportedAt(data),
	}, nil
}

func WriteHistoryChunk(rootDir, partition string, chunk HistoryChunk) (string, string, error) {
	sum := sha256.Sum256(chunk.Data)
	digest := hex.EncodeToString(sum[:])
	sourceKey := fileutil.ShortStringHash(chunk.Path)
	chunkKey := fmt.Sprintf("%s-%020d-%020d-%s", sourceKey, chunk.StartOffset, chunk.EndOffset, digest[:12])
	dir := filepath.Join(rootDir, fileutil.SafePathComponent(partition), sourceKey)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", "", err
	}
	path := filepath.Join(dir, chunkKey+".jsonl")
	if err := fileutil.WriteFileAtomic(path, chunk.Data, 0o644); err != nil {
		return "", "", err
	}
	if !chunk.ExportedAt.IsZero() {
		_ = os.Chtimes(path, chunk.ExportedAt, chunk.ExportedAt)
	}
	return path, chunkKey, nil
}

func filePrefixSHA256(f *os.File, offset int64) (string, error) {
	if offset <= 0 {
		return "", nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, offset); err != nil && err != io.EOF {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func chunkExportedAt(raw []byte) time.Time {
	var latest time.Time
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(line))
		dec.UseNumber()
		var payload map[string]any
		if err := dec.Decode(&payload); err != nil {
			continue
		}
		seconds, ok := jsonNumberValue(payload["_timestamp"])
		if !ok {
			continue
		}
		t := time.UnixMicro(int64(seconds * 1_000_000)).UTC()
		if latest.IsZero() || t.After(latest) {
			latest = t
		}
	}
	if latest.IsZero() {
		return time.Unix(0, 0).UTC()
	}
	return latest
}

func jsonNumberValue(value any) (float64, bool) {
	switch v := value.(type) {
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	default:
		return 0, false
	}
}
