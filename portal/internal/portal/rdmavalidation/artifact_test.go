// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	corevalidation "github.com/Azure/taugrid/core/rdmavalidation"
)

func TestDecodeArtifactMapsCanonicalPass(t *testing.T) {
	raw := readGolden(t, "pass.golden.json")
	now := time.Date(2026, time.September, 14, 20, 5, 0, 0, time.UTC)
	detail, err := DecodeArtifact(raw, metadataFor(t, raw), now)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != StatePassed || detail.Freshness != FreshnessFresh ||
		detail.HistoricalStatus == nil || *detail.HistoricalStatus != "pass" {
		t.Fatalf("state = %q/%q/%v", detail.State, detail.Freshness, detail.HistoricalStatus)
	}
	if detail.ValidationID != "nccl-rdma-0123456789abcdef0123456789abcdef" ||
		detail.WorkspaceID != "taugrid-rdma" || detail.Cluster != "h200-validation" {
		t.Fatalf("identity = %+v", detail.Validation)
	}
	if detail.Parameters == nil || len(detail.Parameters.MessageSizesBytes) != 1 ||
		detail.Parameters.MessageSizesBytes[0] != 67_108_864 {
		t.Fatalf("derived message size = %+v", detail.Parameters)
	}
	if detail.Requested == nil || detail.Requested.NodeCount == nil || *detail.Requested.NodeCount != 2 ||
		detail.Actual == nil || len(detail.Actual.Nodes) != 2 ||
		len(detail.Actual.Nodes[0].GPUUUIDs) != 1 || detail.Actual.Nodes[0].GPUUUIDs[0] != "GPU-aaaaaaaa" {
		t.Fatalf("topology = requested %+v actual %+v", detail.Requested, detail.Actual)
	}
	if detail.Transport == nil || detail.Transport.NCCLNet != "IB" ||
		detail.Transport.SocketFallbackDetected == nil || *detail.Transport.SocketFallbackDetected ||
		len(detail.Transport.Evidence) != 2 {
		t.Fatalf("transport = %+v", detail.Transport)
	}
	if len(detail.Measurements) != 2 || detail.Measurements[0].Rank == nil ||
		*detail.Measurements[0].Rank != 0 || detail.Measurements[0].ElapsedSeconds == nil {
		t.Fatalf("measurements = %+v", detail.Measurements)
	}
	if detail.Source == nil || detail.Source.SBOMManifestDigest == "" ||
		detail.Source.SignatureLayerDigest == "" {
		t.Fatalf("source = %+v", detail.Source)
	}
	if detail.Cleanup == nil || detail.Cleanup.Status != "complete" ||
		len(detail.Cleanup.OwnedResources) != 6 || len(detail.Evidence) != 5 {
		t.Fatalf("cleanup/evidence = %+v/%+v", detail.Cleanup, detail.Evidence)
	}
	if detail.ArtifactVerification == nil || detail.ArtifactVerification.State != "verified" ||
		detail.ArtifactVerification.VerifiedAt != now.Format(time.RFC3339Nano) {
		t.Fatalf("verification = %+v", detail.ArtifactVerification)
	}
}

func TestDecodeArtifactPreservesFailUnknownAndStale(t *testing.T) {
	tests := []struct {
		name       string
		golden     string
		now        time.Time
		state      string
		historical string
		reason     string
	}{
		{
			name: "socket fallback", golden: "socket-fail.golden.json",
			now:   time.Date(2026, time.September, 14, 20, 5, 0, 0, time.UTC),
			state: StateFailed, historical: "fail", reason: "socket_fallback_observed",
		},
		{
			name: "missing evidence", golden: "missing-unknown.golden.json",
			now:   time.Date(2026, time.September, 14, 20, 5, 0, 0, time.UTC),
			state: StateUnknown, historical: "unknown", reason: "evidence_integrity_missing",
		},
		{
			name: "stale pass", golden: "pass.golden.json",
			now:   time.Date(2026, time.September, 15, 20, 0, 15, 0, time.UTC),
			state: StateStale, historical: "pass", reason: "validation_passed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw := readGolden(t, test.golden)
			detail, err := DecodeArtifact(raw, metadataFor(t, raw), test.now)
			if err != nil {
				t.Fatal(err)
			}
			if detail.State != test.state || detail.HistoricalStatus == nil ||
				*detail.HistoricalStatus != test.historical || detail.ReasonCode != test.reason {
				t.Fatalf("result = %+v", detail.Validation)
			}
		})
	}
}

func TestDecodeArtifactRejectsMalformedUnsupportedAndUnverified(t *testing.T) {
	pass := readGolden(t, "pass.golden.json")
	tests := []struct {
		name     string
		raw      []byte
		metadata ArtifactMetadata
		target   error
	}{
		{name: "malformed", raw: []byte(`{"schema":`), metadata: metadataForRaw([]byte(`{"schema":`)), target: ErrMalformedArtifact},
		{name: "hash mismatch", raw: pass, metadata: ArtifactMetadata{
			SHA256: "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		}, target: ErrArtifactIntegrity},
		{name: "size mismatch", raw: pass, metadata: ArtifactMetadata{
			SHA256: digest(pass), SizeBytes: int64(len(pass) + 1),
		}, target: ErrArtifactIntegrity},
		{name: "content type", raw: pass, metadata: ArtifactMetadata{
			SHA256: digest(pass), SizeBytes: int64(len(pass)), ContentType: "application/json",
		}, target: ErrArtifactIntegrity},
	}
	var decoded map[string]any
	if err := json.Unmarshal(pass, &decoded); err != nil {
		t.Fatal(err)
	}
	decoded["schema"] = "rdma-validation.v2"
	unsupported, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	tests = append(tests, struct {
		name     string
		raw      []byte
		metadata ArtifactMetadata
		target   error
	}{
		name: "unsupported schema", raw: unsupported,
		metadata: metadataForRaw(unsupported), target: ErrUnsupportedSchema,
	})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := DecodeArtifact(test.raw, test.metadata, time.Now().UTC()); !errors.Is(err, test.target) {
				t.Fatalf("DecodeArtifact() error = %v, want %v", err, test.target)
			}
		})
	}
}

func TestDecodeArtifactRejectsScopeMismatch(t *testing.T) {
	raw := readGolden(t, "pass.golden.json")
	for name, mutate := range map[string]func(*ArtifactMetadata){
		"validation": func(metadata *ArtifactMetadata) { metadata.ValidationID = "other-validation" },
		"run":        func(metadata *ArtifactMetadata) { metadata.RunID = "other-run" },
		"workspace":  func(metadata *ArtifactMetadata) { metadata.WorkspaceID = "other-workspace" },
	} {
		t.Run(name, func(t *testing.T) {
			metadata := metadataFor(t, raw)
			mutate(&metadata)
			if _, err := DecodeArtifact(raw, metadata, time.Now().UTC()); !errors.Is(err, ErrScopeMismatch) {
				t.Fatalf("DecodeArtifact() error = %v, want scope mismatch", err)
			}
		})
	}
}

func TestDecodeArtifactDoesNotGuessUnknownMessageSize(t *testing.T) {
	result := readResult(t, "pass.golden.json")
	result.Requested.Parameters.DataType = "custom128"
	if err := corevalidation.Finalize(&result); err != nil {
		t.Fatal(err)
	}
	raw, err := corevalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := DecodeArtifact(raw, metadataForRaw(raw), result.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Parameters == nil || len(detail.Parameters.MessageSizesBytes) != 0 ||
		detail.Requested == nil || len(detail.Requested.MessageSizesBytes) != 0 {
		t.Fatalf("unknown data type produced message bytes: %+v/%+v", detail.Parameters, detail.Requested)
	}
}

func TestDecodeArtifactMapsCleanupFailure(t *testing.T) {
	result := readResult(t, "pass.golden.json")
	result.Cleanup.State = corevalidation.CleanupIncomplete
	result.Cleanup.RemainingResources = []corevalidation.ResourceRef{{
		APIVersion: "batch/v1", Kind: "Job", Namespace: "taugrid-rdma-diagnostic",
		Name: "nccl-rdma", UID: "job-uid",
	}}
	if err := corevalidation.Finalize(&result); err != nil {
		t.Fatal(err)
	}
	raw, err := corevalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := DecodeArtifact(raw, metadataForRaw(raw), result.ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != StateFailed || detail.ReasonCode != "cleanup_incomplete" ||
		detail.Cleanup == nil || len(detail.Cleanup.RemainingResources) != 1 {
		t.Fatalf("cleanup failure = %+v", detail)
	}
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "core", "rdmavalidation", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func readResult(t *testing.T, name string) corevalidation.Result {
	t.Helper()
	var result corevalidation.Result
	if err := json.Unmarshal(readGolden(t, name), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func metadataFor(t *testing.T, raw []byte) ArtifactMetadata {
	t.Helper()
	result := readResult(t, "pass.golden.json")
	return ArtifactMetadata{
		ValidationID: result.ValidationID, RunID: result.RunID, WorkspaceID: result.WorkspaceID,
		URI:         "az://results/rdma-validation/" + result.ValidationID + ".json",
		ContentType: corevalidation.ArtifactContentType, SHA256: digest(raw), SizeBytes: int64(len(raw)),
	}
}

func metadataForRaw(raw []byte) ArtifactMetadata {
	return ArtifactMetadata{
		ContentType: corevalidation.ArtifactContentType, SHA256: digest(raw), SizeBytes: int64(len(raw)),
	}
}
