// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dataset

import (
	"fmt"
	"path"
	"sort"
)

// Consumption mode names emitted in a ResolvedReference.
const (
	ModeDurableMount   = "durable_mount"
	ModeHotCache       = "hot_cache"
	ModeNodeLocalStage = "node_local_stage"
)

// StageFile is one shard in the node-local staging contract. It carries exactly
// what a consumer needs to download (relative path under prefix) and verify
// (sha256, bytes) a shard onto node-local NVMe.
type StageFile struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	TokenCount int64  `json:"token_count,omitempty"`
	Source     string `json:"source,omitempty"`
	Domain     string `json:"domain,omitempty"`
	Split      string `json:"split,omitempty"`
}

// NodeLocalStage is the verified shard manifest a 16-GPU pretraining job uses to
// stage shards to NVMe itself (identity-based download against the dataset
// account; never SAS). account/container/prefix locate the bytes.
type NodeLocalStage struct {
	Account   string      `json:"account,omitempty"`
	Container string      `json:"container,omitempty"`
	Prefix    string      `json:"prefix,omitempty"`
	Files     []StageFile `json:"files"`
}

// ResolvedReference is what `tau data dataset ref` emits: a pinned, content-digest
// reference plus every viable consumption mode. The registry is a control
// plane; it describes modes and recommends one, but does not force a read path.
type ResolvedReference struct {
	Name        string            `json:"name"`
	Version     string            `json:"version"`
	Purpose     string            `json:"purpose"`
	Digest      string            `json:"digest"`
	Manifest    string            `json:"manifest"`
	Modes       ReferenceModes    `json:"modes"`
	Recommended string            `json:"recommended"`
	Tags        map[string]string `json:"tags,omitempty"`
	Components  []Component       `json:"components,omitempty"`
}

// ReferenceModes enumerates the consumption mechanisms for a dataset version.
type ReferenceModes struct {
	DurableMount   string         `json:"durable_mount"`
	HotCache       string         `json:"hot_cache"`
	NodeLocalStage NodeLocalStage `json:"node_local_stage"`
}

// ReferenceOptions carries mount roots so this package stays decoupled from the
// storage path constants. The CLI passes the real storage roots.
type ReferenceOptions struct {
	// DurableDatasetsDir is the blobfuse durable datasets root (e.g. /data/datasets).
	DurableDatasetsDir string
	// HotDatasetsDir is the hot-FS datasets root (e.g. /mnt/datasets).
	HotDatasetsDir string
	// ManifestPath is the durable path of the record (dataset.json).
	ManifestPath string
}

// BuildReference produces the resolved reference for a record. node_local_stage
// is recommended for pretraining (sustained 16-GPU throughput needs per-rank
// node-local reads, not a shared FUSE mount); other purposes recommend the
// durable mount, which is fine for small/eval datasets.
func BuildReference(rec Record, opts ReferenceOptions) ResolvedReference {
	files := make([]StageFile, 0, len(rec.Files))
	for _, f := range rec.Files {
		files = append(files, StageFile{
			Path:       f.Path,
			SHA256:     f.SHA256,
			Bytes:      f.Bytes,
			TokenCount: f.TokenCount,
			Source:     f.Source,
			Domain:     f.Domain,
			Split:      f.Split,
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })

	durableMount := opts.DurableDatasetsDir
	if rec.Prefix != "" {
		durableMount = path.Join(opts.DurableDatasetsDir, rec.Prefix)
	}
	hotCache := path.Join(opts.HotDatasetsDir, rec.Name, rec.Version)

	recommended := ModeDurableMount
	if rec.Purpose == PurposePretrain {
		recommended = ModeNodeLocalStage
	}

	return ResolvedReference{
		Name:     rec.Name,
		Version:  rec.Version,
		Purpose:  rec.Purpose,
		Digest:   rec.Digest,
		Manifest: opts.ManifestPath,
		Modes: ReferenceModes{
			DurableMount: durableMount,
			HotCache:     hotCache,
			NodeLocalStage: NodeLocalStage{
				Account:   rec.Account,
				Container: rec.Container,
				Prefix:    rec.Prefix,
				Files:     files,
			},
		},
		Recommended: recommended,
		Tags:        rec.Tags,
		Components:  rec.Components,
	}
}

// BlobURL returns the canonical (identity-authenticated) blob URL for a staged
// file: https://<account>.blob.core.windows.net/<container>/<prefix>/<path>.
// It is NOT a SAS URL; callers must authenticate with workload identity.
func (n NodeLocalStage) BlobURL(file StageFile) (string, error) {
	if n.Account == "" || n.Container == "" {
		return "", fmt.Errorf("node_local_stage is missing account/container; cannot form blob URL")
	}
	full := file.Path
	if n.Prefix != "" {
		full = path.Join(n.Prefix, file.Path)
	}
	return fmt.Sprintf("https://%s.blob.core.windows.net/%s/%s", n.Account, n.Container, full), nil
}
