// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/portal/internal/expstore"
	"github.com/Azure/taugrid/portal/internal/portal/faultevents"
	"github.com/Azure/taugrid/portal/internal/portal/nodes"
)

const experimentFaultsPathPrefix = "/api/portal/experiments/"

type experimentFaultCoverage struct {
	Correlation string   `json:"correlation"`
	Allocation  string   `json:"allocation"`
	TimeBounds  string   `json:"timeBounds"`
	Evidence    string   `json:"evidence"`
	Reasons     []string `json:"reasons,omitempty"`
}

type experimentFaultProvenance struct {
	Allocation string `json:"allocation"`
	Evidence   string `json:"evidence"`
	Limitation string `json:"limitation"`
}

type experimentTimeBounds struct {
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Active      bool       `json:"active"`
}

type experimentFaultResponse struct {
	ExperimentID   string                    `json:"experimentId"`
	GeneratedAt    time.Time                 `json:"generatedAt"`
	AllocatedNodes []string                  `json:"allocatedNodes"`
	MissingNodes   []string                  `json:"missingNodes,omitempty"`
	TimeBounds     experimentTimeBounds      `json:"timeBounds"`
	Coverage       experimentFaultCoverage   `json:"coverage"`
	Provenance     experimentFaultProvenance `json:"provenance"`
	Events         []faultevents.Event       `json:"events"`
}

func (s *Server) handleExperimentFaultEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, experimentFaultsPathPrefix)
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "fault-events" {
		http.NotFound(w, r)
		return
	}
	experimentID, err := url.PathUnescape(parts[0])
	if err != nil || strings.TrimSpace(experimentID) == "" || strings.Contains(experimentID, "/") {
		writeJSONError(w, http.StatusBadRequest, "invalid experiment ID")
		return
	}
	scope, ok := s.localWorkspaceScope(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	response := unavailableExperimentFaultResponse(experimentID, now)
	if s.stellarSource != "local" {
		response.Coverage.Reasons = []string{
			"experiment run allocation is unavailable from the configured non-local experiment source",
		}
		writeScopedJSON(w, http.StatusOK, response, scope, "unavailable")
		return
	}

	store, err := expstore.Open(r.Context(), s.stellar.StoreRoot())
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, "experiment allocation evidence could not be read")
		return
	}
	defer store.Close()
	runs, truncated, err := store.ExperimentFaultEvidence(r.Context(), experimentID, scope.WorkspaceID)
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, "experiment allocation evidence could not be queried")
		return
	}
	if len(runs) == 0 {
		writeScopedError(w, http.StatusNotFound, scope, "experiment not found")
		return
	}
	response = correlateExperimentFaults(experimentID, scope, runs, truncated, now)
	if len(response.AllocatedNodes) == 0 || s.nodes.Reader == nil {
		if s.nodes.Reader == nil {
			response.Coverage.Evidence = "unavailable"
			response.Coverage.Correlation = "unavailable"
			response.Coverage.Reasons = append(response.Coverage.Reasons,
				"current Node-condition evidence is unavailable because the portal has no Kubernetes reader")
		}
		writeScopedJSON(w, http.StatusOK, response, scope, response.Coverage.Correlation)
		return
	}

	snapshot, err := nodes.Board(r.Context(), s.nodes.Reader, nodes.Options{})
	if err != nil {
		writeScopedError(w, http.StatusBadGateway, scope, "current Node-condition evidence could not be read")
		return
	}
	allocated := make(map[string]struct{}, len(response.AllocatedNodes))
	for _, nodeName := range response.AllocatedNodes {
		allocated[nodeName] = struct{}{}
	}
	currentNodes := make(map[string]struct{}, len(snapshot.Nodes))
	filteredNodes := make([]nodes.Node, 0, len(response.AllocatedNodes))
	for _, node := range snapshot.Nodes {
		if _, ok := allocated[node.Name]; !ok {
			continue
		}
		currentNodes[node.Name] = struct{}{}
		filteredNodes = append(filteredNodes, node)
	}
	for _, nodeName := range response.AllocatedNodes {
		if _, ok := currentNodes[nodeName]; !ok {
			response.MissingNodes = append(response.MissingNodes, nodeName)
		}
	}
	response.Events = faultevents.Normalize(filteredNodes, now)
	response.Coverage.Evidence = currentEvidenceCoverage(response.Events)
	response.Coverage.Correlation = response.Coverage.Evidence
	eventNodes := make(map[string]struct{}, len(response.Events))
	for _, event := range response.Events {
		eventNodes[event.Node] = struct{}{}
	}
	nodesWithoutEvidence := len(currentNodes) - len(eventNodes)
	if response.Coverage.Allocation == "partial" || len(response.MissingNodes) > 0 || nodesWithoutEvidence > 0 {
		response.Coverage.Correlation = "partial"
	}
	if len(response.MissingNodes) > 0 {
		response.Coverage.Reasons = append(response.Coverage.Reasons,
			"some historically allocated nodes are absent from the current cluster snapshot")
	}
	if nodesWithoutEvidence > 0 {
		response.Coverage.Reasons = append(response.Coverage.Reasons,
			"some allocated nodes have no allowlisted GPU or InfiniBand Node-condition evidence")
	}
	if !response.TimeBounds.Active {
		response.Coverage.Reasons = append(response.Coverage.Reasons,
			"completed experiments have no historical Node-condition log; returned events describe current node state only")
	}
	writeScopedJSON(w, http.StatusOK, response, scope, response.Coverage.Correlation)
}

func unavailableExperimentFaultResponse(experimentID string, now time.Time) experimentFaultResponse {
	return experimentFaultResponse{
		ExperimentID:   experimentID,
		GeneratedAt:    now,
		AllocatedNodes: []string{},
		Coverage: experimentFaultCoverage{
			Correlation: "unavailable",
			Allocation:  "unavailable",
			TimeBounds:  "unknown",
			Evidence:    "unavailable",
		},
		Provenance: experimentFaultProvenance{
			Allocation: "expstore run_context.node_names",
			Evidence:   "current Kubernetes Node status.conditions",
			Limitation: "Node conditions are a current-state snapshot, not an event log",
		},
		Events: []faultevents.Event{},
	}
}

func correlateExperimentFaults(
	experimentID string,
	scope WorkspaceScope,
	runs []expstore.ExperimentRunEvidence,
	truncated bool,
	now time.Time,
) experimentFaultResponse {
	response := unavailableExperimentFaultResponse(experimentID, now)
	response.TimeBounds, response.Coverage.TimeBounds = experimentBounds(runs)
	nodeSet := map[string]struct{}{}
	missingAllocation := truncated
	for _, run := range runs {
		if run.Context == nil {
			missingAllocation = true
			continue
		}
		if scope.Cluster != "" && run.Context.Cluster != "" && run.Context.Cluster != scope.Cluster {
			missingAllocation = true
			continue
		}
		if scope.Managed && scope.Namespace != "" && run.Context.Namespace != "" && run.Context.Namespace != scope.Namespace {
			missingAllocation = true
			continue
		}
		names := parseNodeNames(run.Context.NodeNames)
		if len(names) == 0 {
			missingAllocation = true
			continue
		}
		for _, name := range names {
			nodeSet[name] = struct{}{}
		}
	}
	response.AllocatedNodes = make([]string, 0, len(nodeSet))
	for name := range nodeSet {
		response.AllocatedNodes = append(response.AllocatedNodes, name)
	}
	sort.Strings(response.AllocatedNodes)
	switch {
	case len(response.AllocatedNodes) == 0:
		response.Coverage.Allocation = "unavailable"
		response.Coverage.Reasons = append(response.Coverage.Reasons,
			"no workspace-scoped run recorded a node allocation on this cluster")
	case missingAllocation:
		response.Coverage.Allocation = "partial"
		response.Coverage.Reasons = append(response.Coverage.Reasons,
			"one or more experiment runs lack an in-scope recorded node allocation")
	default:
		response.Coverage.Allocation = "exact"
	}
	return response
}

func experimentBounds(runs []expstore.ExperimentRunEvidence) (experimentTimeBounds, string) {
	var bounds experimentTimeBounds
	completeStarts := true
	completeEnds := true
	for _, run := range runs {
		start := parseEvidenceTime(firstNonEmpty(run.StartedAt, run.CreatedAt))
		if start == nil {
			completeStarts = false
		} else if bounds.StartedAt == nil || start.Before(*bounds.StartedAt) {
			bounds.StartedAt = start
		}
		if strings.TrimSpace(run.CompletedAt) == "" {
			bounds.Active = true
			completeEnds = false
			continue
		}
		end := parseEvidenceTime(run.CompletedAt)
		if end == nil {
			completeEnds = false
		} else if bounds.CompletedAt == nil || end.After(*bounds.CompletedAt) {
			bounds.CompletedAt = end
		}
	}
	switch {
	case bounds.StartedAt == nil:
		return bounds, "unknown"
	case completeStarts && (bounds.Active || completeEnds):
		return bounds, "exact"
	default:
		return bounds, "partial"
	}
}

func parseEvidenceTime(value string) *time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func parseNodeNames(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	var jsonNames []string
	if strings.HasPrefix(value, "[") && json.Unmarshal([]byte(value), &jsonNames) == nil {
		return compactNodeNames(jsonNames)
	}
	return compactNodeNames(strings.Split(value, ","))
}

func compactNodeNames(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func currentEvidenceCoverage(events []faultevents.Event) string {
	if len(events) == 0 {
		return "unknown"
	}
	fresh := false
	for _, event := range events {
		switch event.EvidenceStatus {
		case faultevents.EvidenceStatusFresh:
			fresh = true
		case faultevents.EvidenceStatusStale:
		default:
			return "unknown"
		}
	}
	if fresh {
		return "current-only"
	}
	return "stale"
}
