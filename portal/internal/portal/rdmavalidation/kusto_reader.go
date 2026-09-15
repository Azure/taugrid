// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/kustoquery"
	corevalidation "github.com/Azure/taugrid/core/rdmavalidation"
)

const (
	validationIDTag     = corevalidation.MetricValidationIDTag
	validationSchemaTag = corevalidation.MetricSchemaTag
	validationKindTag   = corevalidation.MetricKindTag
	lifecycleStateTag   = corevalidation.MetricLifecycleStateTag
	validationStatusTag = corevalidation.MetricValidationStatusTag
	validationReasonTag = corevalidation.MetricValidationReasonTag
)

type KustoReader struct {
	Querier   kustoquery.Querier
	Ingestion string
	Now       func() time.Time
}

func terminalLifecycleConsistent(lifecycle, historical string) bool {
	if historical == "pass" {
		return lifecycle == corevalidation.RunStateSucceeded
	}
	return lifecycle == corevalidation.RunStateFailed
}

func terminalMarkerConsistent(lifecycle metricObservation, historical string) bool {
	if !lifecycle.hasValue {
		return false
	}
	if historical == "pass" {
		return lifecycle.value == 1
	}
	return lifecycle.value == -1
}

func completePassProjection(metrics map[string]metricObservation) bool {
	algbw, algOK := scalarValue(metrics[corevalidation.MetricAlgBWGbps])
	busbw, busOK := scalarValue(metrics[corevalidation.MetricBusBWGbps])
	maxError, errorOK := scalarValue(metrics[corevalidation.MetricMaxError])
	duration, durationOK := scalarValue(metrics[corevalidation.MetricDurationSeconds])
	netIB := boolMetric(metrics[corevalidation.MetricNetIBObserved])
	socket := boolMetric(metrics[corevalidation.MetricSocketFallbackObserved])
	cleanup := boolMetric(metrics[corevalidation.MetricCleanupComplete])
	return algOK && algbw > 0 && busOK && busbw > 0 &&
		errorOK && maxError == 0 && durationOK && duration >= 0 &&
		netIB != nil && *netIB && socket != nil && !*socket &&
		cleanup != nil && *cleanup
}

func consistentArtifactLinks(metrics map[string]metricObservation, uri, hash string) bool {
	for _, metric := range metrics {
		if metric.artifactURI != uri || metric.artifactHash != hash {
			return false
		}
	}
	return true
}

func consistentTerminalMetrics(
	metrics map[string]metricObservation,
	lifecycle metricObservation,
	status metricObservation,
) bool {
	for _, metric := range metrics {
		if metric.lifecycle != lifecycle.lifecycle ||
			metric.step != lifecycle.step ||
			!metric.wallTime.Equal(status.wallTime) ||
			metric.status != status.status ||
			metric.reason != status.reason {
			return false
		}
	}
	return true
}

func knownReason(reason string) bool {
	switch corevalidation.ReasonCode(reason) {
	case corevalidation.ReasonValidationPassed,
		corevalidation.ReasonSocketFallbackObserved,
		corevalidation.ReasonIBTransportNotProven,
		corevalidation.ReasonTransportFailure,
		corevalidation.ReasonPeerAuthenticationFailed,
		corevalidation.ReasonPlacementMismatch,
		corevalidation.ReasonTopologyMismatch,
		corevalidation.ReasonTopologyEvidenceIncomplete,
		corevalidation.ReasonCorrectnessError,
		corevalidation.ReasonNonzeroExit,
		corevalidation.ReasonCleanupIncomplete,
		corevalidation.ReasonMissingRequiredEvidence,
		corevalidation.ReasonInvalidMeasurement,
		corevalidation.ReasonEvidenceIntegrityMissing,
		corevalidation.ReasonParserRejected,
		corevalidation.ReasonRuntimeError:
		return true
	default:
		return false
	}
}

type metricObservation struct {
	workspaceID  string
	cluster      string
	namespace    string
	validationID string
	schema       string
	kind         string
	lifecycle    string
	status       string
	reason       string
	backend      string
	operation    string
	project      string
	experimentID string
	runGroupID   string
	runID        string
	metric       string
	wallTime     time.Time
	step         int64
	value        float64
	hasValue     bool
	artifactURI  string
	artifactHash string
}

type validationAggregate struct {
	identity  metricObservation
	lifecycle metricObservation
	metrics   map[string]metricObservation
}

func (r KustoReader) Summary(ctx context.Context, scope Scope) (Summary, error) {
	page, err := r.List(ctx, scope, ListOptions{Limit: 1})
	if err != nil {
		return Summary{}, err
	}
	var latest *Validation
	if len(page.Validations) == 1 {
		value := page.Validations[0]
		if value.State == StatePassed || value.State == StateFailed || value.State == StateStale {
			detail, err := r.Get(ctx, scope, value.ValidationID)
			if err != nil {
				return Summary{}, err
			}
			value = detail.Validation
		}
		latest = &value
	}
	return Summary{Latest: latest, GeneratedAt: r.now().Format(time.RFC3339Nano)}, nil
}

func (r KustoReader) List(ctx context.Context, scope Scope, opts ListOptions) (Page, error) {
	if r.Querier == nil {
		return Page{}, ErrUnavailable
	}
	if strings.TrimSpace(scope.WorkspaceID) == "" {
		return Page{}, fmt.Errorf("%w: workspace is required", ErrScopeMismatch)
	}
	if opts.Limit <= 0 || opts.Limit > 100 {
		return Page{}, fmt.Errorf("RDMA validation limit must be from 1 to 100")
	}
	cursor, err := DecodeCursor(opts.Cursor)
	if err != nil {
		return Page{}, err
	}
	query, err := buildValidationQueryForIngestion(scope, "", cursor, opts.Limit+1, r.Ingestion)
	if err != nil {
		return Page{}, err
	}
	rows, err := r.Querier.Query(ctx, query)
	if err != nil {
		return Page{}, fmt.Errorf("query RDMA validations: %w", err)
	}
	aggregates, err := aggregateMetricRows(rows, scope)
	if err != nil {
		return Page{}, err
	}
	sort.Slice(aggregates, func(i, j int) bool {
		if aggregates[i].lifecycle.wallTime.Equal(aggregates[j].lifecycle.wallTime) {
			return aggregates[i].lifecycle.validationID > aggregates[j].lifecycle.validationID
		}
		return aggregates[i].lifecycle.wallTime.After(aggregates[j].lifecycle.wallTime)
	})
	truncated := len(aggregates) > opts.Limit
	if truncated {
		aggregates = aggregates[:opts.Limit]
	}
	validations := make([]Validation, 0, len(aggregates))
	for _, aggregate := range aggregates {
		validations = append(validations, aggregate.validation(r.now()))
	}
	page := Page{
		Validations: validations, Truncated: truncated,
		GeneratedAt: r.now().Format(time.RFC3339Nano),
	}
	if truncated && len(aggregates) > 0 {
		last := aggregates[len(aggregates)-1].lifecycle
		page.NextCursor, err = EncodeCursor(Cursor{
			SortAt: last.wallTime, ValidationID: last.validationID,
		})
		if err != nil {
			return Page{}, err
		}
	}
	return page, nil
}

func (r KustoReader) Get(ctx context.Context, scope Scope, validationID string) (Detail, error) {
	if r.Querier == nil {
		return Detail{}, ErrUnavailable
	}
	query, err := buildValidationQueryForIngestion(scope, validationID, Cursor{}, 1, r.Ingestion)
	if err != nil {
		return Detail{}, err
	}
	rows, err := r.Querier.Query(ctx, query)
	if err != nil {
		return Detail{}, fmt.Errorf("query RDMA validation %q: %w", validationID, err)
	}
	aggregates, err := aggregateMetricRows(rows, scope)
	if err != nil {
		return Detail{}, err
	}
	if len(aggregates) == 0 {
		return Detail{}, ErrNotFound
	}
	var aggregate validationAggregate
	found := false
	for _, candidate := range aggregates {
		if candidate.lifecycle.validationID == validationID {
			aggregate = candidate
			found = true
			break
		}
	}
	if !found {
		return Detail{}, ErrNotFound
	}
	summary := aggregate.validation(r.now())
	if summary.ValidationID != validationID {
		return Detail{}, ErrNotFound
	}
	if aggregate.lifecycle.schema != corevalidation.SchemaVersion ||
		aggregate.lifecycle.kind != corevalidation.Kind {
		return Detail{}, ErrUnsupportedSchema
	}
	if summary.State == StateRunning {
		return Detail{Validation: summary, SchemaVersion: aggregate.lifecycle.schema, Kind: aggregate.lifecycle.kind}, nil
	}
	if summary.HistoricalStatus == nil {
		return Detail{}, fmt.Errorf("%w: terminal projection is incomplete or inconsistent", ErrArtifactIntegrity)
	}
	status, ok := aggregate.metrics[corevalidation.MetricStatus]
	if !ok || status.artifactURI == "" || status.artifactHash == "" {
		return Detail{}, fmt.Errorf("%w: terminal result has no verified artifact linkage", ErrArtifactIntegrity)
	}
	if scope.FetchArtifact == nil {
		return Detail{}, ErrUnavailable
	}
	expected := ArtifactMetadata{
		ValidationID: validationID, RunID: aggregate.lifecycle.runID,
		WorkspaceID: scope.WorkspaceID, URI: status.artifactURI, SHA256: status.artifactHash,
	}
	raw, metadata, err := scope.FetchArtifact(ctx, expected)
	if err != nil {
		return Detail{}, err
	}
	detail, err := DecodeArtifact(raw, metadata, r.now())
	if err != nil {
		return Detail{}, err
	}
	if detail.RunID != aggregate.lifecycle.runID ||
		detail.RunAttempt == nil || summary.RunAttempt == nil || *detail.RunAttempt != *summary.RunAttempt ||
		detail.HistoricalStatus == nil || *detail.HistoricalStatus != *summary.HistoricalStatus ||
		detail.ReasonCode != summary.ReasonCode {
		return Detail{}, fmt.Errorf("%w: artifact outcome does not match the selected terminal lifecycle", ErrArtifactIntegrity)
	}
	if detail.Cluster != aggregate.lifecycle.cluster ||
		detail.Namespace != aggregate.lifecycle.namespace ||
		(scope.Cluster != "" && detail.Cluster != scope.Cluster) {
		return Detail{}, ErrScopeMismatch
	}
	return detail, nil
}

func (r KustoReader) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func buildValidationQueryForIngestion(scope Scope, validationID string, cursor Cursor, limit int, ingestion string) (string, error) {
	var source string
	switch strings.ToLower(strings.TrimSpace(ingestion)) {
	case "", "projection":
		source = fmt.Sprintf(`%s
| extend source_workspace='', source_cluster=''
| extend experimentIdOf=iff(isempty(column_ifexists('experiment_id', '')), column_ifexists('question_id', ''), column_ifexists('experiment_id', ''))
| project exported_at=todatetime(exported_at), ['project']=tostring(['project']),
          experiment_id=tostring(experimentIdOf),
          run_group_id=tostring(run_group_id), run_id=tostring(run_id),
          metric_name=tostring(metric_name), step=tolong(step), wall_time=todatetime(wall_time),
          value=todouble(value), tags=tostring(tags), source_workspace, source_cluster
`, expkusto.DefaultProjectionTable)
	case "remote-write":
		source = fmt.Sprintf(`%s
| project exported_at=todatetime(Timestamp), ['project']=tostring(Labels['project']),
          experiment_id=coalesce(tostring(Labels.experiment_id), tostring(Labels.question_id), ''),
          run_group_id=tostring(Labels.run_group_id), run_id=tostring(Labels.run_id),
          metric_name=tostring(Labels.metric_name), step=tolong(Labels.step), wall_time=todatetime(Timestamp),
          value=todouble(Value), tags=tostring(Labels.tags),
          source_workspace=tostring(Labels.workspace_id), source_cluster=tostring(Cluster)
`, expkusto.DefaultRemoteWriteTable)
	default:
		return "", fmt.Errorf("unsupported RDMA validation Kusto ingestion %q", ingestion)
	}

	var filters strings.Builder
	fmt.Fprintf(&filters, "| where workspace_id == %s\n", kustoquery.QuoteString(scope.WorkspaceID))
	if scope.Cluster != "" {
		fmt.Fprintf(&filters, "| where cluster == %s\n", kustoquery.QuoteString(scope.Cluster))
	}
	if validationID != "" {
		fmt.Fprintf(&filters, "| where validation_id == %s\n", kustoquery.QuoteString(validationID))
	}
	cursorFilter := ""
	if !cursor.SortAt.IsZero() {
		cursorFilter = fmt.Sprintf(
			"| where wall_time < datetime(%s) or (wall_time == datetime(%s) and validation_id < %s)\n",
			cursor.SortAt.UTC().Format(time.RFC3339Nano),
			cursor.SortAt.UTC().Format(time.RFC3339Nano),
			kustoquery.QuoteString(cursor.ValidationID),
		)
	}
	return fmt.Sprintf(`let rdma = materialize(
%s
| extend rdma_tags=parse_json(tostring(tags))
| extend workspace_id=coalesce(source_workspace, tostring(rdma_tags[%s])),
         cluster=coalesce(source_cluster, tostring(rdma_tags[%s])),
         namespace=tostring(rdma_tags[%s]),
         validation_id=tostring(rdma_tags[%s]),
         validation_schema=tostring(rdma_tags[%s]),
         validation_kind=tostring(rdma_tags[%s]),
         lifecycle_state=tostring(rdma_tags[%s])
| where validation_kind == %s
%s| project exported_at=todatetime(exported_at), ['project']=tostring(['project']),
          experiment_id=tostring(experiment_id),
          run_group_id=tostring(run_group_id), run_id=tostring(run_id),
          metric_name=tostring(metric_name), step=tolong(step), wall_time=todatetime(wall_time),
          value=todouble(value), tags=tostring(tags), workspace_id, cluster, namespace,
          validation_id, validation_schema, validation_kind, lifecycle_state
);
let latest_validations = rdma
| where metric_name == %s
| summarize arg_max(step, *) by workspace_id, validation_id
%s| sort by wall_time desc, validation_id desc
| take %d;
rdma
| join kind=inner (latest_validations | project workspace_id, validation_id) on workspace_id, validation_id
| project-away workspace_id1, validation_id1
| summarize arg_max(step, *) by workspace_id, validation_id, metric_name
| order by wall_time desc, validation_id desc, metric_name asc
`,
		source,
		kustoquery.QuoteString(exptelemetry.TauWorkspaceTag),
		kustoquery.QuoteString(exptelemetry.TauClusterTag),
		kustoquery.QuoteString(exptelemetry.TauNamespaceTag),
		kustoquery.QuoteString(validationIDTag),
		kustoquery.QuoteString(validationSchemaTag),
		kustoquery.QuoteString(validationKindTag),
		kustoquery.QuoteString(lifecycleStateTag),
		kustoquery.QuoteString(corevalidation.Kind),
		filters.String(),
		kustoquery.QuoteString(exptelemetry.RunStatusMetricName),
		cursorFilter,
		limit,
	), nil
}

func aggregateMetricRows(rows []kustoquery.Row, scope Scope) ([]validationAggregate, error) {
	byID := make(map[string]*validationAggregate)
	for _, row := range rows {
		observed, err := parseMetricObservation(row)
		if err != nil {
			return nil, err
		}
		if observed.workspaceID != scope.WorkspaceID ||
			scope.Cluster != "" && observed.cluster != scope.Cluster {
			return nil, ErrScopeMismatch
		}
		aggregate := byID[observed.validationID]
		if aggregate == nil {
			aggregate = &validationAggregate{metrics: make(map[string]metricObservation)}
			byID[observed.validationID] = aggregate
		}
		if aggregate.identity.validationID == "" {
			aggregate.identity = observed
		} else if !sameMetricIdentity(aggregate.identity, observed) {
			return nil, fmt.Errorf("%w: validation %q has inconsistent metric identity", ErrArtifactIntegrity, observed.validationID)
		}
		if observed.metric == exptelemetry.RunStatusMetricName {
			if aggregate.lifecycle.validationID == "" ||
				observed.wallTime.After(aggregate.lifecycle.wallTime) ||
				observed.wallTime.Equal(aggregate.lifecycle.wallTime) && observed.step > aggregate.lifecycle.step {
				aggregate.lifecycle = observed
			}
			continue
		}
		existing, ok := aggregate.metrics[observed.metric]
		if !ok || observed.wallTime.After(existing.wallTime) ||
			observed.wallTime.Equal(existing.wallTime) && observed.step > existing.step {
			aggregate.metrics[observed.metric] = observed
		}
	}
	aggregates := make([]validationAggregate, 0, len(byID))
	for validationID, aggregate := range byID {
		if aggregate.lifecycle.validationID == "" {
			return nil, fmt.Errorf("RDMA validation %q has no lifecycle marker", validationID)
		}
		aggregates = append(aggregates, *aggregate)
	}
	return aggregates, nil
}

func sameMetricIdentity(left, right metricObservation) bool {
	return left.workspaceID == right.workspaceID &&
		left.cluster == right.cluster &&
		left.namespace == right.namespace &&
		left.validationID == right.validationID &&
		left.schema == right.schema &&
		left.kind == right.kind &&
		left.project == right.project &&
		left.experimentID == right.experimentID &&
		left.runGroupID == right.runGroupID &&
		left.runID == right.runID
}

func parseMetricObservation(row kustoquery.Row) (metricObservation, error) {
	var tags map[string]string
	if err := json.Unmarshal([]byte(row.Str("tags")), &tags); err != nil {
		return metricObservation{}, fmt.Errorf("decode RDMA validation metric tags: %w", err)
	}
	wallTime, err := time.Parse(time.RFC3339Nano, row.Str("wall_time"))
	if err != nil {
		return metricObservation{}, fmt.Errorf("parse RDMA validation wall_time: %w", err)
	}
	step, err := strconv.ParseInt(row.Str("step"), 10, 64)
	if err != nil {
		return metricObservation{}, fmt.Errorf("parse RDMA validation step: %w", err)
	}
	validationID := firstTag(tags, validationIDTag, "validation_id")
	if validationID == "" {
		validationID = row.Str("validation_id")
	}
	if validationID == "" {
		return metricObservation{}, fmt.Errorf("RDMA validation metric is missing validation ID")
	}
	value, hasValue := row.Num("value")
	if hasValue && (math.IsNaN(value) || math.IsInf(value, 0)) {
		return metricObservation{}, fmt.Errorf("RDMA validation metric %q has non-finite value", row.Str("metric_name"))
	}
	return metricObservation{
		workspaceID:  firstNonEmpty(row.Str("workspace_id"), tags[exptelemetry.TauWorkspaceTag]),
		cluster:      firstNonEmpty(row.Str("cluster"), tags[exptelemetry.TauClusterTag]),
		namespace:    firstNonEmpty(row.Str("namespace"), tags[exptelemetry.TauNamespaceTag]),
		validationID: validationID,
		schema:       firstTag(tags, validationSchemaTag, "schema"),
		kind:         firstTag(tags, validationKindTag, "kind"),
		lifecycle:    firstTag(tags, lifecycleStateTag, exptelemetry.RunStatusStateTag),
		status:       firstTag(tags, validationStatusTag, "status"),
		reason:       firstTag(tags, validationReasonTag, "reason"),
		backend:      firstTag(tags, "tau.rdma_validation.backend", "backend"),
		operation:    firstTag(tags, "tau.rdma_validation.operation", "operation"),
		project:      row.Str("project"), experimentID: row.Str("experiment_id"),
		runGroupID: row.Str("run_group_id"), runID: row.Str("run_id"),
		metric: row.Str("metric_name"), wallTime: wallTime.UTC(), step: step,
		value: value, hasValue: hasValue,
		artifactURI:  tags[corevalidation.MetricArtifactURITag],
		artifactHash: tags[corevalidation.MetricArtifactSHA256Tag],
	}, nil
}

func (aggregate validationAggregate) validation(now time.Time) Validation {
	lifecycle := aggregate.lifecycle
	validation := Validation{
		ValidationID: lifecycle.validationID, RunID: lifecycle.runID,
		State: StateUnknown, Freshness: FreshnessUnknown,
		ReasonCode:  "unverified_result",
		Reason:      "Validation evidence is incomplete or unverified.",
		WorkspaceID: lifecycle.workspaceID, Cluster: lifecycle.cluster, Namespace: lifecycle.namespace,
		Project: lifecycle.project, ExperimentID: lifecycle.experimentID, RunGroupID: lifecycle.runGroupID,
	}
	attempt, validAttempt := attemptFromStep(lifecycle.step, lifecycle.lifecycle)
	if validAttempt {
		validation.RunAttempt = &attempt
	}
	if lifecycle.schema != corevalidation.SchemaVersion || lifecycle.kind != corevalidation.Kind {
		validation.ReasonCode = "unsupported_schema"
		validation.Reason = "The validation schema or kind is unsupported."
		return validation
	}
	switch lifecycle.lifecycle {
	case corevalidation.RunStatePending, corevalidation.RunStateRunning:
		validation.State = StateRunning
		validation.Freshness = FreshnessNotApplicable
		if lifecycle.lifecycle == corevalidation.RunStatePending {
			validation.CreatedAt = formatTime(lifecycle.wallTime)
			validation.ReasonCode = "pending"
			validation.Reason = "The validation is pending admission; no final result is available."
		} else {
			validation.StartedAt = formatTime(lifecycle.wallTime)
			validation.ReasonCode = "running"
			validation.Reason = "The validation is running; no final result is available."
		}
		validation.AgeSeconds = ageSeconds(now, lifecycle.wallTime)
		return validation
	case corevalidation.RunStateSucceeded, corevalidation.RunStateFailed:
		if !validAttempt {
			validation.ReasonCode = "malformed_lifecycle"
			validation.Reason = "The validation attempt or lifecycle step is malformed."
			return validation
		}
		validation.CompletedAt = formatTime(lifecycle.wallTime)
	default:
		validation.ReasonCode = "unknown_lifecycle"
		validation.Reason = "The validation lifecycle state is unknown."
		return validation
	}

	status, ok := aggregate.metrics[corevalidation.MetricStatus]
	if !ok || !status.hasValue || status.wallTime.IsZero() ||
		status.artifactURI == "" || status.artifactHash == "" {
		return validation
	}
	historical, consistent := historicalStatus(status.status, status.value)
	if !consistent || historical == "" || !knownReason(status.reason) ||
		!terminalLifecycleConsistent(lifecycle.lifecycle, historical) ||
		!terminalMarkerConsistent(lifecycle, historical) ||
		lifecycle.artifactURI != status.artifactURI || lifecycle.artifactHash != status.artifactHash ||
		!consistentTerminalMetrics(aggregate.metrics, lifecycle, status) ||
		!consistentArtifactLinks(aggregate.metrics, status.artifactURI, status.artifactHash) ||
		historical == "pass" && !completePassProjection(aggregate.metrics) {
		validation.ReasonCode = "malformed_status"
		validation.Reason = "The terminal validation projection is incomplete, malformed, or inconsistent."
		return validation
	}
	validation.HistoricalStatus = &historical
	validation.ObservedAt = formatTime(status.wallTime)
	validUntil := status.wallTime.Add(time.Duration(corevalidation.DefaultStaleAfterSeconds) * time.Second)
	validation.ValidUntil = formatTime(validUntil)
	validation.StaleAfterSeconds = int64Pointer(corevalidation.DefaultStaleAfterSeconds)
	validation.State, validation.Freshness, validation.AgeSeconds = DeriveState(
		now, status.wallTime, validUntil, historical, false, true,
	)
	validation.ReasonCode = firstNonEmpty(status.reason, lifecycle.reason)
	validation.Reason = reasonText(corevalidation.ReasonCode(validation.ReasonCode))
	validation.Transport = &Transport{
		Backend:                status.backend,
		SocketFallbackDetected: boolMetric(aggregate.metrics[corevalidation.MetricSocketFallbackObserved]),
		IBPositiveEvidence:     boolMetric(aggregate.metrics[corevalidation.MetricNetIBObserved]),
	}
	validation.Summary = &BandwidthSummary{
		AlgBWGbps: scalarDistribution(aggregate.metrics[corevalidation.MetricAlgBWGbps]),
		BusBWGbps: scalarDistribution(aggregate.metrics[corevalidation.MetricBusBWGbps]),
	}
	if validation.Summary.AlgBWGbps == nil && validation.Summary.BusBWGbps == nil {
		validation.Summary = nil
	}
	if duration, ok := scalarValue(aggregate.metrics[corevalidation.MetricDurationSeconds]); ok {
		validation.DurationSeconds = &duration
	}
	validation.Cleanup = &Cleanup{Status: cleanupStatus(aggregate.metrics[corevalidation.MetricCleanupComplete])}
	validation.ArtifactVerification = &ArtifactVerification{
		State: "verified", URI: status.artifactURI, SHA256: status.artifactHash,
	}
	return validation
}

func attemptFromStep(step int64, lifecycle string) (int, bool) {
	if step < 0 {
		return 0, false
	}
	var offset int64
	switch lifecycle {
	case corevalidation.RunStatePending:
		offset = 0
	case corevalidation.RunStateRunning:
		offset = 1
	case corevalidation.RunStateSucceeded, corevalidation.RunStateFailed:
		offset = 2
	default:
		return 0, false
	}
	if step%3 != offset {
		return 0, false
	}
	attempt := step/3 + 1
	if attempt > int64(^uint(0)>>1) {
		return 0, false
	}
	return int(attempt), true
}

func historicalStatus(tag string, value float64) (string, bool) {
	switch tag {
	case "pass":
		return "pass", value == 1
	case "fail":
		return "fail", value == -1
	case "unknown":
		return "unknown", value == 0
	default:
		switch value {
		case 1:
			return "pass", true
		case -1:
			return "fail", true
		case 0:
			return "unknown", true
		default:
			return "", false
		}
	}
}

func boolMetric(metric metricObservation) *bool {
	if !metric.hasValue {
		return nil
	}
	value := metric.value == 1
	if metric.value != 0 && metric.value != 1 {
		return nil
	}
	return &value
}

func scalarDistribution(metric metricObservation) *Distribution {
	value, ok := scalarValue(metric)
	if !ok {
		return nil
	}
	return &Distribution{Min: &value}
}

func scalarValue(metric metricObservation) (float64, bool) {
	return metric.value, metric.hasValue && !math.IsNaN(metric.value) && !math.IsInf(metric.value, 0)
}

func cleanupStatus(metric metricObservation) string {
	if !metric.hasValue {
		return "unknown"
	}
	if metric.value == 1 {
		return "complete"
	}
	if metric.value == 0 {
		return "incomplete"
	}
	return "unknown"
}

func firstTag(tags map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(tags[key]); value != "" {
			return value
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
