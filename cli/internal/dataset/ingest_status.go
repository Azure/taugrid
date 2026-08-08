// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dataset

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// IngestState is the lifecycle state of a dataset ingest operation, bound to
// one immutable record digest. States advance forward only; they never
// regress. A ready state with a matching record_digest is the truth of
// readiness; the record's tags are not readiness evidence.
type IngestState string

const (
	// IngestStateRegistered means the record is known but no ingest has
	// started (or a previous attempt was abandoned without leaving ingesting).
	IngestStateRegistered IngestState = "registered"
	// IngestStateIngesting means an active attempt holds the version lock.
	IngestStateIngesting IngestState = "ingesting"
	// IngestStateReady means all files are verified at the destination.
	// This is the only state from which a resolved reference is emitted.
	IngestStateReady IngestState = "ready"
	// IngestStateFailed means the attempt concluded with an unrecoverable
	// error; failure_summary carries the bounded reason. A new attempt
	// may start (the state transitions back to ingesting).
	IngestStateFailed IngestState = "failed"
)

// IngestStatusSchemaVersion is the schema version for IngestStatus JSON.
const IngestStatusSchemaVersion = 1

// FileProof is the durable per-file evidence of a successful commit: the
// manifest-relative path, the sha256 of the bytes that were written and
// verified, the byte count, and the time the file was committed. Once a
// FileProof is appended to completed_files it is never mutated.
type FileProof struct {
	// Path is the manifest-relative path (forward slashes, no leading slash).
	Path string `json:"path"`
	// SHA256 is the hex-encoded sha256 of the committed bytes.
	SHA256 string `json:"sha256"`
	// Bytes is the byte count verified at commit time.
	Bytes int64 `json:"bytes"`
	// CommittedAt is the RFC3339 UTC timestamp of the successful commit.
	CommittedAt string `json:"committed_at"`
}

// IngestStatus is the mutable ingest-progress companion to an immutable Record.
// It is stored at ingest-status.json alongside dataset.json in the version
// directory and is always written with overwrite=true.
//
// The record_digest field binds this status to exactly one immutable record: if
// the record is replaced (which is prohibited) or a status from a different
// dataset is accidentally loaded, the digest mismatch surfaces immediately.
type IngestStatus struct {
	// SchemaVersion is always IngestStatusSchemaVersion (1).
	SchemaVersion int `json:"schema_version"`
	// Name is the dataset name.
	Name string `json:"name"`
	// Version is the dataset version.
	Version string `json:"version"`
	// RecordDigest is the digest of the immutable Record this status is
	// bound to. Must match Record.Digest on every read.
	RecordDigest string `json:"record_digest"`
	// State is the current lifecycle state.
	State IngestState `json:"state"`
	// SourceRoot is the source root URI (file:// or az://) used for this
	// ingest. Stored so the worker can resume without re-parsing flags.
	SourceRoot string `json:"source_root,omitempty"`
	// Destination is the destination URI (file:// or az://) for this ingest.
	Destination string `json:"destination,omitempty"`
	// AttemptID is the unique ID of the current or last attempt. A new
	// attempt always generates a new ID so concurrent workers can detect
	// a stolen lock.
	AttemptID string `json:"attempt_id,omitempty"`
	// CompletedFiles holds durable per-file proofs. A file present here with
	// a matching sha256 is skipped on resume (idempotent copy).
	CompletedFiles []FileProof `json:"completed_files,omitempty"`
	// VerifiedBytes is the sum of bytes across all CompletedFiles.
	VerifiedBytes int64 `json:"verified_bytes"`
	// VerifiedFiles is the count of CompletedFiles; kept separately so
	// callers can check progress without iterating the proof list.
	VerifiedFiles int `json:"verified_files"`
	// StartedAt is the RFC3339 UTC time the first attempt began.
	StartedAt string `json:"started_at,omitempty"`
	// UpdatedAt is the RFC3339 UTC time this status was last written.
	UpdatedAt string `json:"updated_at,omitempty"`
	// FailureSummary is a bounded (≤512 bytes) human-readable description
	// of the last failure. It is cleared when a new attempt starts.
	FailureSummary string `json:"failure_summary,omitempty"`
}

// ParseIngestStatus unmarshals and validates raw bytes as an IngestStatus.
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

// Validate checks required fields, known-valid state values, and internal
// consistency of the durable proof accounting. It is called on every write and
// on every parse so a malformed or tampered status can never be trusted.
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

	// Per-proof validation and duplicate detection.
	seen := make(map[string]struct{}, len(s.CompletedFiles))
	var sumBytes int64
	for i, p := range s.CompletedFiles {
		if p.Path == "" {
			return fmt.Errorf("ingest-status: completed_files[%d] has empty path", i)
		}
		if filepath.IsAbs(filepath.FromSlash(p.Path)) || hasTraversalSegment(p.Path) {
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

	// Totals must be consistent with the durable proofs so a caller can trust
	// verified_files/verified_bytes without walking the list.
	if s.VerifiedFiles != len(s.CompletedFiles) {
		return fmt.Errorf("ingest-status: verified_files=%d but completed_files has %d entries", s.VerifiedFiles, len(s.CompletedFiles))
	}
	if s.VerifiedBytes != sumBytes {
		return fmt.Errorf("ingest-status: verified_bytes=%d but completed_files sum to %d", s.VerifiedBytes, sumBytes)
	}
	if s.VerifiedFiles < 0 || s.VerifiedBytes < 0 {
		return fmt.Errorf("ingest-status: verified counters must be non-negative")
	}

	// Bounded failure summary.
	if len(s.FailureSummary) > 512 {
		return fmt.Errorf("ingest-status: failure_summary exceeds 512 bytes (%d)", len(s.FailureSummary))
	}

	// State/field consistency.
	if s.State == IngestStateFailed && s.FailureSummary == "" {
		return fmt.Errorf("ingest-status: failed state requires a failure_summary")
	}
	if s.State == IngestStateReady && s.FailureSummary != "" {
		return fmt.Errorf("ingest-status: ready state must not carry a failure_summary")
	}

	// Timestamp formats when present.
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

func hasTraversalSegment(p string) bool {
	for _, segment := range strings.Split(p, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

// isHex reports whether s is entirely lowercase/uppercase hexadecimal digits.
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

// Marshal serialises the status as indented JSON.
func (s IngestStatus) Marshal() ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}

// newRegisteredStatus creates an initial IngestStatus in the registered state.
// now is called for the started_at/updated_at timestamps; pass nil to use
// time.Now.
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
