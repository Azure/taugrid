// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package dataset implements the tau dataset registry: an immutable,
// subscription-scoped catalog of pre-training and RL datasets.
//
// The registry is a control plane. A record (dataset.json) is a small JSON
// document describing what a dataset is, where its bytes live, and how to
// verify and consume them. The bytes themselves live in a dedicated dataset
// storage account (referenced by account/container/prefix); the registry never
// moves payloads. Records are immutable once registered; only aliases move.
//
// The schema is a common envelope plus exactly one purpose-specific section
// (pretrain | rl | eval) so RL data (prompts / preference pairs / trajectories)
// is not forced into a pretraining-token-shard shape.
package dataset

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// SchemaVersion is the current dataset record schema version.
const SchemaVersion = 1

// Purposes.
const (
	PurposePretrain = "pretrain"
	PurposeRL       = "rl"
	PurposeEval     = "eval"
)

// Assurance levels describe how much the registry trusts a record's hashes and
// counts. They are explicit so callers never silently trust user-supplied data.
const (
	// AssuranceVerified means the registry computed and checked the file
	// hashes (and, for known formats, the token counts) itself.
	AssuranceVerified = "verified"
	// AssuranceManifestSupplied means the caller provided a manifest and the
	// registry recorded it (optionally sampling); counts are caller-asserted.
	AssuranceManifestSupplied = "manifest-supplied"
	// AssuranceTrusted means the record was imported with lower assurance.
	AssuranceTrusted = "trusted"
)

// FormatTokenizedBinUint16 is a raw little-endian uint16 token stream (the
// nanoGPT / FineWeb tokenized .bin format). For this format under
// AssuranceVerified the registry requires token_count == bytes/2.
const FormatTokenizedBinUint16 = "tokenized-bin-uint16"

// Source captures dataset provenance.
type Source struct {
	Kind     string `json:"kind,omitempty"`
	Repo     string `json:"repo,omitempty"`
	Revision string `json:"revision,omitempty"`
	Config   string `json:"config,omitempty"`
}

// File is one immutable member of a dataset, addressed relative to prefix.
type File struct {
	Path       string `json:"path"`
	Bytes      int64  `json:"bytes"`
	SHA256     string `json:"sha256"`
	TokenCount int64  `json:"token_count,omitempty"`
	Source     string `json:"source,omitempty"`
	Domain     string `json:"domain,omitempty"`
	Split      string `json:"split,omitempty"`
}

// Component records source-level provenance and the held-out split assigned to
// every file from that source.
type Component struct {
	Source     string `json:"source"`
	Domain     string `json:"domain"`
	Split      string `json:"split"`
	License    string `json:"license"`
	Provenance string `json:"provenance"`
}

// Pretrain is the purpose section for pre-training corpora.
type Pretrain struct {
	Tokenizer       string `json:"tokenizer,omitempty"`
	Format          string `json:"format,omitempty"`
	TotalTokens     int64  `json:"total_tokens,omitempty"`
	SequencePacking bool   `json:"sequence_packing,omitempty"`
}

// RL is the purpose section for reinforcement-learning datasets.
type RL struct {
	// Kind is one of: prompts | preferences | trajectories.
	Kind        string `json:"kind,omitempty"`
	PolicyRun   string `json:"policy_run,omitempty"`
	RewardModel string `json:"reward_model,omitempty"`
	Schema      string `json:"schema,omitempty"`
}

// Eval is the purpose section for evaluation datasets.
type Eval struct {
	Task   string `json:"task,omitempty"`
	Split  string `json:"split,omitempty"`
	Metric string `json:"metric,omitempty"`
}

// Record is a single immutable dataset version record.
type Record struct {
	SchemaVersion int               `json:"schema_version"`
	Name          string            `json:"name"`
	Version       string            `json:"version"`
	Purpose       string            `json:"purpose"`
	Source        Source            `json:"source,omitempty"`
	Account       string            `json:"account,omitempty"`
	Container     string            `json:"container,omitempty"`
	Prefix        string            `json:"prefix,omitempty"`
	Files         []File            `json:"files"`
	TotalBytes    int64             `json:"total_bytes"`
	Digest        string            `json:"digest"`
	Assurance     string            `json:"assurance"`
	CreatedAt     string            `json:"created_at,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	Components    []Component       `json:"components,omitempty"`

	// Exactly one of the following is set, matching Purpose.
	Pretrain *Pretrain `json:"pretrain,omitempty"`
	RL       *RL       `json:"rl,omitempty"`
	Eval     *Eval     `json:"eval,omitempty"`
}

// AliasRecord is a movable pointer from an alias to a pinned dataset version.
type AliasRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Alias         string `json:"alias"`
	Version       string `json:"version"`
	Digest        string `json:"digest,omitempty"`
	RecordPath    string `json:"record_path,omitempty"`
	UpdatedAt     string `json:"updated_at"`
}

// ComputeDigest returns the content-addressed identifier for a record's file
// list: sha256 over newline-joined "<sha256>  <path>" lines sorted by line. It
// is independent of file ordering, so the same bytes always yield the same
// digest.
func (r *Record) ComputeDigest() string {
	lines := make([]string, 0, len(r.Files))
	for _, f := range r.Files {
		lines = append(lines, strings.ToLower(f.SHA256)+"  "+f.Path)
	}
	sort.Strings(lines)
	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SumBytes returns the total size across all files.
func (r *Record) SumBytes() int64 {
	var total int64
	for _, f := range r.Files {
		total += f.Bytes
	}
	return total
}

// Marshal returns canonical record bytes (stable indentation + trailing
// newline) for writing a record to the registry.
func (r *Record) Marshal() ([]byte, error) {
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// ParseRecord decodes and structurally validates a record's JSON.
func ParseRecord(raw []byte) (Record, error) {
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("parse dataset record: %w", err)
	}
	if err := rec.Validate(); err != nil {
		return Record{}, err
	}
	return rec, nil
}

// ParseAlias decodes and validates an alias record's JSON.
func ParseAlias(raw []byte) (AliasRecord, error) {
	var a AliasRecord
	if err := json.Unmarshal(raw, &a); err != nil {
		return AliasRecord{}, fmt.Errorf("parse dataset alias: %w", err)
	}
	if a.Name == "" || a.Alias == "" || a.Version == "" {
		return AliasRecord{}, fmt.Errorf("dataset alias missing name/alias/version")
	}
	return a, nil
}

// Validate enforces the record invariants: required envelope fields, a single
// purpose section matching purpose, canonical file paths, byte/digest
// consistency, and assurance-gated token accounting.
func (r *Record) Validate() error {
	if r.SchemaVersion == 0 {
		return fmt.Errorf("schema_version is required")
	}
	if err := ValidateName(r.Name); err != nil {
		return err
	}
	if err := ValidateVersion(r.Version); err != nil {
		return err
	}
	switch r.Purpose {
	case PurposePretrain, PurposeRL, PurposeEval:
	default:
		return fmt.Errorf("purpose %q must be one of: pretrain, rl, eval", r.Purpose)
	}
	if err := r.validatePurposeSection(); err != nil {
		return err
	}
	switch r.Assurance {
	case AssuranceVerified, AssuranceManifestSupplied, AssuranceTrusted:
	default:
		return fmt.Errorf("assurance %q must be one of: verified, manifest-supplied, trusted", r.Assurance)
	}
	if len(r.Files) == 0 {
		return fmt.Errorf("dataset %s@%s has no files", r.Name, r.Version)
	}
	seen := map[string]struct{}{}
	for i, f := range r.Files {
		if err := validateFilePath(f.Path); err != nil {
			return fmt.Errorf("files[%d]: %w", i, err)
		}
		if _, dup := seen[f.Path]; dup {
			return fmt.Errorf("files[%d]: duplicate path %q", i, f.Path)
		}
		seen[f.Path] = struct{}{}
		if f.Bytes <= 0 {
			return fmt.Errorf("files[%d] (%s): bytes must be > 0", i, f.Path)
		}
		if !isHex64(f.SHA256) {
			return fmt.Errorf("files[%d] (%s): sha256 must be 64 hex chars", i, f.Path)
		}
	}
	if want := r.SumBytes(); r.TotalBytes != want {
		return fmt.Errorf("total_bytes=%d does not match sum of files=%d", r.TotalBytes, want)
	}
	if computed := r.ComputeDigest(); r.Digest != computed {
		return fmt.Errorf("digest %q does not match computed %q", r.Digest, computed)
	}
	if err := r.validateComponents(); err != nil {
		return err
	}
	return r.validateTokenAccounting()
}

func (r *Record) validateComponents() error {
	if len(r.Components) == 0 {
		for i, f := range r.Files {
			if f.Source != "" || f.Domain != "" || f.Split != "" {
				return fmt.Errorf("files[%d] (%s): source/domain/split metadata requires record components", i, f.Path)
			}
		}
		return nil
	}

	components := make(map[string]Component, len(r.Components))
	used := make(map[string]bool, len(r.Components))
	for i, c := range r.Components {
		if c.Source == "" || c.Domain == "" || c.License == "" || c.Provenance == "" {
			return fmt.Errorf("components[%d]: source, domain, license, and provenance are required", i)
		}
		if !validDatasetSplit(c.Split) {
			return fmt.Errorf("components[%d] (%s): split %q must be one of: train, validation, test", i, c.Source, c.Split)
		}
		if _, duplicate := components[c.Source]; duplicate {
			return fmt.Errorf("components[%d]: duplicate source %q", i, c.Source)
		}
		components[c.Source] = c
	}
	for i, f := range r.Files {
		if f.Source == "" || f.Domain == "" || f.Split == "" {
			return fmt.Errorf("files[%d] (%s): source, domain, and split are required when record components are present", i, f.Path)
		}
		c, ok := components[f.Source]
		if !ok {
			return fmt.Errorf("files[%d] (%s): source %q has no matching component", i, f.Path, f.Source)
		}
		if f.Domain != c.Domain || f.Split != c.Split {
			return fmt.Errorf("files[%d] (%s): domain/split %s/%s does not match component %s/%s", i, f.Path, f.Domain, f.Split, c.Domain, c.Split)
		}
		used[f.Source] = true
	}
	for source := range components {
		if !used[source] {
			return fmt.Errorf("component source %q has no files", source)
		}
	}
	return nil
}

func validDatasetSplit(split string) bool {
	switch split {
	case "train", "validation", "test":
		return true
	default:
		return false
	}
}

func (r *Record) validatePurposeSection() error {
	set := 0
	if r.Pretrain != nil {
		set++
	}
	if r.RL != nil {
		set++
	}
	if r.Eval != nil {
		set++
	}
	if set != 1 {
		return fmt.Errorf("exactly one purpose section (pretrain|rl|eval) must be present, found %d", set)
	}
	switch r.Purpose {
	case PurposePretrain:
		if r.Pretrain == nil {
			return fmt.Errorf("purpose=pretrain requires a pretrain section")
		}
	case PurposeRL:
		if r.RL == nil {
			return fmt.Errorf("purpose=rl requires an rl section")
		}
		switch r.RL.Kind {
		case "prompts", "preferences", "trajectories":
		default:
			return fmt.Errorf("rl.kind %q must be one of: prompts, preferences, trajectories", r.RL.Kind)
		}
	case PurposeEval:
		if r.Eval == nil {
			return fmt.Errorf("purpose=eval requires an eval section")
		}
	}
	return nil
}

// validateTokenAccounting enforces that verified pretraining records have
// internally consistent token counts for known formats. Caller-asserted counts
// are only permitted when assurance is not "verified".
func (r *Record) validateTokenAccounting() error {
	if r.Purpose != PurposePretrain || r.Pretrain == nil {
		return nil
	}
	if r.Pretrain.Format != FormatTokenizedBinUint16 {
		return nil
	}
	var sumTokens int64
	for i, f := range r.Files {
		if r.Assurance == AssuranceVerified {
			if f.Bytes%2 != 0 {
				return fmt.Errorf("files[%d] (%s): %s requires even byte length, got %d", i, f.Path, FormatTokenizedBinUint16, f.Bytes)
			}
			if want := f.Bytes / 2; f.TokenCount != want {
				return fmt.Errorf("files[%d] (%s): verified %s requires token_count=bytes/2=%d, got %d", i, f.Path, FormatTokenizedBinUint16, want, f.TokenCount)
			}
		}
		sumTokens += f.TokenCount
	}
	if r.Pretrain.TotalTokens != 0 && r.Pretrain.TotalTokens != sumTokens {
		return fmt.Errorf("pretrain.total_tokens=%d does not match sum of file token_count=%d", r.Pretrain.TotalTokens, sumTokens)
	}
	return nil
}

// ValidateName checks a dataset name slug.
func ValidateName(name string) error {
	return validateSlug("name", name)
}

// ValidateVersion checks a dataset version slug (allows dots, e.g. v1.2).
func ValidateVersion(version string) error {
	if version == "" {
		return fmt.Errorf("version: required")
	}
	if strings.TrimSpace(version) != version || strings.HasPrefix(version, "-") || strings.HasSuffix(version, "-") {
		return fmt.Errorf("version %q is invalid (use lowercase alphanumerics, hyphens, dots)", version)
	}
	for _, c := range version {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '.' {
			continue
		}
		return fmt.Errorf("version %q is invalid (use lowercase alphanumerics, hyphens, dots)", version)
	}
	return nil
}

// ValidateAlias checks an alias slug.
func ValidateAlias(alias string) error {
	return validateSlug("alias", alias)
}

func validateSlug(kind, value string) error {
	if value == "" {
		return fmt.Errorf("%s: required", kind)
	}
	if strings.TrimSpace(value) != value || strings.HasPrefix(value, "-") || strings.HasSuffix(value, "-") {
		return fmt.Errorf("%s %q is invalid (use lowercase alphanumerics with internal hyphens)", kind, value)
	}
	for _, c := range value {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' {
			continue
		}
		return fmt.Errorf("%s %q is invalid (use lowercase alphanumerics with internal hyphens)", kind, value)
	}
	return nil
}

// validateFilePath enforces that a file path is a clean, relative blob path
// under the dataset prefix: no leading slash, no "..", no "." segments, no
// duplicate slashes. This guards both digest stability and node-local staging.
func validateFilePath(p string) error {
	if p == "" {
		return fmt.Errorf("path is required")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("path %q must be relative (no leading slash)", p)
	}
	if path.Clean(p) != p {
		return fmt.Errorf("path %q is not canonical (clean form is %q)", p, path.Clean(p))
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("path %q has an invalid segment", p)
		}
	}
	return nil
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') {
			continue
		}
		return false
	}
	return true
}
