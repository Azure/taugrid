// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/parquet-go/parquet-go"
)

func TestProjectRDMAValidationLifecycleAndArtifactLinkage(t *testing.T) {
	result := readRDMAValidationGolden(t, "pass.golden.json")

	pending := result
	pending.StartedAt = time.Time{}
	projection, err := ProjectRDMAValidation(pending, nil)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Run.State != rdmavalidation.RunStatePending ||
		projection.Artifact != nil || len(projection.Metrics) != 0 {
		t.Fatalf("pending projection = %+v", projection)
	}
	if classification := ClassifyRun(projection.Run, nil, nil, SuccessOptions{}); classification.LifecycleState != "pending" {
		t.Fatalf("pending classification = %+v", classification)
	}
	if len(projection.Tags) != 1 ||
		projection.Tags[0].Key != rdmavalidation.RunKindTag ||
		projection.Tags[0].Value != rdmavalidation.Kind {
		t.Fatalf("pending validation identity tags = %+v", projection.Tags)
	}

	projection, err = ProjectRDMAValidation(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Run.State != rdmavalidation.RunStateRunning ||
		projection.Run.ResultURI != "" || projection.Run.CompletedAt != "" {
		t.Fatalf("running projection = %+v", projection.Run)
	}
	if classification := ClassifyRun(projection.Run, nil, nil, SuccessOptions{}); classification.LifecycleState != "running" {
		t.Fatalf("running classification = %+v", classification)
	}

	raw, err := rdmavalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	link := rdmavalidation.ArtifactLink{
		URI:         "file:///immutable/rdma-validation/" + result.ValidationID + ".json",
		SHA256:      sha256Bytes(raw),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
	}
	first, err := ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("RDMA validation projection is not deterministic")
	}
	if first.Run.State != rdmavalidation.RunStateSucceeded ||
		first.Run.ResultURI != link.URI ||
		first.Run.CompletedAt != link.FinalizedAt.Format(time.RFC3339Nano) {
		t.Fatalf("terminal run projection = %+v", first.Run)
	}
	if classification := ClassifyRun(first.Run, nil, nil, SuccessOptions{}); classification.LifecycleState != "succeeded" {
		t.Fatalf("succeeded classification = %+v", classification)
	}
	if first.Artifact == nil ||
		first.Artifact.Type != rdmavalidation.ArtifactType ||
		first.Artifact.ContentType != rdmavalidation.ArtifactContentType ||
		first.Artifact.URI != link.URI ||
		first.Artifact.Digest != link.SHA256 {
		t.Fatalf("artifact projection = %+v", first.Artifact)
	}
	if len(first.Metrics) != 8 {
		t.Fatalf("metric rows = %d, want 8", len(first.Metrics))
	}
	for _, row := range first.Metrics {
		var tags map[string]string
		if err := json.Unmarshal([]byte(row.Tags), &tags); err != nil {
			t.Fatalf("metric %s tags: %v", row.MetricName, err)
		}
		if tags[rdmavalidation.MetricArtifactURITag] != link.URI ||
			tags[rdmavalidation.MetricArtifactSHA256Tag] != link.SHA256 {
			t.Fatalf("metric %s artifact linkage = %+v", row.MetricName, tags)
		}
		if row.Source != rdmavalidation.Kind || row.RunID != result.RunID {
			t.Fatalf("metric row identity = %+v", row)
		}
	}
}

func TestProjectRDMAValidationUnknownArtifactIsTerminalFailure(t *testing.T) {
	result := readRDMAValidationGolden(t, "missing-unknown.golden.json")
	raw, err := rdmavalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	link := rdmavalidation.ArtifactLink{
		URI:         "file:///immutable/rdma-validation/" + result.ValidationID + ".json",
		SHA256:      sha256Bytes(raw),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
	}
	projection, err := ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Run.State != rdmavalidation.RunStateFailed {
		t.Fatalf("unknown terminal run state = %q, want failed", projection.Run.State)
	}
	if classification := ClassifyRun(projection.Run, nil, nil, SuccessOptions{}); classification.LifecycleState != "failed" {
		t.Fatalf("unknown terminal classification = %+v", classification)
	}
	if len(projection.Metrics) == 0 || projection.Metrics[0].Value != 0 {
		t.Fatalf("unknown status metrics = %+v", projection.Metrics)
	}
}

func TestRDMAValidationInProgressRunLifecyclePersistsWithoutArtifact(t *testing.T) {
	result := readRDMAValidationGolden(t, "pass.golden.json")
	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name: result.ExperimentID, Project: result.ProjectID, Group: result.RunGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	pending := result
	pending.StartedAt = time.Time{}
	pendingProjection, err := ProjectRDMAValidation(pending, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnrichRunData(ctx, EnrichRunDataOptions{
		Run: pendingProjection.Run, Tags: pendingProjection.Tags, Command: "rdma validation pending",
	}); err != nil {
		t.Fatal(err)
	}
	runningProjection, err := ProjectRDMAValidation(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnrichRunData(ctx, EnrichRunDataOptions{
		Run: runningProjection.Run, Tags: runningProjection.Tags, Command: "rdma validation running",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := store.Run(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != rdmavalidation.RunStateRunning || stored.CompletedAt != "" || stored.ResultURI != "" {
		t.Fatalf("stored in-progress run = %+v", stored)
	}
	artifacts, err := store.ArtifactsForRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 0 {
		t.Fatalf("in-progress run unexpectedly has artifacts: %+v", artifacts)
	}
}

func TestRDMAValidationProjectionPersistsExistingPortalRecords(t *testing.T) {
	result := readRDMAValidationGolden(t, "pass.golden.json")
	raw, err := rdmavalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	link := rdmavalidation.ArtifactLink{
		URI:         "file:///immutable/rdma-validation/" + result.ValidationID + ".json",
		SHA256:      sha256Bytes(raw),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
	}
	projection, err := ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	store, _, err := Init(ctx, filepath.Join(t.TempDir(), "store"), InitOptions{
		Name: result.ExperimentID, Project: result.ProjectID, Group: result.RunGroupID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	metricPath := filepath.Join("metrics", result.RunID, "rdma-validation.parquet")
	absoluteMetricPath := filepath.Join(store.Root, metricPath)
	if err := os.MkdirAll(filepath.Dir(absoluteMetricPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := parquet.WriteFile(absoluteMetricPath, projection.Metrics); err != nil {
		t.Fatal(err)
	}
	file := MetricFileRecord{
		FileID:        result.ValidationID + "-metrics",
		Path:          metricPath,
		Format:        "parquet",
		SchemaVersion: MetricSchemaVersion,
		Project:       result.ProjectID,
		RunGroupID:    result.RunGroupID,
		RunID:         result.RunID,
		RowCount:      int64(len(projection.Metrics)),
		CreatedAt:     link.FinalizedAt.Format(time.RFC3339Nano),
	}
	_, err = store.RecordRunData(ctx, RecordRunDataOptions{
		Run:             projection.Run,
		Tags:            projection.Tags,
		Artifacts:       []ArtifactRecord{*projection.Artifact},
		MetricFiles:     []MetricFileRecord{file},
		MetricSummaries: SummarizeMetricRows(file, projection.Metrics),
		IdempotencyKey:  result.ValidationID + "-portal-fixture",
		Command:         "rdma validation projection fixture",
		RequestHash:     link.SHA256,
	})
	if err != nil {
		t.Fatal(err)
	}
	storedRun, err := store.Run(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if storedRun.State != rdmavalidation.RunStateSucceeded || storedRun.ResultURI != link.URI {
		t.Fatalf("stored run = %+v", storedRun)
	}
	artifacts, err := store.ArtifactsForRun(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Type != rdmavalidation.ArtifactType ||
		artifacts[0].Digest != link.SHA256 {
		t.Fatalf("stored artifacts = %+v", artifacts)
	}
	rows, err := parquet.ReadFile[MetricRow](absoluteMetricPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		var tags map[string]string
		if err := json.Unmarshal([]byte(row.Tags), &tags); err != nil {
			t.Fatal(err)
		}
		if tags[rdmavalidation.MetricArtifactURITag] != link.URI ||
			tags[rdmavalidation.MetricArtifactSHA256Tag] != link.SHA256 {
			t.Fatalf("persisted metric %s lost artifact linkage: %+v", row.MetricName, tags)
		}
	}
}

func readRDMAValidationGolden(t *testing.T, name string) rdmavalidation.Result {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "core", "rdmavalidation", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	var result rdmavalidation.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if err := rdmavalidation.Validate(result); err != nil {
		t.Fatal(err)
	}
	return result
}

func sha256Bytes(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}
