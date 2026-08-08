// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dataset

import (
	"context"
	"fmt"
	"time"
)

// EnsureRegister is an idempotent variant of Register.
//
//   - If name@version is not yet registered, Register is called and
//     (record, true, nil) is returned.
//   - If name@version exists and its digest matches rec's computed digest,
//     the existing record is returned with (record, false, nil) — the call is a
//     no-op.
//   - If name@version exists but its digest differs from rec's computed digest,
//     an error is returned (drift detected; versions are immutable).
func (r *Registry) EnsureRegister(ctx context.Context, rec Record) (Record, bool, error) {
	// Fill in computed fields so the digest we compare against is canonical.
	if rec.SchemaVersion == 0 {
		rec.SchemaVersion = SchemaVersion
	}
	rec.TotalBytes = rec.SumBytes()
	rec.Digest = rec.ComputeDigest()

	existing, err := r.Get(ctx, rec.Name, rec.Version)
	if err == nil {
		// Record already exists.
		if existing.Digest != rec.Digest {
			return Record{}, false, fmt.Errorf(
				"dataset %s@%s already registered with digest %s; "+
					"supplied manifest has digest %s — drift detected (versions are immutable)",
				rec.Name, rec.Version, existing.Digest, rec.Digest,
			)
		}
		return existing, false, nil
	}
	// If error is anything other than "not found", surface it.
	if !IsNotExist(err) {
		return Record{}, false, err
	}
	registered, err := r.Register(ctx, rec)
	if err != nil {
		// Handle a race where two concurrent callers both see "not found" and
		// one wins the write. Retry the get to confirm their digest matches.
		if IsExist(err) {
			existing, gErr := r.Get(ctx, rec.Name, rec.Version)
			if gErr != nil {
				return Record{}, false, gErr
			}
			if existing.Digest != rec.Digest {
				return Record{}, false, fmt.Errorf(
					"dataset %s@%s registered concurrently with a different digest %s; "+
						"supplied digest %s — drift detected",
					rec.Name, rec.Version, existing.Digest, rec.Digest,
				)
			}
			return existing, false, nil
		}
		return Record{}, false, err
	}
	return registered, true, nil
}

// GetIngestStatus reads and parses the mutable ingest-status.json for
// name@version. Returns ErrNotExist (wrapped) if no status has been written.
// Returns an error if IngestStatusFile is not configured in the Paths.
func (r *Registry) GetIngestStatus(ctx context.Context, name, version string) (IngestStatus, error) {
	if r.paths.IngestStatusFile == nil {
		return IngestStatus{}, fmt.Errorf("ingest-status path not configured for this registry")
	}
	if err := ValidateName(name); err != nil {
		return IngestStatus{}, err
	}
	if err := ValidateVersion(version); err != nil {
		return IngestStatus{}, err
	}
	raw, err := r.backend.ReadFile(ctx, r.paths.IngestStatusFile(name, version))
	if err != nil {
		return IngestStatus{}, err // caller checks IsNotExist
	}
	return ParseIngestStatus(raw)
}

// WriteIngestStatus persists s as the current ingest status. The status file is
// always written with overwrite=true because it is the mutable companion to an
// immutable record. The caller is responsible for holding the version lock.
func (r *Registry) WriteIngestStatus(ctx context.Context, s IngestStatus) error {
	if r.paths.IngestStatusFile == nil {
		return fmt.Errorf("ingest-status path not configured for this registry")
	}
	if err := s.Validate(); err != nil {
		return err
	}
	s.UpdatedAt = r.now().UTC().Format(time.RFC3339)
	raw, err := s.Marshal()
	if err != nil {
		return err
	}
	path := r.paths.IngestStatusFile(s.Name, s.Version)
	if atomic, ok := r.backend.(AtomicWriteBackend); ok {
		return atomic.WriteFileAtomic(ctx, path, raw)
	}
	return r.backend.WriteFile(ctx, path, raw, true)
}

// InitIngestStatus writes an initial registered status for name@version if no
// status file exists yet. If a status already exists (any state), it is
// returned as-is without mutation. Created is true only when the status was
// newly written.
//
// This is called after Register/EnsureRegister so the ingest surface always
// starts from a known baseline.
func (r *Registry) InitIngestStatus(ctx context.Context, rec Record) (IngestStatus, bool, error) {
	existing, err := r.GetIngestStatus(ctx, rec.Name, rec.Version)
	if err == nil {
		return existing, false, nil
	}
	if !IsNotExist(err) {
		return IngestStatus{}, false, err
	}
	status := newRegisteredStatus(rec, r.now)
	if wErr := r.WriteIngestStatus(ctx, status); wErr != nil {
		return IngestStatus{}, false, wErr
	}
	return status, true, nil
}
