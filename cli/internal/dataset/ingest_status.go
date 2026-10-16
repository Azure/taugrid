// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dataset

import (
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

type IngestState string

const (
	IngestStateRegistered IngestState = "registered"
	IngestStateIngesting  IngestState = "ingesting"
	IngestStateReady      IngestState = "ready"
	IngestStateFailed     IngestState = "failed"
)

const IngestStatusSchemaVersion = 1

type FileProof struct {
	Path        string `json:"path"`
	SHA256      string `json:"sha256"`
	Bytes       int64  `json:"bytes"`
	CommittedAt string `json:"committed_at"`
}

type IngestStatus struct {
	SchemaVersion  int         `json:"schema_version"`
	Name           string      `json:"name"`
	Version        string      `json:"version"`
	RecordDigest   string      `json:"record_digest"`
	State          IngestState `json:"state"`
	SourceRoot     string      `json:"source_root,omitempty"`
	Destination    string      `json:"destination,omitempty"`
	AttemptID      string      `json:"attempt_id,omitempty"`
	CompletedFiles []FileProof `json:"completed_files,omitempty"`
	VerifiedBytes  int64       `json:"verified_bytes"`
	VerifiedFiles  int         `json:"verified_files"`
	StartedAt      string      `json:"started_at,omitempty"`
	UpdatedAt      string      `json:"updated_at,omitempty"`
	FailureSummary string      `json:"failure_summary,omitempty"`
}

func ParseIngestStatus(raw []byte) (IngestStatus, error) {
	var s IngestStatus
	if err := json.Unmarshal(raw, &s); err != nil {
		return IngestStatus{}, fmt.Errorf("parse ingest-status: %w", err)
	}
	if err := s.Validate(); err != nil {
		return IngestStatus{}, err
	}
	return s, nil
}

func (s IngestStatus) Validate() error {
	if s.SchemaVersion != IngestStatusSchemaVersion {
		return fmt.Errorf("ingest-status schema_version %d not supported (want %d)", s.SchemaVersion, IngestStatusSchemaVersion)
	}
	if err := ValidateName(s.Name); err != nil {
		return fmt.Errorf("ingest-status: %w", err)
	}
	if err := ValidateVersion(s.Version); err != nil {
		return fmt.Errorf("ingest-status: %w", err)
	}
	if s.RecordDigest == "" {
		return fmt.Errorf("ingest-status: record_digest is required")
	}
	if !strings.HasPrefix(s.RecordDigest, "sha256:") {
		return fmt.Errorf("ingest-status: record_digest %q must be sha256:-prefixed", s.RecordDigest)
	}
	switch s.State {
	case IngestStateRegistered, IngestStateIngesting, IngestStateReady, IngestStateFailed:
	default:
		return fmt.Errorf("ingest-status: unknown state %q", s.State)
	}

	seen := make(map[string]struct{}, len(s.CompletedFiles))
	var sumBytes int64
	for i, p := range s.CompletedFiles {
		if p.Path == "" {
			return fmt.Errorf("ingest-status: completed_files[%d] has empty path", i)
		}
		if path.IsAbs(p.Path) || filepath.IsAbs(filepath.FromSlash(p.Path)) || hasWindowsVolumePath(p.Path) || hasTraversalSegment(p.Path) {
			return fmt.Errorf("ingest-status: completed_files[%d] path %q is not a clean relative path", i, p.Path)
		}
		if _, dup := seen[p.Path]; dup {
			return fmt.Errorf("ingest-status: duplicate proof for path %q", p.Path)
		}
		seen[p.Path] = struct{}{}
		if len(p.SHA256) != 64 || !isHex(p.SHA256) {
			return fmt.Errorf("ingest-status: completed_files[%d] sha256 %q is not a 64-char hex digest", i, p.SHA256)
		}
		if p.Bytes < 0 {
			return fmt.Errorf("ingest-status: completed_files[%d] has negative bytes", i)
		}
		if p.CommittedAt == "" {
			return fmt.Errorf("ingest-status: completed_files[%d] missing committed_at", i)
		}
		if _, err := time.Parse(time.RFC3339, p.CommittedAt); err != nil {
			return fmt.Errorf("ingest-status: completed_files[%d] committed_at %q is not RFC3339: %w", i, p.CommittedAt, err)
		}
		sumBytes += p.Bytes
	}

	if s.VerifiedFiles != len(s.CompletedFiles) {
		return fmt.Errorf("ingest-status: verified_files=%d but completed_files has %d entries", s.VerifiedFiles, len(s.CompletedFiles))
	}
	if s.VerifiedBytes != sumBytes {
		return fmt.Errorf("ingest-status: verified_bytes=%d but completed_files sum to %d", s.VerifiedBytes, sumBytes)
	}
	if s.VerifiedFiles < 0 || s.VerifiedBytes < 0 {
		return fmt.Errorf("ingest-status: verified counters must be non-negative")
	}

	if len(s.FailureSummary) > 512 {
		return fmt.Errorf("ingest-status: failure_summary exceeds 512 bytes (%d)", len(s.FailureSummary))
	}

	if s.State == IngestStateFailed && s.FailureSummary == "" {
		return fmt.Errorf("ingest-status: failed state requires a failure_summary")
	}
	if s.State == IngestStateReady && s.FailureSummary != "" {
		return fmt.Errorf("ingest-status: ready state must not carry a failure_summary")
	}

	for label, ts := range map[string]string{"started_at": s.StartedAt, "updated_at": s.UpdatedAt} {
		if ts == "" {
			continue
		}
		if _, err := time.Parse(time.RFC3339, ts); err != nil {
			return fmt.Errorf("ingest-status: %s %q is not RFC3339: %w", label, ts, err)
		}
	}
	return nil
}

func hasWindowsVolumePath(value string) bool {
	return len(value) >= 3 &&
		((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) &&
		value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func hasTraversalSegment(p string) bool {
	return slices.Contains(strings.Split(p, "/"), "..")
}

func isHex(s string) bool {
	for _, c := range s {
		switch {
		case c >= '0' && c <= '9':
		case c >= 'a' && c <= 'f':
		case c >= 'A' && c <= 'F':
		default:
			return false
		}
	}
	return true
}

func (s IngestStatus) Marshal() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

func newRegisteredStatus(rec Record, now func() time.Time) IngestStatus {
	if now == nil {
		now = time.Now
	}
	ts := now().UTC().Format(time.RFC3339)
	return IngestStatus{
		SchemaVersion: IngestStatusSchemaVersion,
		Name:          rec.Name,
		Version:       rec.Version,
		RecordDigest:  rec.Digest,
		State:         IngestStateRegistered,
		StartedAt:     ts,
		UpdatedAt:     ts,
	}
}
