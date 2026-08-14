// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package artifactoffload

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/portal/internal/blobstore"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

const CheckpointSchemaVersion = "tau.artifact.upload.v1"

var ErrPartialFailure = errors.New("one or more artifacts failed to offload")

type PartialFailureError struct {
	Failed int
}

func (e PartialFailureError) Error() string {
	if e.Failed == 1 {
		return "1 artifact failed to offload"
	}
	return fmt.Sprintf("%d artifacts failed to offload", e.Failed)
}

func (e PartialFailureError) Unwrap() error {
	return ErrPartialFailure
}

func IsPartialFailure(err error) bool {
	return errors.Is(err, ErrPartialFailure)
}

type Options struct {
	RunID         string
	Out           string
	Checkpoint    string
	ObjectStore   blobstore.Store
	ObjectBaseURI string
	Account       string
	Container     string
	MaxSizeBytes  int64
}

type Result struct {
	SchemaVersion string           `json:"schema_version"`
	RunID         string           `json:"run_id"`
	Artifacts     []ArtifactResult `json:"artifacts"`
	Uploaded      int              `json:"uploaded"`
	Deduped       int              `json:"deduped"`
	Skipped       int              `json:"skipped"`
	Verified      int              `json:"verified"`
	Indexed       int              `json:"indexed"`
	Failed        int              `json:"failed"`
	Checkpoint    string           `json:"checkpoint"`
	Completed     bool             `json:"completed,omitempty"`
}

type ArtifactResult struct {
	ArtifactID  string `json:"artifact_id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	SourceURI   string `json:"source_uri"`
	ObjectURI   string `json:"object_uri,omitempty"`
	Digest      string `json:"digest,omitempty"`
	SizeBytes   int64  `json:"size_bytes,omitempty"`
	Status      string `json:"status"`
	DedupedFrom string `json:"deduped_from,omitempty"`
	Error       string `json:"error,omitempty"`
}

type Checkpoint struct {
	SchemaVersion string                     `json:"schema_version"`
	RunID         string                     `json:"run_id"`
	UpdatedAt     string                     `json:"updated_at"`
	Artifacts     map[string]CheckpointEntry `json:"artifacts"`
}

type CheckpointEntry struct {
	ArtifactID string `json:"artifact_id"`
	RunID      string `json:"run_id"`
	SourceURI  string `json:"source_uri"`
	Digest     string `json:"digest"`
	SizeBytes  int64  `json:"size_bytes"`
	DurableRef string `json:"durable_ref,omitempty"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	Error      string `json:"error,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

func Run(ctx context.Context, store *expstore.Store, opts Options) (Result, error) {
	if store == nil {
		return Result{}, fmt.Errorf("expstore is required")
	}
	opts.RunID = strings.TrimSpace(opts.RunID)
	if opts.RunID == "" {
		return Result{}, fmt.Errorf("--run is required")
	}
	if opts.ObjectStore == nil {
		return Result{}, fmt.Errorf("object store is required")
	}
	if opts.MaxSizeBytes < 0 {
		return Result{}, fmt.Errorf("--max-size-bytes must be non-negative")
	}
	checkpointFile, err := checkpointPath(opts)
	if err != nil {
		return Result{}, err
	}
	checkpoint, err := readCheckpoint(checkpointFile, opts.RunID)
	if err != nil {
		return Result{}, err
	}
	run, err := store.Run(ctx, opts.RunID)
	if err != nil {
		return Result{}, err
	}
	artifacts, err := store.ArtifactsForRun(ctx, opts.RunID)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		SchemaVersion: CheckpointSchemaVersion,
		RunID:         opts.RunID,
		Checkpoint:    checkpointFile,
	}
	verifiedByDigest := verifiedRefsByDigest(checkpoint)
	var indexUpdates []expstore.ArtifactRecord
	for _, artifact := range artifacts {
		artifactResult, updated, err := processArtifact(ctx, store, opts, run, artifact, checkpoint, verifiedByDigest)
		if err != nil && artifactResult.Status == "" {
			if writeErr := writeCheckpoint(checkpointFile, checkpoint); writeErr != nil {
				return result, fmt.Errorf("%w; write artifact checkpoint: %v", err, writeErr)
			}
			return result, err
		}
		result.Artifacts = append(result.Artifacts, artifactResult)
		switch artifactResult.Status {
		case "uploaded":
			result.Uploaded++
			result.Verified++
		case "deduped":
			result.Deduped++
			result.Verified++
		case "skipped":
			result.Skipped++
			result.Verified++
		case "failed":
			result.Failed++
		}
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
				if writeErr := writeCheckpoint(checkpointFile, checkpoint); writeErr != nil {
					return result, fmt.Errorf("%w; write artifact checkpoint: %v", err, writeErr)
				}
				return result, err
			}
			continue
		}
		if updated != nil {
			indexUpdates = append(indexUpdates, *updated)
		}
	}
	if err := writeCheckpoint(checkpointFile, checkpoint); err != nil {
		return result, err
	}
	if len(indexUpdates) > 0 {
		if err := store.UpdateArtifactDurableRefs(ctx, indexUpdates); err != nil {
			return result, err
		}
		result.Indexed = len(indexUpdates)
	}
	if result.Failed > 0 {
		return result, PartialFailureError{Failed: result.Failed}
	}
	return result, nil
}

func processArtifact(ctx context.Context, store *expstore.Store, opts Options, run expstore.RunRecord, artifact expstore.ArtifactRecord, checkpoint Checkpoint, verifiedByDigest map[string]blobstore.DurableRef) (ArtifactResult, *expstore.ArtifactRecord, error) {
	if err := ctx.Err(); err != nil {
		return ArtifactResult{}, nil, err
	}
	sourcePath, err := resolveArtifactPath(store.Root, artifact.URI)
	if err != nil {
		updateCheckpoint(checkpoint, artifact, "", 0, blobstore.DurableRef{}, "failed", err.Error())
		return failedArtifactResult(artifact, err), nil, err
	}
	digest, size, err := fileutil.FileSHA256(sourcePath)
	if err != nil {
		updateCheckpoint(checkpoint, artifact, "", 0, blobstore.DurableRef{}, "failed", err.Error())
		return failedArtifactResult(artifact, err), nil, err
	}
	if opts.MaxSizeBytes > 0 && size > opts.MaxSizeBytes {
		err := fmt.Errorf("artifact %q size %d exceeds --max-size-bytes %d", artifact.ArtifactID, size, opts.MaxSizeBytes)
		updateCheckpoint(checkpoint, artifact, digest, size, blobstore.DurableRef{}, "failed", err.Error())
		return failedArtifactResult(artifact, err), nil, err
	}
	if err := validateArtifactIdentity(artifact, digest, size); err != nil {
		updateCheckpoint(checkpoint, artifact, digest, size, blobstore.DurableRef{}, "failed", err.Error())
		return failedArtifactResult(artifact, err), nil, err
	}
	result := ArtifactResult{
		ArtifactID: artifact.ArtifactID,
		Name:       artifact.Name,
		Type:       artifact.Type,
		SourceURI:  artifact.URI,
		Digest:     digest,
		SizeBytes:  size,
	}
	if existingRef, ok := verifiedArtifactCheckpoint(checkpoint, artifact, digest, size); ok {
		if err := opts.ObjectStore.Verify(ctx, existingRef); err != nil {
			err = fmt.Errorf("verify checkpointed artifact %q: %w", artifact.ArtifactID, err)
			delete(verifiedByDigest, digest)
			updateCheckpoint(checkpoint, artifact, digest, size, existingRef, "failed", err.Error())
			return failedArtifactResult(artifact, err), nil, err
		}
		result.Status = "skipped"
		result.ObjectURI = existingRef.URI
		updated, err := updatedArtifactRecord(artifact, existingRef, digest, size, sourcePath)
		if err != nil {
			return failedArtifactResult(artifact, err), nil, err
		}
		verifiedByDigest[digest] = existingRef
		return result, updated, nil
	}
	if existingRef, ok := verifiedByDigest[digest]; ok {
		if err := opts.ObjectStore.Verify(ctx, existingRef); err != nil {
			err = fmt.Errorf("verify deduped artifact %q: %w", artifact.ArtifactID, err)
			delete(verifiedByDigest, digest)
			updateCheckpoint(checkpoint, artifact, digest, size, existingRef, "failed", err.Error())
			return failedArtifactResult(artifact, err), nil, err
		}
		updateCheckpoint(checkpoint, artifact, digest, size, existingRef, "verified", "")
		result.Status = "deduped"
		result.ObjectURI = existingRef.URI
		result.DedupedFrom = existingRef.BlobPath
		updated, err := updatedArtifactRecord(artifact, existingRef, digest, size, sourcePath)
		if err != nil {
			return failedArtifactResult(artifact, err), nil, err
		}
		return result, updated, nil
	}
	ref := blobstore.NewDurableRef(blobstore.Partition{
		Account:      opts.Account,
		Container:    opts.Container,
		BaseURI:      opts.ObjectBaseURI,
		Project:      run.Project,
		ExperimentID: run.ExperimentID,
		RunGroupID:   run.RunGroupID,
		RunID:        run.RunID,
		ArtifactType: artifact.Type,
		ArtifactName: artifact.Name,
		Digest:       digest,
		Rank:         artifact.Rank,
	}, size, contentTypeForArtifact(artifact, sourcePath), time.Now())
	updateCheckpoint(checkpoint, artifact, digest, size, ref, "uploading", "")
	uploaded, err := opts.ObjectStore.UploadFile(ctx, ref, sourcePath)
	if err != nil {
		updateCheckpoint(checkpoint, artifact, digest, size, ref, "failed", err.Error())
		return failedArtifactResult(artifact, err), nil, err
	}
	updateCheckpoint(checkpoint, artifact, digest, size, uploaded, "uploaded", "")
	if err := opts.ObjectStore.Verify(ctx, uploaded); err != nil {
		updateCheckpoint(checkpoint, artifact, digest, size, uploaded, "failed", err.Error())
		return failedArtifactResult(artifact, err), nil, err
	}
	updateCheckpoint(checkpoint, artifact, digest, size, uploaded, "verified", "")
	verifiedByDigest[digest] = uploaded
	result.Status = "uploaded"
	result.ObjectURI = uploaded.URI
	updated, err := updatedArtifactRecord(artifact, uploaded, digest, size, sourcePath)
	if err != nil {
		return failedArtifactResult(artifact, err), nil, err
	}
	return result, updated, nil
}

func checkpointPath(opts Options) (string, error) {
	if strings.TrimSpace(opts.Checkpoint) != "" {
		return filepath.Abs(opts.Checkpoint)
	}
	if strings.TrimSpace(opts.Out) == "" {
		return "", fmt.Errorf("--out is required")
	}
	return filepath.Abs(filepath.Join(opts.Out, "artifact_upload_checkpoint.json"))
}

func readCheckpoint(path, runID string) (Checkpoint, error) {
	checkpoint := Checkpoint{
		SchemaVersion: CheckpointSchemaVersion,
		RunID:         runID,
		Artifacts:     map[string]CheckpointEntry{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return checkpoint, nil
		}
		return Checkpoint{}, err
	}
	if err := json.Unmarshal(raw, &checkpoint); err != nil {
		return Checkpoint{}, fmt.Errorf("read artifact checkpoint %s: %w", path, err)
	}
	if checkpoint.SchemaVersion != CheckpointSchemaVersion {
		return Checkpoint{}, fmt.Errorf("read artifact checkpoint %s: unsupported schema_version %q", path, checkpoint.SchemaVersion)
	}
	if checkpoint.RunID != runID {
		return Checkpoint{}, fmt.Errorf("read artifact checkpoint %s: run_id %q does not match %q", path, checkpoint.RunID, runID)
	}
	if checkpoint.Artifacts == nil {
		checkpoint.Artifacts = map[string]CheckpointEntry{}
	}
	return checkpoint, nil
}

func writeCheckpoint(path string, checkpoint Checkpoint) error {
	checkpoint.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	return fileutil.WriteJSONFileAtomic(path, checkpoint)
}

func verifiedRefsByDigest(checkpoint Checkpoint) map[string]blobstore.DurableRef {
	refs := map[string]blobstore.DurableRef{}
	for _, entry := range checkpoint.Artifacts {
		if entry.Status != "verified" || entry.DurableRef == "" || entry.Digest == "" {
			continue
		}
		ref, err := blobstore.ParseDurableRef(entry.DurableRef)
		if err != nil {
			continue
		}
		refs[entry.Digest] = ref
	}
	return refs
}

func verifiedArtifactCheckpoint(checkpoint Checkpoint, artifact expstore.ArtifactRecord, digest string, size int64) (blobstore.DurableRef, bool) {
	entry, ok := checkpoint.Artifacts[artifact.ArtifactID]
	if !ok || entry.Status != "verified" || entry.Digest != digest || entry.SizeBytes != size || entry.DurableRef == "" {
		return blobstore.DurableRef{}, false
	}
	ref, err := blobstore.ParseDurableRef(entry.DurableRef)
	if err != nil {
		return blobstore.DurableRef{}, false
	}
	return ref, true
}

func updateCheckpoint(checkpoint Checkpoint, artifact expstore.ArtifactRecord, digest string, size int64, ref blobstore.DurableRef, status, message string) {
	entry := checkpoint.Artifacts[artifact.ArtifactID]
	if status == "uploading" {
		entry.Attempts++
	}
	refString, _ := ref.String()
	checkpoint.Artifacts[artifact.ArtifactID] = CheckpointEntry{
		ArtifactID: artifact.ArtifactID,
		RunID:      artifact.RunID,
		SourceURI:  artifact.URI,
		Digest:     digest,
		SizeBytes:  size,
		DurableRef: refString,
		Status:     status,
		Attempts:   entry.Attempts,
		Error:      message,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339),
	}
}

func updatedArtifactRecord(artifact expstore.ArtifactRecord, ref blobstore.DurableRef, digest string, size int64, sourcePath string) (*expstore.ArtifactRecord, error) {
	refString, err := ref.String()
	if err != nil {
		return nil, err
	}
	contentType := contentTypeForArtifact(artifact, sourcePath)
	if artifact.DurableRef == refString && artifact.ContentType == contentType && artifact.Digest == digest && artifact.SizeBytes != nil && *artifact.SizeBytes == size {
		return nil, nil
	}
	updated := artifact
	updated.DurableRef = refString
	updated.ContentType = contentType
	updated.Digest = digest
	updated.SizeBytes = &size
	return &updated, nil
}

func validateArtifactIdentity(artifact expstore.ArtifactRecord, digest string, size int64) error {
	if artifact.Digest != "" && blobstore.DigestWithAlgorithm(artifact.Digest) != blobstore.DigestWithAlgorithm(digest) {
		return fmt.Errorf("artifact %q digest %s does not match local file digest %s", artifact.ArtifactID, artifact.Digest, digest)
	}
	if artifact.SizeBytes != nil && *artifact.SizeBytes != size {
		return fmt.Errorf("artifact %q size %d does not match local file size %d", artifact.ArtifactID, *artifact.SizeBytes, size)
	}
	return nil
}

func resolveArtifactPath(storeRoot, uri string) (string, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return "", fmt.Errorf("artifact uri is empty")
	}
	if parsed, err := url.Parse(uri); err == nil && parsed.Scheme != "" {
		if parsed.Scheme != "file" {
			return "", fmt.Errorf("artifact uri %q is not a local file", uri)
		}
		return filepath.FromSlash(parsed.Path), nil
	}
	path := filepath.FromSlash(uri)
	if filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Join(storeRoot, path), nil
}

func contentTypeForArtifact(artifact expstore.ArtifactRecord, sourcePath string) string {
	if strings.TrimSpace(artifact.ContentType) != "" {
		return artifact.ContentType
	}
	return blobstore.ContentTypeForFile(sourcePath)
}

func failedArtifactResult(artifact expstore.ArtifactRecord, err error) ArtifactResult {
	return ArtifactResult{
		ArtifactID: artifact.ArtifactID,
		Name:       artifact.Name,
		Type:       artifact.Type,
		SourceURI:  artifact.URI,
		Status:     "failed",
		Error:      err.Error(),
	}
}
