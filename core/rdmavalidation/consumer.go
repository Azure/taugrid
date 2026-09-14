// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"fmt"
	"strings"

	"github.com/Azure/taugrid/core/exptelemetry"
)

func NewArtifactLink(uri string, info ArtifactInfo) ArtifactLink {
	return ArtifactLink{
		URI:         uri,
		SHA256:      info.SHA256,
		SizeBytes:   info.SizeBytes,
		FinalizedAt: info.WrittenAt,
	}
}

func ValidateArtifactLink(link ArtifactLink) error {
	if strings.TrimSpace(link.URI) == "" || len(link.URI) > 2048 {
		return fmt.Errorf("artifact URI is required and must not exceed 2048 characters")
	}
	if containsSecretMaterial(link.URI) || strings.ContainsAny(link.URI, "?#") {
		return fmt.Errorf("artifact URI is unsafe")
	}
	if !sha256DigestRE.MatchString(link.SHA256) {
		return fmt.Errorf("artifact SHA-256 must be sha256 followed by 64 lowercase hexadecimal characters")
	}
	if link.SizeBytes <= 0 {
		return fmt.Errorf("artifact size_bytes must be positive")
	}
	if link.FinalizedAt.IsZero() || !isUTC(link.FinalizedAt) {
		return fmt.Errorf("artifact finalized_at must be a nonzero UTC timestamp")
	}
	return nil
}

// Lifecycle maps an RDMA validation onto the existing experiment run states.
// A terminal state is impossible until the immutable artifact is finalized
// after cleanup; an unknown terminal result is deliberately failed, not a new
// lifecycle state.
func (result Result) Lifecycle(link *ArtifactLink) (RunLifecycle, error) {
	if result.Schema != SchemaVersion {
		return RunLifecycle{}, fmt.Errorf("schema = %q, want %q", result.Schema, SchemaVersion)
	}
	if result.Kind != Kind {
		return RunLifecycle{}, fmt.Errorf("kind = %q, want %q", result.Kind, Kind)
	}
	if err := exptelemetry.ValidateID("validation_id", result.ValidationID); err != nil {
		return RunLifecycle{}, err
	}
	if err := exptelemetry.ValidateID("run_id", result.RunID); err != nil {
		return RunLifecycle{}, err
	}
	for kind, value := range map[string]string{
		"workspace_id": result.WorkspaceID,
		"cluster":      result.Cluster,
		"namespace":    result.Namespace,
	} {
		if err := exptelemetry.ValidateID(kind, value); err != nil {
			return RunLifecycle{}, err
		}
	}
	if result.Attempt < 1 {
		return RunLifecycle{}, fmt.Errorf("attempt must be positive")
	}
	if result.CreatedAt.IsZero() || !isUTC(result.CreatedAt) {
		return RunLifecycle{}, fmt.Errorf("created_at must be a nonzero UTC timestamp")
	}
	if !result.StartedAt.IsZero() {
		if !isUTC(result.StartedAt) || result.StartedAt.Before(result.CreatedAt) {
			return RunLifecycle{}, fmt.Errorf("started_at must be UTC and not precede created_at")
		}
	}
	lifecycle := RunLifecycle{
		State:     RunStatePending,
		CreatedAt: result.CreatedAt,
		StartedAt: result.StartedAt,
	}
	if !result.StartedAt.IsZero() {
		lifecycle.State = RunStateRunning
	}
	if link == nil {
		return lifecycle, nil
	}
	if err := Validate(result); err != nil {
		return RunLifecycle{}, fmt.Errorf("validate finalized result: %w", err)
	}
	if err := ValidateArtifactLink(*link); err != nil {
		return RunLifecycle{}, err
	}
	raw, err := MarshalCanonical(result)
	if err != nil {
		return RunLifecycle{}, err
	}
	if link.SHA256 != digestBytes(raw) || link.SizeBytes != int64(len(raw)) {
		return RunLifecycle{}, fmt.Errorf("artifact link does not match the canonical result bytes")
	}
	if result.Cleanup.StartedAt.IsZero() || result.Cleanup.CompletedAt.IsZero() {
		return RunLifecycle{}, fmt.Errorf("artifact cannot finalize before cleanup completes")
	}
	if link.FinalizedAt.Before(result.Cleanup.CompletedAt) {
		return RunLifecycle{}, fmt.Errorf("artifact finalized_at precedes cleanup completion")
	}
	lifecycle.CompletedAt = link.FinalizedAt
	if result.Status == StatusPass {
		lifecycle.State = RunStateSucceeded
	} else {
		lifecycle.State = RunStateFailed
	}
	return lifecycle, nil
}
