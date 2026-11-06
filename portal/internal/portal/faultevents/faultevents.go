// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package faultevents normalizes current GPU and InfiniBand Node conditions
// into structured, node-scoped fault events for internal Portal consumers.
package faultevents

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Azure/taugrid/portal/internal/portal/nodes"
)

const (
	conditionFreshness = 15 * time.Minute
	futureClockSkew    = time.Minute
)

// HealthState is the normalized operational state of one condition.
type HealthState string

const (
	HealthStateHealthy   HealthState = "healthy"
	HealthStateUnhealthy HealthState = "unhealthy"
	HealthStateUnknown   HealthState = "unknown"
)

// EvidenceStatus describes whether the condition can support a current verdict.
type EvidenceStatus string

const (
	EvidenceStatusFresh     EvidenceStatus = "fresh"
	EvidenceStatusStale     EvidenceStatus = "stale"
	EvidenceStatusFuture    EvidenceStatus = "future"
	EvidenceStatusMalformed EvidenceStatus = "malformed"
	EvidenceStatusDuplicate EvidenceStatus = "duplicate"
)

// Scope identifies the resource level represented by an event.
type Scope string

const ScopeNode Scope = "node"

// Event is one normalized view of an allowlisted operational Node condition.
// It deliberately remains node-scoped because Node conditions carry no
// trustworthy per-GPU identity.
type Event struct {
	DedupKey       string          `json:"dedupKey"`
	Node           string          `json:"node"`
	Scope          Scope           `json:"scope"`
	Category       string          `json:"category"`
	CheckType      string          `json:"checkType"`
	HealthState    HealthState     `json:"healthState"`
	Status         string          `json:"status"`
	EvidenceStatus EvidenceStatus  `json:"evidenceStatus"`
	Reason         string          `json:"reason,omitempty"`
	Message        string          `json:"message,omitempty"`
	ObservedAt     *time.Time      `json:"observedAt,omitempty"`
	TransitionAt   *time.Time      `json:"transitionAt,omitempty"`
	RawCondition   nodes.Condition `json:"rawCondition"`
}

// Normalize converts the allowlisted conditions already parsed by nodes.Board
// into deterministic events. Invalid, stale, future-dated, or ambiguous
// evidence is retained but never presented as healthy.
func Normalize(nodeRows []nodes.Node, evaluatedAt time.Time) []Event {
	counts := make(map[string]int)
	for _, node := range nodeRows {
		for _, condition := range node.Conditions {
			counts[conditionIdentity(node.Name, condition.Category, condition.Type)]++
		}
	}

	events := make([]Event, 0)
	for _, node := range nodeRows {
		for _, condition := range node.Conditions {
			observedAt := parseTimestamp(condition.LastHeartbeatTime)
			transitionAt := parseTimestamp(condition.LastTransitionTime)
			duplicate := counts[conditionIdentity(node.Name, condition.Category, condition.Type)] != 1
			healthState, evidenceStatus := evaluate(node.Name, condition, observedAt, duplicate, evaluatedAt)
			events = append(events, Event{
				DedupKey:       dedupKey(node.Name, condition),
				Node:           node.Name,
				Scope:          ScopeNode,
				Category:       condition.Category,
				CheckType:      condition.Type,
				HealthState:    healthState,
				Status:         condition.Status,
				EvidenceStatus: evidenceStatus,
				Reason:         condition.Reason,
				Message:        condition.Message,
				ObservedAt:     observedAt,
				TransitionAt:   transitionAt,
				RawCondition:   condition,
			})
		}
	}
	sort.Slice(events, func(i, j int) bool {
		left, right := events[i], events[j]
		if left.Node != right.Node {
			return left.Node < right.Node
		}
		if left.Category != right.Category {
			return left.Category < right.Category
		}
		if left.CheckType != right.CheckType {
			return left.CheckType < right.CheckType
		}
		return left.DedupKey < right.DedupKey
	})
	return events
}

func evaluate(
	nodeName string,
	condition nodes.Condition,
	observedAt *time.Time,
	duplicate bool,
	evaluatedAt time.Time,
) (HealthState, EvidenceStatus) {
	if nodeName == "" || condition.Category == "" || condition.Type == "" || observedAt == nil {
		return HealthStateUnknown, EvidenceStatusMalformed
	}
	now := evaluatedAt.UTC()
	if observedAt.After(now.Add(futureClockSkew)) {
		return HealthStateUnknown, EvidenceStatusFuture
	}
	if now.Sub(*observedAt) > conditionFreshness {
		return HealthStateUnknown, EvidenceStatusStale
	}
	if duplicate {
		if condition.Status == "True" {
			return HealthStateUnhealthy, EvidenceStatusDuplicate
		}
		return HealthStateUnknown, EvidenceStatusDuplicate
	}
	switch condition.Status {
	case "True":
		return HealthStateUnhealthy, EvidenceStatusFresh
	case "False":
		return HealthStateHealthy, EvidenceStatusFresh
	case "Unknown":
		return HealthStateUnknown, EvidenceStatusFresh
	default:
		return HealthStateUnknown, EvidenceStatusMalformed
	}
}

func parseTimestamp(value string) *time.Time {
	if value == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil
	}
	parsed = parsed.UTC()
	return &parsed
}

func dedupKey(nodeName string, condition nodes.Condition) string {
	transition := condition.LastTransitionTime
	if parsed := parseTimestamp(transition); parsed != nil {
		transition = parsed.Format(time.RFC3339Nano)
	}
	var canonical strings.Builder
	for _, part := range []string{
		nodeName,
		string(ScopeNode),
		condition.Category,
		condition.Type,
		condition.Status,
		transition,
	} {
		canonical.WriteString(strconv.Itoa(len(part)))
		canonical.WriteByte(':')
		canonical.WriteString(part)
	}
	sum := sha256.Sum256([]byte(canonical.String()))
	return "node-condition:v1:" + hex.EncodeToString(sum[:])
}

func conditionIdentity(nodeName, category, conditionType string) string {
	return nodeName + "\x00" + category + "\x00" + conditionType
}
