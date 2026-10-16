// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dataset

import (
	"context"
	"fmt"
	"time"
)

func (r *Registry) EnsureRegister(ctx context.Context, rec Record) (Record, bool, error) {
	if rec.SchemaVersion == 0 {
		rec.SchemaVersion = SchemaVersion
	}
	rec.TotalBytes = rec.SumBytes()
	rec.Digest = rec.ComputeDigest()

	existing, err := r.Get(ctx, rec.Name, rec.Version)
	if err == nil {
		if existing.Digest != rec.Digest {
			return Record{}, false, fmt.Errorf(
				"dataset %s@%s already registered with digest %s; "+
					"supplied manifest has digest %s — drift detected (versions are immutable)",
				rec.Name, rec.Version, existing.Digest, rec.Digest,
			)
		}
		return existing, false, nil
	}
	if !IsNotExist(err) {
		return Record{}, false, err
	}
	registered, err := r.Register(ctx, rec)
	if err != nil {
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
		return IngestStatus{}, err
	}
	return ParseIngestStatus(raw)
}

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
