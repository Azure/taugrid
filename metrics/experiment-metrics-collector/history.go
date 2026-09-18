// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package collector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type SourceCheckpoint struct {
	Path         string `json:"path"`
	FileID       string `json:"file_id"`
	Offset       int64  `json:"offset"`
	PrefixSHA256 string `json:"prefix_sha256,omitempty"`
	Sequence     uint64 `json:"sequence"`
	ChunkDigest  string `json:"chunk_digest,omitempty"`
	Lines        int    `json:"lines"`
	UpdatedAt    string `json:"updated_at"`
}

type sourceRead struct {
	path      string
	fileID    string
	data      []byte
	start     int64
	end       int64
	prefix    string
	startLine int
	modTime   time.Time
}

func expandHistory(patterns []string) ([]string, error) {
	seen := map[string]bool{}
	var out []string
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		matches := []string{pattern}
		if strings.ContainsAny(pattern, "*?[") {
			var err error
			matches, err = filepath.Glob(pattern)
			if err != nil {
				return nil, err
			}
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
				return nil, fmt.Errorf("history path %s is a directory", match)
			}
			absolute, err := filepath.Abs(match)
			if err != nil {
				return nil, err
			}
			if !seen[absolute] {
				seen[absolute] = true
				out = append(out, absolute)
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

func readSource(path string, checkpoint SourceCheckpoint) (sourceRead, error) {
	f, err := os.Open(path)
	if err != nil {
		return sourceRead{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return sourceRead{}, err
	}
	fileID := fileIdentity(info)
	if checkpoint.Path != "" {
		if checkpoint.Path != path {
			return sourceRead{}, fmt.Errorf("checkpoint path mismatch: %q != %q", checkpoint.Path, path)
		}
		if checkpoint.FileID != fileID {
			return sourceRead{}, fmt.Errorf("history file identity changed for %s", path)
		}
		if checkpoint.Offset < 0 || checkpoint.Offset > info.Size() {
			return sourceRead{}, fmt.Errorf("checkpoint offset %d is outside %s size %d", checkpoint.Offset, path, info.Size())
		}
		prefix, err := prefixDigest(f, checkpoint.Offset)
		if err != nil {
			return sourceRead{}, err
		}
		if prefix != checkpoint.PrefixSHA256 {
			return sourceRead{}, fmt.Errorf("history prefix mismatch for %s at offset %d", path, checkpoint.Offset)
		}
	}
	if _, err := f.Seek(checkpoint.Offset, io.SeekStart); err != nil {
		return sourceRead{}, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return sourceRead{}, err
	}
	lastNewline := bytes.LastIndexByte(raw, '\n')
	if lastNewline < 0 {
		raw = nil
	} else {
		raw = raw[:lastNewline+1]
	}
	end := checkpoint.Offset + int64(len(raw))
	prefix, err := prefixDigest(f, end)
	if err != nil {
		return sourceRead{}, err
	}
	return sourceRead{
		path: path, fileID: fileID, data: raw, start: checkpoint.Offset, end: end,
		prefix: prefix, startLine: checkpoint.Lines + 1, modTime: info.ModTime().UTC(),
	}, nil
}

func baselineSource(path string, sequence uint64) (SourceCheckpoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return SourceCheckpoint{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return SourceCheckpoint{}, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return SourceCheckpoint{}, err
	}
	end := int64(0)
	if last := bytes.LastIndexByte(raw, '\n'); last >= 0 {
		end = int64(last + 1)
	}
	prefix, err := prefixDigest(f, end)
	if err != nil {
		return SourceCheckpoint{}, err
	}
	return SourceCheckpoint{
		Path: path, FileID: fileIdentity(info), Offset: end, PrefixSHA256: prefix,
		Sequence: sequence, Lines: bytes.Count(raw[:end], []byte{'\n'}), UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, nil
}

func prefixDigest(f *os.File, offset int64) (string, error) {
	if offset == 0 {
		return "", nil
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.CopyN(h, f, offset); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func fileIdentity(info os.FileInfo) string {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return fmt.Sprintf("%d:%d", uint64(stat.Dev), uint64(stat.Ino))
	}
	return fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
}

func logicalMetricFileID(path string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return "history-" + hex.EncodeToString(sum[:])[:16]
}
