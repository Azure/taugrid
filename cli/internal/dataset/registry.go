// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dataset

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Sentinel errors a Backend reports so the registry can enforce immutability
// (refuse overwrite) and tolerate missing paths.
var (
	// ErrNotExist is returned by a Backend when a path is absent.
	ErrNotExist = errors.New("dataset registry: path does not exist")
	// ErrExist is returned by WriteFile when overwrite is false and the path
	// already exists. This is how immutability is enforced.
	ErrExist = errors.New("dataset registry: path already exists")
)

// IsNotExist reports whether err indicates a missing path.
func IsNotExist(err error) bool { return errors.Is(err, ErrNotExist) }

// IsExist reports whether err indicates an existing path.
func IsExist(err error) bool { return errors.Is(err, ErrExist) }

// Backend is the durable store the registry reads and writes small JSON records
// through. The pvc (helper-pod) and in-memory test implementations both satisfy
// it; an az-blob implementation can provide server-enforced conditional writes.
type Backend interface {
	// ReadFile returns the bytes at path or an error satisfying IsNotExist.
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile writes data at path. When overwrite is false and the path
	// already exists it returns an error satisfying IsExist.
	WriteFile(ctx context.Context, path string, data []byte, overwrite bool) error
	// List returns the immediate child names under dir, or an error
	// satisfying IsNotExist when dir is absent.
	List(ctx context.Context, dir string) ([]string, error)
	// Delete removes the file at path. Deleting a missing path is not an error.
	Delete(ctx context.Context, path string) error
}

// AtomicWriteBackend is an optional capability for mutable registry companions.
// Immutable records keep using Backend.WriteFile with overwrite=false; callers
// use this only where readers must never observe a partial replacement.
type AtomicWriteBackend interface {
	WriteFileAtomic(ctx context.Context, path string, data []byte) error
}

// Paths abstracts the registry layout so the Registry does not depend on the
// storage package directly (keeping this package import-cycle free). The CLI
// wires the real storage.DatasetRegistry* helpers.
type Paths struct {
	DatasetsDir func() string
	DatasetDir  func(name string) string
	VersionDir  func(name, version string) string
	RecordFile  func(name, version string) string
	AliasesDir  func(name string) string
	AliasFile   func(name, alias string) string
	// IngestStatusFile is the mutable per-version ingest progress companion.
	// It is optional: set it when ingest operations are needed; commands that
	// only read/write records may leave it nil.
	IngestStatusFile func(name, version string) string
}

// Registry is the dataset registry client over a Backend.
type Registry struct {
	backend Backend
	paths   Paths
	now     func() time.Time
}

// NewRegistry constructs a Registry. now may be nil (defaults to time.Now).
func NewRegistry(backend Backend, paths Paths, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{backend: backend, paths: paths, now: now}
}

// Register writes an immutable record for rec.Name@rec.Version. It refuses to
// overwrite an existing version (immutability). The record is validated and its
// digest/total_bytes are filled in from the file list before writing.
func (r *Registry) Register(ctx context.Context, rec Record) (Record, error) {
	if rec.SchemaVersion == 0 {
		rec.SchemaVersion = SchemaVersion
	}
	rec.TotalBytes = rec.SumBytes()
	rec.Digest = rec.ComputeDigest()
	if rec.CreatedAt == "" {
		rec.CreatedAt = r.now().UTC().Format(time.RFC3339)
	}
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	raw, err := rec.Marshal()
	if err != nil {
		return Record{}, err
	}
	path := r.paths.RecordFile(rec.Name, rec.Version)
	if err := r.backend.WriteFile(ctx, path, raw, false); err != nil {
		if IsExist(err) {
			return Record{}, fmt.Errorf("dataset %s@%s already registered (versions are immutable; register a new version)", rec.Name, rec.Version)
		}
		return Record{}, err
	}
	return rec, nil
}

// Get reads and validates the record for name@version.
func (r *Registry) Get(ctx context.Context, name, version string) (Record, error) {
	if err := ValidateName(name); err != nil {
		return Record{}, err
	}
	if err := ValidateVersion(version); err != nil {
		return Record{}, err
	}
	raw, err := r.backend.ReadFile(ctx, r.paths.RecordFile(name, version))
	if err != nil {
		if IsNotExist(err) {
			// Wrap the sentinel so callers can use IsNotExist while the
			// human-readable "not found" text is preserved for messages.
			return Record{}, fmt.Errorf("dataset %s@%s not found: %w", name, version, ErrNotExist)
		}
		return Record{}, err
	}
	return ParseRecord(raw)
}

// ListVersions returns the registered versions for a dataset name.
func (r *Registry) ListVersions(ctx context.Context, name string) ([]string, error) {
	if err := ValidateName(name); err != nil {
		return nil, err
	}
	entries, err := r.backend.List(ctx, r.paths.DatasetDir(name))
	if err != nil {
		if IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var versions []string
	for _, e := range entries {
		e = strings.TrimSuffix(e, "/")
		if e == "" || e == aliasesDirName(r.paths, name) {
			continue
		}
		versions = append(versions, e)
	}
	sort.Strings(versions)
	return versions, nil
}

// aliasesDirName returns the base name of the aliases directory so ListVersions
// can skip it when enumerating version directories.
func aliasesDirName(p Paths, name string) string {
	full := p.AliasesDir(name)
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}

// List returns all records, optionally filtered by purpose and tag equality.
// Records that fail to parse are skipped and reported as warnings.
func (r *Registry) List(ctx context.Context, purpose string, tags map[string]string) ([]Record, []error, error) {
	names, err := r.backend.List(ctx, r.paths.DatasetsDir())
	if err != nil {
		if IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	var records []Record
	var warnings []error
	for _, name := range names {
		name = strings.TrimSuffix(name, "/")
		if name == "" {
			continue
		}
		versions, err := r.ListVersions(ctx, name)
		if err != nil {
			warnings = append(warnings, fmt.Errorf("list versions for %s: %w", name, err))
			continue
		}
		for _, version := range versions {
			rec, err := r.Get(ctx, name, version)
			if err != nil {
				warnings = append(warnings, fmt.Errorf("read %s@%s: %w", name, version, err))
				continue
			}
			if purpose != "" && rec.Purpose != purpose {
				continue
			}
			if !tagsMatch(rec.Tags, tags) {
				continue
			}
			records = append(records, rec)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Name != records[j].Name {
			return records[i].Name < records[j].Name
		}
		return records[i].Version < records[j].Version
	})
	return records, warnings, nil
}

func tagsMatch(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

// GetAlias reads and validates the alias pointer name:alias.
func (r *Registry) GetAlias(ctx context.Context, name, alias string) (AliasRecord, error) {
	if err := ValidateName(name); err != nil {
		return AliasRecord{}, err
	}
	if err := ValidateAlias(alias); err != nil {
		return AliasRecord{}, err
	}
	raw, err := r.backend.ReadFile(ctx, r.paths.AliasFile(name, alias))
	if err != nil {
		if IsNotExist(err) {
			return AliasRecord{}, fmt.Errorf("alias %s:%s not found: %w", name, alias, ErrNotExist)
		}
		return AliasRecord{}, err
	}
	return ParseAlias(raw)
}

// SetAliasOptions controls the compare-and-swap behavior of SetAlias.
type SetAliasOptions struct {
	// Expect, when non-empty, requires the alias to currently point at this
	// version (compare-and-swap). Mismatch fails without writing.
	Expect string
	// ExpectAbsent requires the alias to not yet exist.
	ExpectAbsent bool
}

// SetAlias points name:alias at version. The target version must already exist.
// When opts requests it, the update is a compare-and-swap against the current
// alias value. Aliases are mutable, so the write itself overwrites.
func (r *Registry) SetAlias(ctx context.Context, name, alias, version string, opts SetAliasOptions) (AliasRecord, error) {
	if err := ValidateName(name); err != nil {
		return AliasRecord{}, err
	}
	if err := ValidateAlias(alias); err != nil {
		return AliasRecord{}, err
	}
	target, err := r.Get(ctx, name, version)
	if err != nil {
		return AliasRecord{}, err
	}
	current, err := r.GetAlias(ctx, name, alias)
	currentExists := err == nil
	if err != nil && !IsNotExist(err) {
		return AliasRecord{}, err
	}
	if opts.ExpectAbsent && currentExists {
		return AliasRecord{}, fmt.Errorf("alias %s:%s already exists (points at %s); --expect-absent not satisfied", name, alias, current.Version)
	}
	if opts.Expect != "" {
		if !currentExists {
			return AliasRecord{}, fmt.Errorf("alias %s:%s does not exist; --expect %s not satisfied", name, alias, opts.Expect)
		}
		if current.Version != opts.Expect {
			return AliasRecord{}, fmt.Errorf("alias %s:%s points at %s, not %s; compare-and-swap aborted", name, alias, current.Version, opts.Expect)
		}
	}
	record := AliasRecord{
		SchemaVersion: SchemaVersion,
		Name:          name,
		Alias:         alias,
		Version:       version,
		Digest:        target.Digest,
		RecordPath:    r.paths.RecordFile(name, version),
		UpdatedAt:     r.now().UTC().Format(time.RFC3339),
	}
	raw, err := marshalJSON(record)
	if err != nil {
		return AliasRecord{}, err
	}
	if err := r.backend.WriteFile(ctx, r.paths.AliasFile(name, alias), raw, true); err != nil {
		return AliasRecord{}, err
	}
	return record, nil
}

// Resolve resolves a parsed reference to a concrete record. A bare name uses
// the "latest" alias; name@token tries an exact version first, then an alias.
func (r *Registry) Resolve(ctx context.Context, ref Ref) (Record, error) {
	if ref.Version != "" {
		rec, err := r.Get(ctx, ref.Name, ref.Version)
		if err == nil {
			return rec, nil
		}
		if !IsNotExist(err) {
			return Record{}, err
		}
		// Fall back to treating the token as an alias.
		alias, aErr := r.GetAlias(ctx, ref.Name, ref.Version)
		if aErr != nil {
			return Record{}, err // original "version not found" is the better message
		}
		return r.Get(ctx, ref.Name, alias.Version)
	}
	alias, err := r.GetAlias(ctx, ref.Name, ref.Alias)
	if err != nil {
		return Record{}, err
	}
	return r.Get(ctx, ref.Name, alias.Version)
}

// Remove deletes name@version. It refuses while any alias points at the
// version, and aborts (without deleting) if the alias scan cannot complete.
func (r *Registry) Remove(ctx context.Context, name, version string) error {
	if _, err := r.Get(ctx, name, version); err != nil {
		return err
	}
	aliasEntries, err := r.backend.List(ctx, r.paths.AliasesDir(name))
	if err != nil && !IsNotExist(err) {
		return fmt.Errorf("cannot verify aliases for %s before removal: %w", name, err)
	}
	for _, e := range aliasEntries {
		if !strings.HasSuffix(e, ".json") {
			continue
		}
		alias := strings.TrimSuffix(e, ".json")
		a, err := r.GetAlias(ctx, name, alias)
		if err != nil {
			return fmt.Errorf("cannot verify alias %s:%s before removal: %w", name, alias, err)
		}
		if a.Version == version {
			return fmt.Errorf("dataset %s@%s is referenced by alias %q; move or delete the alias first", name, version, alias)
		}
	}
	return r.backend.Delete(ctx, r.paths.RecordFile(name, version))
}

// Verify recomputes a record's digest from its (already-recorded) file hashes
// and checks total_bytes. It does not re-download bytes; byte-level
// re-hashing is the CLI verify command's job via the data source.
func (r *Registry) Verify(ctx context.Context, name, version string) (Record, error) {
	rec, err := r.Get(ctx, name, version)
	if err != nil {
		return Record{}, err
	}
	if computed := rec.ComputeDigest(); computed != rec.Digest {
		return Record{}, fmt.Errorf("dataset %s@%s digest mismatch: record=%s computed=%s", name, version, rec.Digest, computed)
	}
	if sum := rec.SumBytes(); sum != rec.TotalBytes {
		return Record{}, fmt.Errorf("dataset %s@%s total_bytes mismatch: record=%d computed=%d", name, version, rec.TotalBytes, sum)
	}
	return rec, nil
}
