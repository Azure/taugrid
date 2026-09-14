// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

func MarshalCanonical(result Result) ([]byte, error) {
	normalized := normalizeResult(result)
	if err := Validate(normalized); err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(normalized, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	if containsSecretMaterial(string(raw)) {
		return nil, fmt.Errorf("result contains forbidden credential material")
	}
	return raw, nil
}

func Hash(result Result) (string, error) {
	raw, err := MarshalCanonical(result)
	if err != nil {
		return "", err
	}
	return digestBytes(raw), nil
}

func DefaultArtifactPath(baseDirectory, validationID string) string {
	return filepath.Join(baseDirectory, "rdma-validation", validationID+".json")
}

func WriteArtifact(path string, result Result) (ArtifactInfo, error) {
	if result.Cleanup.StartedAt.IsZero() || result.Cleanup.CompletedAt.IsZero() {
		return ArtifactInfo{}, fmt.Errorf("artifact cannot be finalized before cleanup completes")
	}
	if err := Finalize(&result); err != nil {
		return ArtifactInfo{}, err
	}
	raw, err := MarshalCanonical(result)
	if err != nil {
		return ArtifactInfo{}, err
	}
	if err := writeImmutableAtomic(path, raw, 0o644); err != nil {
		return ArtifactInfo{}, err
	}
	return ArtifactInfo{
		Path: path, SHA256: digestBytes(raw), SizeBytes: int64(len(raw)), WrittenAt: time.Now().UTC(),
	}, nil
}

func NewEvidenceRef(name, uri string, raw []byte, capturedAt time.Time) (EvidenceRef, error) {
	if containsSecretMaterial(string(raw)) {
		return EvidenceRef{}, fmt.Errorf("evidence %q contains forbidden credential material", name)
	}
	digest := digestBytes(raw)
	if uri == "" {
		uri = "urn:" + digest
	}
	ref := EvidenceRef{
		Name: name, URI: uri, SHA256: digest, SizeBytes: int64(len(raw)), CapturedAt: capturedAt.UTC(),
	}
	probe := Result{
		Schema: SchemaVersion, Kind: Kind, ValidationID: "evidence-validation", Attempt: 1,
		CreatedAt: capturedAt.UTC(), ObservedAt: capturedAt.UTC(),
		StaleAfterSeconds: DefaultStaleAfterSeconds,
		ValidUntil:        capturedAt.UTC().Add(time.Duration(DefaultStaleAfterSeconds) * time.Second),
		Cleanup:           Cleanup{State: CleanupUnknown},
		Evidence:          []EvidenceRef{ref},
	}
	evaluation := Evaluate(probe)
	probe.Status, probe.Reason, probe.Errors = evaluation.Status, evaluation.Reason, evaluation.Errors
	if err := Validate(probe); err != nil {
		return EvidenceRef{}, fmt.Errorf("validate evidence reference: %w", err)
	}
	return ref, nil
}

func writeImmutableAtomic(path string, raw []byte, perm os.FileMode) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("artifact path is required")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(directory, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil && !chmodUnsupported(err) {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("artifact %s already exists; refusing to replace immutable result: %w", path, err)
		}
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !chmodUnsupported(err) {
		return err
	}
	return nil
}

func normalizeResult(result Result) Result {
	result.Errors = normalizeErrors(result.Errors)
	result.Actual.Nodes = append([]NodeResult(nil), result.Actual.Nodes...)
	slices.SortFunc(result.Actual.Nodes, func(left, right NodeResult) int {
		return strings.Compare(left.Name, right.Name)
	})
	result.Pods = append([]PodResult(nil), result.Pods...)
	slices.SortFunc(result.Pods, func(left, right PodResult) int {
		return left.Rank - right.Rank
	})
	result.Ranks = append([]RankResult(nil), result.Ranks...)
	slices.SortFunc(result.Ranks, func(left, right RankResult) int {
		return left.Rank - right.Rank
	})
	result.RankExits = append([]RankExit(nil), result.RankExits...)
	slices.SortFunc(result.RankExits, func(left, right RankExit) int {
		return left.Rank - right.Rank
	})
	result.Measurements.Samples = append([]BandwidthMeasurement(nil), result.Measurements.Samples...)
	slices.SortFunc(result.Measurements.Samples, func(left, right BandwidthMeasurement) int {
		return left.Rank - right.Rank
	})
	result.Transport.Interfaces = append([]string(nil), result.Transport.Interfaces...)
	slices.Sort(result.Transport.Interfaces)
	result.Transport.Evidence = append([]string(nil), result.Transport.Evidence...)
	slices.Sort(result.Transport.Evidence)
	result.Cleanup.RemainingResources = append([]ResourceRef(nil), result.Cleanup.RemainingResources...)
	result.Cleanup.OwnedResources = append([]ResourceRef(nil), result.Cleanup.OwnedResources...)
	sortResources := func(resources []ResourceRef) {
		slices.SortFunc(resources, func(left, right ResourceRef) int {
			if value := strings.Compare(left.APIVersion, right.APIVersion); value != 0 {
				return value
			}
			if value := strings.Compare(left.Kind, right.Kind); value != 0 {
				return value
			}
			if value := strings.Compare(left.Namespace, right.Namespace); value != 0 {
				return value
			}
			return strings.Compare(left.Name, right.Name)
		})
	}
	sortResources(result.Cleanup.OwnedResources)
	sortResources(result.Cleanup.RemainingResources)
	result.Evidence = append([]EvidenceRef(nil), result.Evidence...)
	slices.SortFunc(result.Evidence, func(left, right EvidenceRef) int {
		return strings.Compare(left.Name, right.Name)
	})
	return result
}

func digestBytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func chmodUnsupported(err error) bool {
	return errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
