// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/kustoquery"
	corevalidation "github.com/Azure/taugrid/core/rdmavalidation"
	"github.com/Azure/taugrid/portal/internal/expstore"
)

type fakeKustoQuerier struct {
	rows  []kustoquery.Row
	err   error
	query string
}

func (f *fakeKustoQuerier) Query(_ context.Context, query string) ([]kustoquery.Row, error) {
	f.query = query
	return f.rows, f.err
}

func TestKustoReaderMapsTerminalSummaryFailClosed(t *testing.T) {
	observed := time.Date(2026, time.September, 14, 20, 0, 14, 0, time.UTC)
	rows := terminalMetricRows("validation-pass", "run-pass", observed, "pass", "validation_passed", 1)
	query := &fakeKustoQuerier{rows: rows}
	reader := KustoReader{Querier: query, Now: func() time.Time { return observed.Add(time.Minute) }}
	page, err := reader.List(context.Background(), Scope{
		WorkspaceID: "research", Cluster: "cluster-a",
	}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Validations) != 1 {
		t.Fatalf("validations = %+v", page.Validations)
	}
	validation := page.Validations[0]
	if validation.State != StatePassed || validation.HistoricalStatus == nil ||
		*validation.HistoricalStatus != "pass" || validation.Freshness != FreshnessFresh ||
		validation.RunAttempt == nil || *validation.RunAttempt != 1 {
		t.Fatalf("validation state = %+v", validation)
	}
	if validation.Summary == nil || validation.Summary.AlgBWGbps == nil ||
		validation.Summary.AlgBWGbps.Min == nil || *validation.Summary.AlgBWGbps.Min != 13 ||
		validation.Summary.AlgBWGbps.Mean != nil {
		t.Fatalf("summary = %+v", validation.Summary)
	}
	if validation.Transport == nil || validation.Transport.Backend != "nccl" ||
		validation.Transport.IBPositiveEvidence == nil || !*validation.Transport.IBPositiveEvidence {
		t.Fatalf("transport = %+v", validation.Transport)
	}
	if !strings.Contains(query.query, "workspace_id == 'research'") ||
		!strings.Contains(query.query, "cluster == 'cluster-a'") ||
		!strings.Contains(query.query, "arg_max(step") ||
		!strings.Contains(query.query, "let latest_validations = rdma") ||
		!strings.Contains(query.query, "| sort by wall_time desc, validation_id desc\n| take 21") {
		t.Fatalf("query is not scoped or ordered:\n%s", query.query)
	}
}

func TestKustoReaderUsesConfiguredIngestionShape(t *testing.T) {
	query := &fakeKustoQuerier{}
	reader := KustoReader{Querier: query, Ingestion: "remote-write"}
	if _, err := reader.List(context.Background(), Scope{
		WorkspaceID: "research", Cluster: "cluster-a",
	}, ListOptions{Limit: 20}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"ExperimentMetrics",
		"Labels['project']",
		"Labels.workspace_id",
		"source_cluster=tostring(Cluster)",
	} {
		if !strings.Contains(query.query, want) {
			t.Fatalf("remote-write query missing %q:\n%s", want, query.query)
		}
	}
	if strings.Contains(query.query, "TauExpMetrics") {
		t.Fatalf("remote-write query contains projection table:\n%s", query.query)
	}

	query.query = ""
	reader.Ingestion = "unsupported"
	if _, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20}); err == nil ||
		!strings.Contains(err.Error(), "unsupported RDMA validation Kusto ingestion") {
		t.Fatalf("List() error = %v, want unsupported ingestion", err)
	}
	if query.query != "" {
		t.Fatalf("invalid ingestion executed query:\n%s", query.query)
	}
}

func TestKustoReaderConsumesExperimentStoreProjection(t *testing.T) {
	result := readResult(t, "pass.golden.json")
	raw, err := corevalidation.MarshalCanonical(result)
	if err != nil {
		t.Fatal(err)
	}
	link := corevalidation.ArtifactLink{
		URI:         "az://results/rdma-validation/" + result.ValidationID + ".json",
		SHA256:      digest(raw),
		SizeBytes:   int64(len(raw)),
		FinalizedAt: result.Cleanup.CompletedAt.Add(time.Second),
	}
	projection, err := expstore.ProjectRDMAValidation(result, &link)
	if err != nil {
		t.Fatal(err)
	}
	rows := make([]kustoquery.Row, 0, len(projection.Metrics))
	for _, metric := range projection.Metrics {
		row := kustoquery.Row{
			"project": projection.Run.Project, "experiment_id": projection.Run.ExperimentID,
			"run_group_id": projection.Run.RunGroupID, "run_id": metric.RunID,
			"metric_name": metric.MetricName, "value": metric.Value, "source": metric.Source,
			"tags": metric.Tags,
		}
		if metric.Step != nil {
			row["step"] = *metric.Step
		}
		if metric.WallTime != nil {
			row["wall_time"] = time.UnixMicro(*metric.WallTime).UTC().Format(time.RFC3339Nano)
		}
		rows = append(rows, row)
	}

	reader := KustoReader{
		Querier: &fakeKustoQuerier{rows: rows},
		Now:     func() time.Time { return result.ObservedAt.Add(time.Minute) },
	}
	page, err := reader.List(context.Background(), Scope{
		WorkspaceID: result.WorkspaceID, Cluster: result.Cluster,
	}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Validations) != 1 {
		t.Fatalf("projection validation = %+v", page.Validations)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, page.Validations[0].ObservedAt)
	if err != nil {
		t.Fatal(err)
	}
	validUntil, err := time.Parse(time.RFC3339Nano, page.Validations[0].ValidUntil)
	if err != nil {
		t.Fatal(err)
	}
	if page.Validations[0].State != StatePassed ||
		!observedAt.Equal(result.ObservedAt.Truncate(time.Microsecond)) ||
		!validUntil.Equal(result.ValidUntil.Truncate(time.Microsecond)) {
		t.Fatalf(
			"projection validation = %+v; observed=%s want=%s validUntil=%s want=%s",
			page.Validations, observedAt, result.ObservedAt.Truncate(time.Microsecond),
			validUntil, result.ValidUntil.Truncate(time.Microsecond),
		)
	}

	detail, err := reader.Get(context.Background(), Scope{
		WorkspaceID: result.WorkspaceID,
		Cluster:     result.Cluster,
		FetchArtifact: func(_ context.Context, metadata ArtifactMetadata) ([]byte, ArtifactMetadata, error) {
			if metadata.ValidationID != result.ValidationID ||
				metadata.RunID != result.RunID ||
				metadata.URI != link.URI ||
				metadata.SHA256 != link.SHA256 {
				t.Fatalf("artifact metadata = %+v", metadata)
			}
			metadata.ContentType = corevalidation.ArtifactContentType
			metadata.SizeBytes = int64(len(raw))
			return raw, metadata, nil
		},
	}, result.ValidationID)
	if err != nil {
		t.Fatal(err)
	}
	if detail.State != StatePassed ||
		detail.Namespace != result.Namespace ||
		detail.ArtifactVerification == nil ||
		detail.ArtifactVerification.State != "verified" {
		t.Fatalf("projection detail = %+v", detail)
	}
}

func TestKustoReaderUsesNewestLifecycleAndNeverGreensIncompleteData(t *testing.T) {
	started := time.Date(2026, time.September, 14, 20, 0, 0, 0, time.UTC)
	rows := []kustoquery.Row{
		lifecycleMetricRow("validation-running", "run-running", started, corevalidation.RunStatePending),
		lifecycleMetricRow("validation-running", "run-running", started.Add(time.Minute), corevalidation.RunStateRunning),
	}
	reader := KustoReader{
		Querier: &fakeKustoQuerier{rows: rows},
		Now:     func() time.Time { return started.Add(2 * time.Minute) },
	}
	page, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Validations) != 1 || page.Validations[0].State != StateRunning ||
		page.Validations[0].HistoricalStatus != nil || page.Validations[0].Freshness != FreshnessNotApplicable {
		t.Fatalf("running validation = %+v", page.Validations)
	}

	incomplete := []kustoquery.Row{lifecycleMetricRow(
		"validation-incomplete", "run-incomplete", started.Add(3*time.Minute), corevalidation.RunStateSucceeded,
	)}
	reader.Querier = &fakeKustoQuerier{rows: incomplete}
	page, err = reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Validations) != 1 || page.Validations[0].State != StateUnknown {
		t.Fatalf("incomplete terminal validation = %+v", page.Validations)
	}
}

func TestKustoReaderMarksStaleUnsupportedAndInconsistentUnknown(t *testing.T) {
	observed := time.Date(2026, time.September, 14, 20, 0, 0, 0, time.UTC)
	stale := terminalMetricRows("validation-stale", "run-stale", observed, "pass", "validation_passed", 1)
	reader := KustoReader{
		Querier: &fakeKustoQuerier{rows: stale},
		Now:     func() time.Time { return observed.Add(24*time.Hour + time.Nanosecond) },
	}

	page, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Validations[0].State != StateStale ||
		page.Validations[0].HistoricalStatus == nil || *page.Validations[0].HistoricalStatus != "pass" {
		t.Fatalf("stale validation = %+v", page.Validations[0])
	}

	unsupported := lifecycleMetricRow("validation-v2", "run-v2", observed, corevalidation.RunStateRunning)
	var tags map[string]string
	if err := jsonUnmarshalTags(unsupported, &tags); err != nil {
		t.Fatal(err)
	}
	tags[validationSchemaTag] = "rdma-validation.v2"
	unsupported["tags"] = mustJSON(t, tags)
	unsupported["validation_schema"] = "rdma-validation.v2"
	reader.Querier = &fakeKustoQuerier{rows: []kustoquery.Row{unsupported}}
	page, err = reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Validations[0].State != StateUnknown || page.Validations[0].ReasonCode != "unsupported_schema" {
		t.Fatalf("unsupported validation = %+v", page.Validations[0])
	}

	inconsistent := terminalMetricRows("validation-bad", "run-bad", observed, "pass", "validation_passed", -1)
	reader.Querier = &fakeKustoQuerier{rows: inconsistent}
	page, err = reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Validations[0].State != StateUnknown || page.Validations[0].ReasonCode != "malformed_status" {
		t.Fatalf("inconsistent validation = %+v", page.Validations[0])
	}

	mixedAttempt := terminalMetricRows("validation-mixed", "run-mixed", observed, "pass", "validation_passed", 1)
	if err := jsonUnmarshalTags(mixedAttempt[2], &tags); err != nil {
		t.Fatal(err)
	}
	tags[lifecycleStateTag] = corevalidation.RunStateRunning
	mixedAttempt[2]["tags"] = mustJSON(t, tags)
	reader.Querier = &fakeKustoQuerier{rows: mixedAttempt}
	page, err = reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if page.Validations[0].State != StateUnknown || page.Validations[0].ReasonCode != "malformed_status" {
		t.Fatalf("mixed terminal projection = %+v", page.Validations[0])
	}
}

func TestKustoReaderMissingBandwidthRemainsUnknown(t *testing.T) {
	observed := time.Date(2026, time.September, 14, 20, 0, 0, 0, time.UTC)
	rows := terminalMetricRows(
		"validation-missing-bandwidth", "run-missing-bandwidth", observed,
		"pass", "validation_passed", 1,
	)
	filtered := rows[:0]
	for _, row := range rows {
		if row.Str("metric_name") != corevalidation.MetricAlgBWGbps &&
			row.Str("metric_name") != corevalidation.MetricBusBWGbps {
			filtered = append(filtered, row)
		}
	}
	reader := KustoReader{
		Querier: &fakeKustoQuerier{rows: filtered},
		Now:     func() time.Time { return observed.Add(time.Minute) },
	}
	page, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Validations) != 1 || page.Validations[0].State != StateUnknown ||
		page.Validations[0].HistoricalStatus != nil ||
		page.Validations[0].Summary != nil {
		t.Fatalf("missing bandwidth validation = %+v", page.Validations)
	}
}

func TestKustoReaderGetVerifiesArtifactThroughInjectedFetcher(t *testing.T) {
	raw := readGolden(t, "pass.golden.json")
	var result corevalidation.Result
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	rows := terminalMetricRows(
		result.ValidationID, result.RunID, result.ObservedAt,
		string(result.Status), string(result.Reason), 1,
	)
	expectedHash := digest(raw)
	for _, row := range rows {
		var tags map[string]string
		if err := jsonUnmarshalTags(row, &tags); err != nil {
			t.Fatal(err)
		}
		tags[corevalidation.MetricArtifactURITag] = "az://results/" + result.ValidationID + ".json"
		tags[corevalidation.MetricArtifactSHA256Tag] = expectedHash
		tags[exptelemetry.TauWorkspaceTag] = result.WorkspaceID
		tags[exptelemetry.TauClusterTag] = result.Cluster
		tags[exptelemetry.TauNamespaceTag] = result.Namespace
		row["tags"] = mustJSON(t, tags)
		row["workspace_id"] = result.WorkspaceID
		row["cluster"] = result.Cluster
		row["namespace"] = result.Namespace
	}
	fetched := false
	reader := KustoReader{
		Querier: &fakeKustoQuerier{rows: rows},
		Now:     func() time.Time { return result.ObservedAt.Add(time.Minute) },
	}
	detail, err := reader.Get(context.Background(), Scope{
		WorkspaceID: result.WorkspaceID,
		Cluster:     result.Cluster,
		FetchArtifact: func(_ context.Context, metadata ArtifactMetadata) ([]byte, ArtifactMetadata, error) {
			fetched = true
			if metadata.ValidationID != result.ValidationID || metadata.RunID != result.RunID ||
				metadata.URI != "az://results/"+result.ValidationID+".json" || metadata.SHA256 != expectedHash {
				t.Fatalf("expected metadata = %+v", metadata)
			}
			metadata.ContentType = corevalidation.ArtifactContentType
			metadata.SizeBytes = int64(len(raw))
			return raw, metadata, nil
		},
	}, result.ValidationID)
	if err != nil {
		t.Fatal(err)
	}
	if !fetched || detail.State != StatePassed || detail.ArtifactVerification == nil ||
		detail.ArtifactVerification.State != "verified" {
		t.Fatalf("detail = %+v fetched=%v", detail, fetched)
	}
}

func TestKustoReaderGetRejectsMissingLinkageAndScope(t *testing.T) {
	observed := time.Date(2026, time.September, 14, 20, 0, 0, 0, time.UTC)
	rows := terminalMetricRows("validation-no-link", "run-no-link", observed, "pass", "validation_passed", 1)
	for _, row := range rows {
		var tags map[string]string
		if err := jsonUnmarshalTags(row, &tags); err != nil {
			t.Fatal(err)
		}
		delete(tags, corevalidation.MetricArtifactURITag)
		row["tags"] = mustJSON(t, tags)
	}
	reader := KustoReader{Querier: &fakeKustoQuerier{rows: rows}}
	if _, err := reader.Get(context.Background(), Scope{WorkspaceID: "research"}, "validation-no-link"); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("Get() error = %v, want integrity failure", err)
	}

	rows[0]["workspace_id"] = "other"
	reader.Querier = &fakeKustoQuerier{rows: rows}
	if _, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20}); !errors.Is(err, ErrScopeMismatch) {
		t.Fatalf("List() error = %v, want scope mismatch", err)
	}

	rows = terminalMetricRows("validation-bad-namespace", "run-bad-namespace", observed, "pass", "validation_passed", 1)
	var tags map[string]string
	if err := jsonUnmarshalTags(rows[1], &tags); err != nil {
		t.Fatal(err)
	}
	tags[exptelemetry.TauNamespaceTag] = "unexpected-namespace"
	rows[1]["namespace"] = "unexpected-namespace"
	rows[1]["tags"] = mustJSON(t, tags)
	reader.Querier = &fakeKustoQuerier{rows: rows}
	if _, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20}); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("List() namespace error = %v, want integrity failure", err)
	}
}

func TestKustoReaderPaginatesByTimeAndValidationID(t *testing.T) {
	observed := time.Date(2026, time.September, 14, 20, 0, 0, 0, time.UTC)
	rows := append(
		terminalMetricRows("validation-b", "run-b", observed, "pass", "validation_passed", 1),
		terminalMetricRows("validation-a", "run-a", observed, "fail", "runtime_error", -1)...,
	)
	query := &fakeKustoQuerier{rows: rows}
	reader := KustoReader{Querier: query, Now: func() time.Time { return observed }}
	page, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Validations) != 1 || page.Validations[0].ValidationID != "validation-b" ||
		!page.Truncated || page.NextCursor == "" {
		t.Fatalf("page = %+v", page)
	}
	query.rows = terminalMetricRows("validation-a", "run-a", observed, "fail", "runtime_error", -1)
	if _, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{
		Limit: 1, Cursor: page.NextCursor,
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query.query, "validation_id < 'validation-b'") {
		t.Fatalf("cursor query =\n%s", query.query)
	}
}

func TestKustoReaderSurfacesQueryErrors(t *testing.T) {
	reader := KustoReader{Querier: &fakeKustoQuerier{err: errors.New("ADX unavailable")}}
	if _, err := reader.List(context.Background(), Scope{WorkspaceID: "research"}, ListOptions{Limit: 20}); err == nil ||
		!strings.Contains(err.Error(), "query RDMA validations") {
		t.Fatalf("List() error = %v", err)
	}
}

func lifecycleMetricRow(validationID, runID string, at time.Time, lifecycle string) kustoquery.Row {
	tags := baseMetricTags(validationID, lifecycle)
	tags[exptelemetry.RunStatusStateTag] = lifecycle
	step := "0"
	value := float64(0)
	if lifecycle == corevalidation.RunStateRunning {
		step = "1"
	} else if lifecycle == corevalidation.RunStateSucceeded || lifecycle == corevalidation.RunStateFailed {
		step = "2"
		if lifecycle == corevalidation.RunStateSucceeded {
			value = 1
		} else {
			value = -1
		}
	}
	return kustoquery.Row{
		"workspace_id": "research", "cluster": "cluster-a", "namespace": "taugrid-rdma-diagnostic",
		"validation_id": validationID, "validation_schema": corevalidation.SchemaVersion,
		"validation_kind": corevalidation.Kind, "lifecycle_state": lifecycle,
		"project": "taugrid", "experiment_id": "rdma-validation", "run_group_id": "manual",
		"run_id": runID, "metric_name": exptelemetry.RunStatusMetricName,
		"step": step, "wall_time": at.UTC().Format(time.RFC3339Nano), "value": value,
		"tags": mustJSONNoTest(tags),
	}
}

func terminalMetricRows(
	validationID, runID string,
	observed time.Time,
	status, reason string,
	statusValue float64,
) []kustoquery.Row {
	uri := "az://results/" + validationID + ".json"
	hash := "sha256:" + strings.Repeat("a", 64)
	tags := baseMetricTags(validationID, terminalLifecycle(status))
	tags[validationStatusTag] = status
	tags[validationReasonTag] = reason
	tags["tau.rdma_validation.backend"] = "nccl"
	tags["tau.rdma_validation.operation"] = "all_reduce"
	tags[corevalidation.MetricArtifactURITag] = uri
	tags[corevalidation.MetricArtifactSHA256Tag] = hash
	makeRow := func(metric string, value float64) kustoquery.Row {
		row := lifecycleMetricRow(validationID, runID, observed.Add(time.Second), terminalLifecycle(status))
		row["metric_name"] = metric
		row["wall_time"] = observed.UTC().Format(time.RFC3339Nano)
		row["value"] = value
		row["tags"] = mustJSONNoTest(tags)
		return row
	}
	rows := []kustoquery.Row{lifecycleMetricRow(
		validationID, runID, observed.Add(time.Second), terminalLifecycle(status),
	)}
	var markerTags map[string]string
	_ = jsonUnmarshalTags(rows[0], &markerTags)
	for key, value := range tags {
		markerTags[key] = value
	}
	rows[0]["tags"] = mustJSONNoTest(markerTags)
	rows = append(rows,
		makeRow(corevalidation.MetricStatus, statusValue),
		makeRow(corevalidation.MetricAlgBWGbps, 13),
		makeRow(corevalidation.MetricBusBWGbps, 13),
		makeRow(corevalidation.MetricMaxError, 0),
		makeRow(corevalidation.MetricDurationSeconds, 11),
		makeRow(corevalidation.MetricNetIBObserved, 1),
		makeRow(corevalidation.MetricSocketFallbackObserved, 0),
		makeRow(corevalidation.MetricCleanupComplete, 1),
	)
	return rows
}

func terminalLifecycle(status string) string {
	if status == "pass" {
		return corevalidation.RunStateSucceeded
	}
	return corevalidation.RunStateFailed
}

func baseMetricTags(validationID, lifecycle string) map[string]string {
	return map[string]string{
		exptelemetry.TauWorkspaceTag: "research",
		exptelemetry.TauClusterTag:   "cluster-a",
		exptelemetry.TauNamespaceTag: "taugrid-rdma-diagnostic",
		validationIDTag:              validationID,
		validationSchemaTag:          corevalidation.SchemaVersion,
		validationKindTag:            corevalidation.Kind,
		lifecycleStateTag:            lifecycle,
	}
}

func jsonUnmarshalTags(row kustoquery.Row, tags *map[string]string) error {
	return json.Unmarshal([]byte(row.Str("tags")), tags)
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func mustJSONNoTest(value any) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}
