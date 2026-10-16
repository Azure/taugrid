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

var (
	ErrNotExist = errors.New("dataset registry: path does not exist")
	ErrExist    = errors.New("dataset registry: path already exists")
)

func IsNotExist(err error) bool { return errors.Is(err, ErrNotExist) }

func IsExist(err error) bool { return errors.Is(err, ErrExist) }

type Backend interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte, overwrite bool) error
	List(ctx context.Context, dir string) ([]string, error)
	Delete(ctx context.Context, path string) error
}

type AtomicWriteBackend interface {
	WriteFileAtomic(ctx context.Context, path string, data []byte) error
}

type Paths struct {
	DatasetsDir      func() string
	DatasetDir       func(name string) string
	VersionDir       func(name, version string) string
	RecordFile       func(name, version string) string
	AliasesDir       func(name string) string
	AliasFile        func(name, alias string) string
	IngestStatusFile func(name, version string) string
}

type Registry struct {
	backend Backend
	paths   Paths
	now     func() time.Time
}

func NewRegistry(backend Backend, paths Paths, now func() time.Time) *Registry {
	if now == nil {
		now = time.Now
	}
	return &Registry{backend: backend, paths: paths, now: now}
}

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
			return Record{}, fmt.Errorf("dataset %s@%s not found: %w", name, version, ErrNotExist)
		}
		return Record{}, err
	}
	return ParseRecord(raw)
}

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

func aliasesDirName(p Paths, name string) string {
	full := p.AliasesDir(name)
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[i+1:]
	}
	return full
}

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

type SetAliasOptions struct {
	Expect       string
	ExpectAbsent bool
}

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

func (r *Registry) Resolve(ctx context.Context, ref Ref) (Record, error) {
	if ref.Version != "" {
		rec, err := r.Get(ctx, ref.Name, ref.Version)
		if err == nil {
			return rec, nil
		}
		if !IsNotExist(err) {
			return Record{}, err
		}
		alias, aErr := r.GetAlias(ctx, ref.Name, ref.Version)
		if aErr != nil {
			return Record{}, err
		}
		return r.Get(ctx, ref.Name, alias.Version)
	}
	alias, err := r.GetAlias(ctx, ref.Name, ref.Alias)
	if err != nil {
		return Record{}, err
	}
	return r.Get(ctx, ref.Name, alias.Version)
}

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
