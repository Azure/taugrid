// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dataset

import (
	"fmt"
	"path"
	"sort"
)

const (
	ModeDurableMount   = "durable_mount"
	ModeHotCache       = "hot_cache"
	ModeNodeLocalStage = "node_local_stage"
)

type StageFile struct {
	Path       string `json:"path"`
	SHA256     string `json:"sha256"`
	Bytes      int64  `json:"bytes"`
	TokenCount int64  `json:"token_count,omitempty"`
	Source     string `json:"source,omitempty"`
	Domain     string `json:"domain,omitempty"`
	Split      string `json:"split,omitempty"`
}

type NodeLocalStage struct {
	Account   string      `json:"account,omitempty"`
	Container string      `json:"container,omitempty"`
	Prefix    string      `json:"prefix,omitempty"`
	Files     []StageFile `json:"files"`
}

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

type ReferenceModes struct {
	DurableMount   string         `json:"durable_mount"`
	HotCache       string         `json:"hot_cache"`
	NodeLocalStage NodeLocalStage `json:"node_local_stage"`
}

type ReferenceOptions struct {
	DurableDatasetsDir string
	HotDatasetsDir     string
	ManifestPath       string
}

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
