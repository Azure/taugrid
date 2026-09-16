// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expstore

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
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
		projection.Artifact != nil || len(projection.Metrics) != 1 {
		t.Fatalf("pending projection = %+v", projection)
	}
	assertRDMAStatusRow(t, projection, pending, rdmavalidation.RunStatePending, 0, pending.CreatedAt)
	if projection.Step != 0 || projection.Phase != "pending" {
		t.Fatalf("pending identity = phase %q step %d", projection.Phase, projection.Step)
	}
	if classification := ClassifyRun(projection.Run, nil, nil, SuccessOptions{}); classification.LifecycleState != "pending" {
		t.Fatalf("pending classification = %+v", classification)
	}
	runTags := map[string]string{}
	for _, tag := range projection.Tags {
		runTags[tag.Key] = tag.Value
	}
	if len(runTags) != 6 ||
		runTags[rdmavalidation.RunKindTag] != rdmavalidation.Kind ||
		runTags[exptelemetry.TauWorkspaceTag] != result.WorkspaceID ||
		runTags[exptelemetry.TauClusterTag] != result.Cluster ||
		runTags[exptelemetry.TauNamespaceTag] != result.Namespace {
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
	if len(projection.Metrics) != 1 {
		t.Fatalf("running metrics = %d, want one status row", len(projection.Metrics))
	}
	assertRDMAStatusRow(t, projection, result, rdmavalidation.RunStateRunning, 0, result.StartedAt)
	if projection.Step != 1 || projection.Phase != "running" {
		t.Fatalf("running identity = phase %q step %d", projection.Phase, projection.Step)
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
	instanceID, err := rdmaValidationInstanceID(result)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("RDMA validation projection is not deterministic")
	}
	if first.IdempotencyKey != instanceID+"-terminal" ||
		first.MetricFileID != instanceID+"-terminal-metrics" ||
		first.RequestHash == "" || first.Step != 2 || first.Phase != "terminal" {
		t.Fatalf("terminal projection identity = %+v", first)
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
		first.Artifact.ArtifactID != instanceID+"-result" ||
		first.Artifact.Type != rdmavalidation.ArtifactType ||
		first.Artifact.ContentType != rdmavalidation.ArtifactContentType ||
		first.Artifact.URI != link.URI ||
		first.Artifact.Digest != link.SHA256 {
		t.Fatalf("artifact projection = %+v", first.Artifact)
	}
	if len(first.Metrics) != 9 {
		t.Fatalf("metric rows = %d, want one run status plus eight validation rows", len(first.Metrics))
	}
	assertRDMAStatusRow(t, first, result, rdmavalidation.RunStateSucceeded, 1, link.FinalizedAt)
	var validationStatus *MetricRow
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
		assertRDMAMetricIdentityTags(t, tags, result, rdmavalidation.RunStateSucceeded)
		wantWallTime := result.ObservedAt.UnixMicro()
		if row.MetricName == exptelemetry.RunStatusMetricName {
			wantWallTime = link.FinalizedAt.UnixMicro()
		}
		if row.Step == nil || *row.Step != first.Step ||
			row.WallTime == nil || *row.WallTime != wantWallTime {
			t.Fatalf("terminal metric timing = %+v", row)
		}
		if row.MetricName == rdmavalidation.MetricStatus {
			statusCopy := row
			validationStatus = &statusCopy
		}
	}
	if validationStatus == nil {
		t.Fatal("terminal projection has no RDMA validation status scalar")
	}
	freshUntil := time.UnixMicro(*validationStatus.WallTime).UTC().Add(24 * time.Hour)
	if !freshUntil.Equal(result.ValidUntil.Truncate(time.Microsecond)) {
		t.Fatalf(
			"status scalar freshness boundary = %s, want %s",
			freshUntil.Format(time.RFC3339Nano),
			result.ValidUntil.Truncate(time.Microsecond).Format(time.RFC3339Nano),
		)
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
	if len(projection.Metrics) != 9 || projection.Metrics[0].Value != -1 {
		t.Fatalf("unknown status metrics = %+v", projection.Metrics)
	}
	tags := metricTags(t, projection.Metrics[0])
	if tags[rdmavalidation.MetricValidationStatusTag] != string(rdmavalidation.StatusUnknown) ||
		tags[rdmavalidation.MetricValidationReasonTag] != string(rdmavalidation.ReasonEvidenceIntegrityMissing) {
		t.Fatalf("unknown validation status/reason tags = %+v", tags)
	}
	var validationStatus *MetricRow
	for index := range projection.Metrics {
		if projection.Metrics[index].MetricName == rdmavalidation.MetricStatus {
			validationStatus = &projection.Metrics[index]
			break
		}
	}
	if validationStatus == nil || validationStatus.Value != 0 {
		t.Fatalf("unknown validation scalar = %+v", validationStatus)
	}
}

func TestProjectRDMAValidationRejectsMissingOrInvalidIdentity(t *testing.T) {
	for name, mutate := range map[string]func(*rdmavalidation.Result){
		"workspace missing":  func(result *rdmavalidation.Result) { result.WorkspaceID = "" },
		"workspace invalid":  func(result *rdmavalidation.Result) { result.WorkspaceID = "Bad/Workspace" },
		"cluster missing":    func(result *rdmavalidation.Result) { result.Cluster = "" },
		"cluster invalid":    func(result *rdmavalidation.Result) { result.Cluster = "Bad/Cluster" },
		"namespace missing":  func(result *rdmavalidation.Result) { result.Namespace = "" },
		"namespace invalid":  func(result *rdmavalidation.Result) { result.Namespace = "Bad/Namespace" },
		"validation missing": func(result *rdmavalidation.Result) { result.ValidationID = "" },
		"validation invalid": func(result *rdmavalidation.Result) { result.ValidationID = "Bad/Validation" },
		"project invalid":    func(result *rdmavalidation.Result) { result.ProjectID = "Bad/Project" },
		"group invalid":      func(result *rdmavalidation.Result) { result.RunGroupID = "Bad/Group" },
	} {
		t.Run(name, func(t *testing.T) {
			result := readRDMAValidationGolden(t, "pass.golden.json")
			mutate(&result)
			if _, err := ProjectRDMAValidation(result, nil); err == nil {
				t.Fatal("ProjectRDMAValidation() succeeded, want identity error")
			}
		})
	}
}

func TestProjectRDMAValidationDefaultsOptionalProjectAndRunGroup(t *testing.T) {
	result := readRDMAValidationGolden(t, "pass.golden.json")
	result.ProjectID = ""
	result.RunGroupID = ""

	running, err := ProjectRDMAValidation(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if running.Run.Project != defaultRDMADimension ||
		running.Run.RunGroupID != defaultRDMADimension ||
		len(running.Metrics) != 1 ||
		running.Metrics[0].Project != defaultRDMADimension ||
		running.Metrics[0].RunGroupID != defaultRDMADimension {
		t.Fatalf("running defaults = run %+v metrics %+v", running.Run, running.Metrics)
	}

	link := artifactLinkForPortalResult(t, result)
	terminal, err := ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range terminal.Metrics {
		if row.Project != defaultRDMADimension || row.RunGroupID != defaultRDMADimension {
			t.Fatalf("terminal metric dimensions = %+v", row)
		}
	}
	if terminal.Run.Project != defaultRDMADimension ||
		terminal.Run.RunGroupID != defaultRDMADimension {
		t.Fatalf("terminal run defaults = %+v", terminal.Run)
	}
}

func TestRDMAMetricTagBoundsFailClosed(t *testing.T) {
	result := readRDMAValidationGolden(t, "pass.golden.json")
	tags := rdmaValidationMetricTags(result, rdmavalidation.RunStateRunning)
	tags["operation"] = strings.Repeat("x", maxRDMAMetricTagValue+1)
	if _, err := rdmaValidationMetricRow(
		result, exptelemetry.RunStatusMetricName, 0, nil, 1, result.StartedAt, tags,
	); err == nil {
		t.Fatal("rdmaValidationMetricRow() accepted an oversized tag value")
	}

	tags = rdmaValidationMetricTags(result, rdmavalidation.RunStateRunning)
	for index := len(tags); index <= maxRDMAMetricTags; index++ {
		tags["extra."+string(rune('a'+index))] = "bounded"
	}
	if _, err := rdmaValidationMetricRow(
		result, exptelemetry.RunStatusMetricName, 0, nil, 1, result.StartedAt, tags,
	); err == nil {
		t.Fatal("rdmaValidationMetricRow() accepted too many tags")
	}
}

func TestRDMAValidationProjectionRetryIdentityAndConflictHash(t *testing.T) {
	result := readRDMAValidationGolden(t, "pass.golden.json")
	first, err := ProjectRDMAValidation(result, nil)
	if err != nil {
		t.Fatal(err)
	}

	retry, err := ProjectRDMAValidation(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, retry) {
		t.Fatal("exact running projection retry is not deeply equal")
	}
	conflict := result
	conflict.StartedAt = conflict.StartedAt.Add(time.Second)
	conflicting, err := ProjectRDMAValidation(conflict, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.IdempotencyKey != conflicting.IdempotencyKey ||
		first.MetricFileID != conflicting.MetricFileID ||
		first.RequestHash == conflicting.RequestHash {
		t.Fatalf("same-phase conflict identity/hash = first %+v conflicting %+v", first, conflicting)
	}

	attemptTwo := result
	attemptTwo.Attempt = 2
	attemptProjection, err := ProjectRDMAValidation(attemptTwo, nil)
	if err != nil {
		t.Fatal(err)
	}
	if attemptProjection.Step != 4 ||
		attemptProjection.IdempotencyKey == first.IdempotencyKey ||
		attemptProjection.MetricFileID == first.MetricFileID {
		t.Fatalf("attempt-two identity = %+v", attemptProjection)
	}
}

func TestRDMAValidationProjectionIdentityTupleIsUnambiguous(t *testing.T) {
	first := readRDMAValidationGolden(t, "pass.golden.json")
	first.WorkspaceID = "a-b"
	first.ValidationID = "c"
	first.RunID = "rdma-identity-first"

	second := first
	second.WorkspaceID = "a"
	second.ValidationID = "b-c"
	second.RunID = "rdma-identity-second"

	firstProjection, err := ProjectRDMAValidation(first, nil)
	if err != nil {
		t.Fatal(err)
	}
	secondProjection, err := ProjectRDMAValidation(second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if firstProjection.IdempotencyKey == secondProjection.IdempotencyKey ||
		firstProjection.MetricFileID == secondProjection.MetricFileID {
		t.Fatalf(
			"ambiguous identity tuple generated colliding keys: first=%+v second=%+v",
			firstProjection,
			secondProjection,
		)
	}
}

func TestRDMAValidationPhaseRowsSelectTerminalOutOfOrder(t *testing.T) {
	result := readRDMAValidationGolden(t, "pass.golden.json")
	pending := result
	pending.StartedAt = time.Time{}
	pendingProjection, err := ProjectRDMAValidation(pending, nil)
	if err != nil {
		t.Fatal(err)
	}
	runningProjection, err := ProjectRDMAValidation(result, nil)
	if err != nil {
		t.Fatal(err)
	}
	link := artifactLinkForPortalResult(t, result)
	terminalProjection, err := ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	rows := []MetricRow{
		terminalProjection.Metrics[0],
		pendingProjection.Metrics[0],
		runningProjection.Metrics[0],
	}
	summaries := SummarizeMetricRows(MetricFileRecord{
		FileID: terminalProjection.MetricFileID, RunID: result.RunID,
		Project: result.ProjectID, RunGroupID: result.RunGroupID,
		CreatedAt: link.FinalizedAt.Format(time.RFC3339Nano),
	}, rows)
	if len(summaries) != 1 || summaries[0].MetricName != exptelemetry.RunStatusMetricName ||
		summaries[0].LatestValue != 1 ||
		summaries[0].LatestStep == nil || *summaries[0].LatestStep != terminalProjection.Step ||
		summaries[0].LatestWallTime == nil || *summaries[0].LatestWallTime != link.FinalizedAt.UnixMicro() {
		t.Fatalf("out-of-order lifecycle summary = %+v", summaries)
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
	link := artifactLinkForPortalResult(t, result)
	terminalProjection, err := ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.EnrichRunData(ctx, EnrichRunDataOptions{
		Run: terminalProjection.Run, Tags: terminalProjection.Tags, Command: "rdma validation terminal",
	}); err != nil {
		t.Fatal(err)
	}
	stored, err = store.Run(ctx, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != rdmavalidation.RunStateSucceeded ||
		stored.CompletedAt != link.FinalizedAt.Format(time.RFC3339Nano) ||
		stored.ResultURI != link.URI {
		t.Fatalf("stored terminal run = %+v", stored)
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
	metricPath := filepath.Join("metrics", result.RunID, projection.Phase+".parquet")
	absoluteMetricPath := filepath.Join(store.Root, metricPath)
	if err := os.MkdirAll(filepath.Dir(absoluteMetricPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := parquet.WriteFile(absoluteMetricPath, projection.Metrics); err != nil {
		t.Fatal(err)
	}
	file := MetricFileRecord{
		FileID:        projection.MetricFileID,
		Path:          metricPath,
		Format:        "parquet",
		SchemaVersion: MetricSchemaVersion,
		Project:       result.ProjectID,
		RunGroupID:    result.RunGroupID,
		RunID:         result.RunID,
		RowCount:      int64(len(projection.Metrics)),
		CreatedAt:     link.FinalizedAt.Format(time.RFC3339Nano),
	}
	recordOpts := RecordRunDataOptions{
		Run:             projection.Run,
		Tags:            projection.Tags,
		Artifacts:       []ArtifactRecord{*projection.Artifact},
		MetricFiles:     []MetricFileRecord{file},
		MetricSummaries: SummarizeMetricRows(file, projection.Metrics),
		IdempotencyKey:  projection.IdempotencyKey,
		Command:         "rdma validation projection fixture",
		RequestHash:     projection.RequestHash,
	}
	_, err = store.RecordRunData(ctx, recordOpts)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.RecordRunData(ctx, recordOpts)
	if err != nil {
		t.Fatal(err)
	}
	if !retry.Reused {
		t.Fatalf("exact projection retry = %+v, want reused", retry)
	}
	conflictOpts := recordOpts
	conflictOpts.RequestHash = "sha256:" + strings.Repeat("f", 64)
	if _, err := store.RecordRunData(ctx, conflictOpts); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting projection retry error = %v, want ErrConflict", err)
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

	attemptTwo := result
	attemptTwo.Attempt = 2
	attemptTwo.RunID = result.RunID + "-retry"
	rawTwo, err := rdmavalidation.MarshalCanonical(attemptTwo)
	if err != nil {
		t.Fatal(err)
	}
	linkTwo := rdmavalidation.ArtifactLink{
		URI:         "file:///immutable/rdma-validation/" + attemptTwo.RunID + ".json",
		SHA256:      sha256Bytes(rawTwo),
		SizeBytes:   int64(len(rawTwo)),
		FinalizedAt: link.FinalizedAt.Add(time.Minute),
	}
	projectionTwo, err := ProjectRDMAValidation(attemptTwo, &linkTwo)
	if err != nil {
		t.Fatal(err)
	}
	if projectionTwo.Artifact == nil || projectionTwo.Artifact.ArtifactID == projection.Artifact.ArtifactID {
		t.Fatalf("attempt artifact IDs are not distinct: first=%+v second=%+v", projection.Artifact, projectionTwo.Artifact)
	}
	metricPathTwo := filepath.Join("metrics", attemptTwo.RunID, projectionTwo.Phase+".parquet")
	absoluteMetricPathTwo := filepath.Join(store.Root, metricPathTwo)
	if err := os.MkdirAll(filepath.Dir(absoluteMetricPathTwo), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := parquet.WriteFile(absoluteMetricPathTwo, projectionTwo.Metrics); err != nil {
		t.Fatal(err)
	}
	fileTwo := MetricFileRecord{
		FileID:        projectionTwo.MetricFileID,
		Path:          metricPathTwo,
		Format:        "parquet",
		SchemaVersion: MetricSchemaVersion,
		Project:       attemptTwo.ProjectID,
		RunGroupID:    attemptTwo.RunGroupID,
		RunID:         attemptTwo.RunID,
		RowCount:      int64(len(projectionTwo.Metrics)),
		CreatedAt:     linkTwo.FinalizedAt.Format(time.RFC3339Nano),
	}
	if _, err := store.RecordRunData(ctx, RecordRunDataOptions{
		Run:             projectionTwo.Run,
		Tags:            projectionTwo.Tags,
		Artifacts:       []ArtifactRecord{*projectionTwo.Artifact},
		MetricFiles:     []MetricFileRecord{fileTwo},
		MetricSummaries: SummarizeMetricRows(fileTwo, projectionTwo.Metrics),
		IdempotencyKey:  projectionTwo.IdempotencyKey,
		Command:         "rdma validation attempt two fixture",
		RequestHash:     projectionTwo.RequestHash,
	}); err != nil {
		t.Fatalf("persist second terminal attempt: %v", err)
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

func assertRDMAStatusRow(
	t *testing.T,
	projection RDMAValidationProjection,
	result rdmavalidation.Result,
	lifecycle string,
	value float64,
	eventTime time.Time,
) {
	t.Helper()
	if len(projection.Metrics) == 0 {
		t.Fatal("projection has no status row")
	}
	row := projection.Metrics[0]
	if row.MetricName != exptelemetry.RunStatusMetricName || row.Value != value ||
		row.Step == nil || *row.Step != projection.Step ||
		row.WallTime == nil || *row.WallTime != eventTime.UnixMicro() {
		t.Fatalf("status row = %+v", row)
	}
	tags := metricTags(t, row)
	if len(tags) > maxRDMAMetricTags {
		t.Fatalf("status row has %d tags, limit %d: %+v", len(tags), maxRDMAMetricTags, tags)
	}
	assertRDMAMetricIdentityTags(t, tags, result, lifecycle)
	if tags[exptelemetry.RunStatusStateTag] != lifecycle {
		t.Fatalf("status state tag = %q, want %q", tags[exptelemetry.RunStatusStateTag], lifecycle)
	}
	if projection.Artifact == nil {
		if _, ok := tags[rdmavalidation.MetricArtifactURITag]; ok {
			t.Fatalf("in-progress status row contains artifact URI: %+v", tags)
		}
		if _, ok := tags[rdmavalidation.MetricArtifactSHA256Tag]; ok {
			t.Fatalf("in-progress status row contains artifact SHA: %+v", tags)
		}
	}
}

func assertRDMAMetricIdentityTags(
	t *testing.T,
	tags map[string]string,
	result rdmavalidation.Result,
	lifecycle string,
) {
	t.Helper()
	if len(tags) > maxRDMAMetricTags {
		t.Fatalf("metric has %d tags, limit %d: %+v", len(tags), maxRDMAMetricTags, tags)
	}
	for key, want := range map[string]string{
		exptelemetry.TauWorkspaceTag:           result.WorkspaceID,
		exptelemetry.TauClusterTag:             result.Cluster,
		exptelemetry.TauNamespaceTag:           result.Namespace,
		rdmavalidation.MetricValidationIDTag:   result.ValidationID,
		rdmavalidation.MetricSchemaTag:         rdmavalidation.SchemaVersion,
		rdmavalidation.MetricKindTag:           rdmavalidation.Kind,
		rdmavalidation.MetricLifecycleStateTag: lifecycle,
	} {
		if tags[key] != want {
			t.Fatalf("metric tag %s = %q, want %q; tags=%+v", key, tags[key], want, tags)
		}
	}
}

func metricTags(t *testing.T, row MetricRow) map[string]string {
	t.Helper()
	var tags map[string]string
	if err := json.Unmarshal([]byte(row.Tags), &tags); err != nil {
		t.Fatalf("metric %s tags: %v", row.MetricName, err)
	}
	return tags
}

func artifactLinkForPortalResult(t *testing.T, result rdmavalidation.Result) rdmavalidation.ArtifactLink {
	t.Helper()
	raw, err := rdmavalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	return rdmavalidation.ArtifactLink{
		URI:         "file:///immutable/rdma-validation/" + result.ValidationID + ".json",
		SHA256:      sha256Bytes(raw),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
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
