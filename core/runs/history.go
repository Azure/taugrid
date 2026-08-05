package runs

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/expkusto"
	"github.com/Azure/taugrid/core/kustoquery"
)

const (
	historyStateAvailable   = "available"
	historyStateLiveOnly    = "live-only"
	historyStateUnavailable = "history-unavailable"
)

// HistoryScope is the server- or CLI-resolved durable-history boundary. Callers
// must not populate it from untrusted browser query parameters.
type HistoryScope struct {
	Table       string
	Cluster     string
	Namespace   string
	LocalQueue  string
	WorkspaceID string
	Limit       int
}

// HistoryReader lists durable lifecycle rows within one already-resolved scope.
type HistoryReader interface {
	ListHistory(ctx context.Context, scope HistoryScope) ([]Run, error)
}

// HistoryQueryBuilder builds the KQL for one scoped history request.
type HistoryQueryBuilder func(HistoryScope) (string, error)

// KustoHistoryReader adapts the portal's existing generic Kusto client to
// durable run lifecycle rows. The injected builder keeps this package free of
// lifecycle-schema ownership while making the scope passed to Kusto explicit.
type KustoHistoryReader struct {
	Querier      kustoquery.Querier
	QueryBuilder HistoryQueryBuilder
}

// NewKustoHistoryReader returns the durable lifecycle adapter used by both the
// Portal and `tau run list`. It deliberately uses the shared Kusto query seam,
// not a second authentication client.
func NewKustoHistoryReader(querier kustoquery.Querier) KustoHistoryReader {
	return KustoHistoryReader{
		Querier: querier,
		QueryBuilder: func(scope HistoryScope) (string, error) {
			return expkusto.BuildRunHistoryQuery(expkusto.RunHistoryQueryOptions{
				Table:       scope.Table,
				Cluster:     scope.Cluster,
				Namespace:   scope.Namespace,
				LocalQueue:  scope.LocalQueue,
				WorkspaceID: scope.WorkspaceID,
				Limit:       scope.Limit,
			})
		},
	}
}

func (r KustoHistoryReader) ListHistory(ctx context.Context, scope HistoryScope) ([]Run, error) {
	if r.Querier == nil {
		return nil, fmt.Errorf("durable history querier is not configured")
	}
	if r.QueryBuilder == nil {
		return nil, fmt.Errorf("durable history query builder is not configured")
	}
	kql, err := r.QueryBuilder(scope)
	if err != nil {
		return nil, fmt.Errorf("build durable history query: %w", err)
	}
	rows, err := r.Querier.Query(ctx, kql)
	if err != nil {
		return nil, fmt.Errorf("query durable history: %w", err)
	}
	out := make([]Run, 0, len(rows))
	for _, row := range rows {
		run := runFromHistoryRow(row)
		if run.Name == "" && run.RunID == "" && run.DurableID == "" {
			continue
		}
		out = append(out, run)
	}
	return out, nil
}

func runFromHistoryRow(row kustoquery.Row) Run {
	created := firstHistoryTime(row, "created_time", "submit_time", "completion_time", "observed_at")
	return Run{
		Name:               firstRowValue(row, "owning_resource_name", "name"),
		Kind:               firstRowValue(row, "owning_resource_kind", "kind"),
		Status:             normalizeHistoryStatus(firstRowValue(row, "effective_state", "state", "status")),
		Created:            created,
		Age:                FormatAge(time.Now(), created),
		RunID:              firstRowValue(row, "run_id", "runId"),
		Queue:              firstRowValue(row, "local_queue", "localQueue", "queue"),
		Namespace:          firstRowValue(row, "namespace"),
		Cluster:            firstRowValue(row, "cluster"),
		ResourceUID:        firstRowValue(row, "resource_uid", "resourceUid"),
		DurableID:          firstRowValue(row, "durable_id", "durableId"),
		ExperimentTracking: firstNonEmpty(firstRowValue(row, "experiment_tracking", "experimentTracking"), experimentTrackingUntracked),
	}
}

func normalizeHistoryStatus(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "submitted", "queued", "admitted", "pending":
		return "Pending"
	case "running":
		return "Running"
	case "succeeded", "complete", "completed":
		return "Succeeded"
	case "failed":
		return "Failed"
	case "cancelled", "canceled":
		return "Cancelled"
	case "stale":
		return "Stale"
	default:
		return state
	}
}

func firstRowValue(row kustoquery.Row, columns ...string) string {
	for _, column := range columns {
		if value := strings.TrimSpace(row.Str(column)); value != "" {
			return value
		}
	}
	return ""
}

func firstHistoryTime(row kustoquery.Row, columns ...string) time.Time {
	for _, column := range columns {
		value := strings.TrimSpace(row.Str(column))
		if value == "" {
			continue
		}
		if parsed, err := time.Parse(time.RFC3339, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

func mergeHistory(live, durable []Run) []Run {
	index := make(map[string]int, len(live)*3)
	for i := range live {
		for _, key := range mergeKeys(live[i]) {
			index[key] = i
		}
	}
	merged := append([]Run(nil), live...)
	for _, historical := range durable {
		matched := -1
		matchedKey := ""
		for _, key := range mergeKeys(historical) {
			if i, ok := index[key]; ok {
				matched = i
				matchedKey = key
				break
			}
		}
		if matched >= 0 {
			// The Kubernetes object is authoritative while it exists. Preserve
			// identifiers and exact tracking evidence that may only be present in
			// the durable projection.
			if merged[matched].DurableID == "" {
				merged[matched].DurableID = historical.DurableID
			}
			if historical.ExperimentTracking == experimentTrackingTracked && !strings.HasPrefix(matchedKey, "run:") {
				merged[matched].ExperimentTracking = experimentTrackingTracked
			}
			continue
		}
		historical.Age = FormatAge(time.Now(), historical.Created)
		merged = append(merged, historical)
	}
	sortRuns(merged)
	return merged
}

func mergeKeys(run Run) []string {
	keys := make([]string, 0, 3)
	if run.DurableID != "" {
		keys = append(keys, "durable:"+run.DurableID)
	}
	if run.Cluster != "" && run.Namespace != "" && run.ResourceUID != "" {
		keys = append(keys, "resource:"+run.Cluster+"/"+run.Namespace+"/"+run.ResourceUID)
	}
	if run.Cluster != "" && run.Namespace != "" && run.RunID != "" {
		keys = append(keys, "run:"+run.Cluster+"/"+run.Namespace+"/"+run.RunID)
	}
	return keys
}
