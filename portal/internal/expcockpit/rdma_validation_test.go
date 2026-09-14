// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

func TestRDMAValidationKustoRowsRemainWorkspaceScopedAndTimeOrdered(t *testing.T) {
	sample := readRDMACockpitGolden(t, "pass.golden.json")
	sample.WorkspaceID = "sample"
	sampleProjection := terminalRDMACockpitProjection(t, sample)

	research := sample
	research.WorkspaceID = "research"
	research.Transport.SocketFallbackObserved = cockpitBoolPointer(true)
	if err := rdmavalidation.Finalize(&research); err != nil {
		t.Fatal(err)
	}
	researchProjection := terminalRDMACockpitProjection(t, research)

	pending := sample
	pending.StartedAt = time.Time{}
	pendingProjection, err := expstore.ProjectRDMAValidation(pending, nil)
	if err != nil {
		t.Fatal(err)
	}
	runningProjection, err := expstore.ProjectRDMAValidation(sample, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sampleProjection.MetricFileID == researchProjection.MetricFileID ||
		sampleProjection.IdempotencyKey == researchProjection.IdempotencyKey {
		t.Fatal("same-named validations in different workspaces share projection identities")
	}

	for name, projection := range map[string]expstore.RDMAValidationProjection{
		rdmavalidation.RunStatePending: pendingProjection,
		rdmavalidation.RunStateRunning: runningProjection,
	} {
		eventTime := time.UnixMicro(*projection.Metrics[0].WallTime).UTC()
		source := KustoSource{
			Metrics: projectionKustoRows(sample, projection), WorkspaceID: "sample",
			StorePath: "kusto://TauExpMetrics", Now: func() time.Time { return eventTime.Add(time.Minute) },
		}
		runs, err := source.SearchRuns(context.Background(), expstore.RunSearchOptions{
			Workspace: "sample", Target: sample.RunID, Limit: 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(runs.Runs) != 1 || runs.Runs[0].LifecycleState != name {
			t.Fatalf("%s discovery = %+v", name, runs.Runs)
		}
	}
	sameTimeRunning := sample
	sameTimeRunning.StartedAt = sameTimeRunning.CreatedAt
	sameTimeProjection, err := expstore.ProjectRDMAValidation(sameTimeRunning, nil)
	if err != nil {
		t.Fatal(err)
	}
	tiedRows := append(
		projectionKustoRows(sameTimeRunning, sameTimeProjection),
		projectionKustoRows(pending, pendingProjection)...,
	)
	latestTied, ok := latestKustoStatusRow(tiedRows)
	if !ok || kustoRunStatusState(latestTied) != rdmavalidation.RunStateRunning {
		t.Fatalf("same-time pending/running status selected %+v, found=%v", latestTied, ok)
	}

	newerAttempt := sample
	newerAttempt.Attempt = 2
	newerAttemptProjection, err := expstore.ProjectRDMAValidation(newerAttempt, nil)
	if err != nil {
		t.Fatal(err)
	}
	overlappingAttempts := append(
		projectionKustoRows(sample, sampleProjection),
		projectionKustoRows(newerAttempt, newerAttemptProjection)...,
	)
	latestAttempt, ok := latestKustoStatusRow(overlappingAttempts)
	if !ok || kustoRunStatusState(latestAttempt) != rdmavalidation.RunStateRunning ||
		latestAttempt.Step != newerAttemptProjection.Step {
		t.Fatalf("newer running attempt selected %+v, found=%v", latestAttempt, ok)
	}
	mergedAttemptTags := kustoMergedTags(overlappingAttempts)
	if mergedAttemptTags[rdmavalidation.MetricLifecycleStateTag] != rdmavalidation.RunStateRunning ||
		mergedAttemptTags["tau.status.state"] != rdmavalidation.RunStateRunning {
		t.Fatalf("newer attempt lifecycle tags = %+v", mergedAttemptTags)
	}

	rows := append([]KustoMetricRow{}, projectionKustoRows(research, researchProjection)...)
	rows = append(rows, projectionKustoRows(sample, sampleProjection)...)
	rows = append(rows, projectionKustoRows(pending, pendingProjection)...)
	rows = append(rows, projectionKustoRows(sample, runningProjection)...)

	sampleSource := KustoSource{
		Metrics: rows, WorkspaceID: "sample", StorePath: "kusto://TauExpMetrics",
		Now: func() time.Time { return sample.ValidUntil },
	}
	sampleRuns, err := sampleSource.SearchRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "sample", Target: sample.RunID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sampleRuns.Runs) != 1 ||
		sampleRuns.Runs[0].RunID != sample.RunID ||
		sampleRuns.Runs[0].LifecycleState != rdmavalidation.RunStateSucceeded ||
		sampleRuns.Runs[0].Tags["tau_workspace"] != "sample" {
		t.Fatalf("sample workspace runs = %+v", sampleRuns.Runs)
	}

	researchSource := sampleSource
	researchSource.WorkspaceID = "research"
	researchRuns, err := researchSource.SearchRuns(context.Background(), expstore.RunSearchOptions{
		Workspace: "research", Target: research.RunID, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(researchRuns.Runs) != 1 ||
		researchRuns.Runs[0].LifecycleState != rdmavalidation.RunStateFailed ||
		researchRuns.Runs[0].Tags["tau_workspace"] != "research" {
		t.Fatalf("research workspace runs = %+v", researchRuns.Runs)
	}

	filtered := filterKustoRowsByWorkspace(rows, "sample")
	latest, ok := latestKustoStatusRow(filtered)
	wantTerminalTime := time.UnixMicro(*sampleProjection.Metrics[0].WallTime).UTC().Format(time.RFC3339Nano)
	if !ok || kustoRunStatusState(latest) != rdmavalidation.RunStateSucceeded ||
		latest.WallTime != wantTerminalTime {
		t.Fatalf("latest sample status = %+v, found=%v", latest, ok)
	}

	series, err := sampleSource.BuildSeries(context.Background(), SeriesOptions{
		Target: sample.ExperimentID, Workspace: "sample", Metric: rdmavalidation.MetricStatus,
		RunID: sample.RunID, MaxRuns: 10, MaxMetricRows: 100, MaxPoints: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(series.Chart.Series) != 1 || len(series.Chart.Series[0].Values) != 1 ||
		series.Chart.Series[0].Values[0].Value != 1 {
		t.Fatalf("workspace-scoped RDMA detail series = %+v", series.Chart.Series)
	}
	if _, err := sampleSource.BuildSeries(context.Background(), SeriesOptions{
		Target: sample.ExperimentID, Workspace: "research", Metric: rdmavalidation.MetricStatus,
		RunID: sample.RunID, MaxRuns: 10, MaxMetricRows: 100, MaxPoints: 100,
	}); err == nil {
		t.Fatal("sample-scoped Kusto source admitted research workspace detail")
	}
}

func terminalRDMACockpitProjection(
	t *testing.T,
	result rdmavalidation.Result,
) expstore.RDMAValidationProjection {
	t.Helper()
	raw, err := rdmavalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	link := rdmavalidation.ArtifactLink{
		URI:         "artifacts/" + result.WorkspaceID + "/" + result.ValidationID + ".json",
		SHA256:      "sha256:" + hex.EncodeToString(sum[:]),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
	}
	projection, err := expstore.ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

func projectionKustoRows(
	result rdmavalidation.Result,
	projection expstore.RDMAValidationProjection,
) []KustoMetricRow {
	rows := make([]KustoMetricRow, 0, len(projection.Metrics))
	for _, row := range projection.Metrics {
		wallTime := ""
		if row.WallTime != nil {
			wallTime = time.UnixMicro(*row.WallTime).UTC().Format(time.RFC3339Nano)
		}
		step := int64(0)
		if row.Step != nil {
			step = *row.Step
		}
		unit := ""
		if row.Unit != nil {
			unit = *row.Unit
		}
		rows = append(rows, KustoMetricRow{
			Project: result.ProjectID, ExperimentID: result.ExperimentID,
			RunGroupID: result.RunGroupID, RunID: result.RunID,
			MetricName: row.MetricName, Step: step, WallTime: wallTime,
			Value: row.Value, Unit: unit, Source: row.Source,
			MetricFileID: projection.MetricFileID, Tags: row.Tags,
		})
	}
	return rows
}

func readRDMACockpitGolden(t *testing.T, name string) rdmavalidation.Result {
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

func cockpitBoolPointer(value bool) *bool {
	return &value
}
