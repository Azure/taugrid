// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package faultevents

import (
	"testing"
	"time"

	"github.com/Azure/taugrid/portal/internal/portal/nodes"
)

func TestNormalizeUnhealthyNodeCondition(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	raw := nodes.Condition{
		Type:               "XIDError79",
		Category:           "gpu",
		Status:             "True",
		Reason:             "XIDError79",
		Message:            "GPU XID 79 observed",
		LastHeartbeatTime:  "2026-09-23T11:55:00Z",
		LastTransitionTime: "2026-09-23T11:54:00Z",
	}

	got := Normalize([]nodes.Node{{Name: "gpu-node-1", Conditions: []nodes.Condition{raw}}}, now)
	if len(got) != 1 {
		t.Fatalf("events = %d, want 1", len(got))
	}
	event := got[0]
	if event.Node != "gpu-node-1" || event.Scope != ScopeNode ||
		event.Category != "gpu" || event.CheckType != "XIDError79" {
		t.Fatalf("event identity = %+v", event)
	}
	if event.HealthState != HealthStateUnhealthy || event.Status != "True" ||
		event.EvidenceStatus != EvidenceStatusFresh {
		t.Fatalf("event health = %+v", event)
	}
	if event.Reason != raw.Reason || event.Message != raw.Message || event.RawCondition != raw {
		t.Fatalf("raw evidence was not preserved: %+v", event)
	}
	if event.ObservedAt == nil || event.ObservedAt.Format(time.RFC3339) != "2026-09-23T11:55:00Z" ||
		event.TransitionAt == nil || event.TransitionAt.Format(time.RFC3339) != "2026-09-23T11:54:00Z" {
		t.Fatalf("event timestamps = observed %v transition %v", event.ObservedAt, event.TransitionAt)
	}
}

func TestNormalizeHealthyRecoveredCondition(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	event := Normalize([]nodes.Node{{
		Name: "gpu-node-1",
		Conditions: []nodes.Condition{{
			Type:               "XIDError79",
			Category:           "gpu",
			Status:             "False",
			Reason:             "XIDError79Recovered",
			LastHeartbeatTime:  "2026-09-23T11:59:00Z",
			LastTransitionTime: "2026-09-23T11:58:00Z",
		}},
	}}, now)[0]

	if event.HealthState != HealthStateHealthy || event.Status != "False" ||
		event.EvidenceStatus != EvidenceStatusFresh {
		t.Fatalf("event = %+v, want fresh healthy evidence", event)
	}
}

func TestNormalizeFailsClosedOnUncertainEvidence(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		condition nodes.Condition
		want      EvidenceStatus
	}{
		{
			name: "stale",
			condition: nodes.Condition{
				Type: "IBLinkDown", Category: "infiniband", Status: "False",
				LastHeartbeatTime: "2026-09-23T11:44:59Z",
			},
			want: EvidenceStatusStale,
		},
		{
			name: "malformed heartbeat",
			condition: nodes.Condition{
				Type: "GPUMissing", Category: "gpu", Status: "True",
				LastHeartbeatTime: "not-a-time",
			},
			want: EvidenceStatusMalformed,
		},
		{
			name: "malformed status",
			condition: nodes.Condition{
				Type: "NvidiaSmiProblem", Category: "gpu", Status: "Broken",
				LastHeartbeatTime: "2026-09-23T11:59:00Z",
			},
			want: EvidenceStatusMalformed,
		},
		{
			name: "unknown status",
			condition: nodes.Condition{
				Type: "IBSymbolError", Category: "infiniband", Status: "Unknown",
				LastHeartbeatTime: "2026-09-23T11:59:00Z",
			},
			want: EvidenceStatusFresh,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event := Normalize([]nodes.Node{{Name: "gpu-node-1", Conditions: []nodes.Condition{tt.condition}}}, now)[0]
			if event.HealthState != HealthStateUnknown || event.EvidenceStatus != tt.want {
				t.Fatalf("event = %+v, want unknown/%s", event, tt.want)
			}
			if event.RawCondition != tt.condition {
				t.Fatalf("raw condition = %+v, want %+v", event.RawCondition, tt.condition)
			}
		})
	}
}

func TestNormalizeDuplicateEvidenceIsAmbiguousUnlessItIsAFreshFault(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	events := Normalize([]nodes.Node{{
		Name: "gpu-node-1",
		Conditions: []nodes.Condition{
			{Type: "GPUMissing", Category: "gpu", Status: "False", LastHeartbeatTime: "2026-09-23T11:59:00Z"},
			{Type: "GPUMissing", Category: "gpu", Status: "True", LastHeartbeatTime: "2026-09-23T11:59:30Z"},
		},
	}}, now)

	if len(events) != 2 {
		t.Fatalf("events = %d, want 2", len(events))
	}
	byStatus := map[string]Event{}
	for _, event := range events {
		byStatus[event.Status] = event
	}
	if event := byStatus["False"]; event.HealthState != HealthStateUnknown || event.EvidenceStatus != EvidenceStatusDuplicate {
		t.Fatalf("healthy duplicate = %+v, want unknown duplicate", event)
	}
	if event := byStatus["True"]; event.HealthState != HealthStateUnhealthy || event.EvidenceStatus != EvidenceStatusDuplicate {
		t.Fatalf("fault duplicate = %+v, want unhealthy duplicate", event)
	}
}

func TestNormalizeDedupKeyTracksStateTransitionNotPollingNoise(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	base := nodes.Condition{
		Type:               "IBLinkDown",
		Category:           "infiniband",
		Status:             "True",
		Reason:             "IBLinkDownObserved",
		Message:            "link down",
		LastHeartbeatTime:  "2026-09-23T11:58:00Z",
		LastTransitionTime: "2026-09-23T11:50:00Z",
	}
	pollUpdate := base
	pollUpdate.Reason = "IBLinkStillDown"
	pollUpdate.Message = "updated details"
	pollUpdate.LastHeartbeatTime = "2026-09-23T11:59:00Z"
	recovered := pollUpdate
	recovered.Status = "False"
	recovered.LastTransitionTime = "2026-09-23T11:59:30Z"

	first := Normalize([]nodes.Node{{Name: "gpu-node-1", Conditions: []nodes.Condition{base}}}, now)[0]
	second := Normalize([]nodes.Node{{Name: "gpu-node-1", Conditions: []nodes.Condition{pollUpdate}}}, now)[0]
	third := Normalize([]nodes.Node{{Name: "gpu-node-1", Conditions: []nodes.Condition{recovered}}}, now)[0]

	if first.DedupKey == "" || first.DedupKey != second.DedupKey {
		t.Fatalf("polling update changed dedup key: %q != %q", first.DedupKey, second.DedupKey)
	}
	const expected = "node-condition:v1:7f07d62d9efc76efb25f994cbc2d76b110c2de7f0a317c7358ded89c30d5110d"
	if first.DedupKey != expected {
		t.Fatalf("dedup key = %q, want stable identity %q", first.DedupKey, expected)
	}
	if third.DedupKey == first.DedupKey {
		t.Fatalf("state transition retained dedup key %q", third.DedupKey)
	}
}

func TestNormalizeSortsEventsDeterministically(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	fresh := "2026-09-23T11:59:00Z"
	events := Normalize([]nodes.Node{
		{Name: "node-b", Conditions: []nodes.Condition{{Type: "XIDError79", Category: "gpu", Status: "False", LastHeartbeatTime: fresh}}},
		{Name: "node-a", Conditions: []nodes.Condition{
			{Type: "IBLinkDown", Category: "infiniband", Status: "False", LastHeartbeatTime: fresh},
			{Type: "GPUMissing", Category: "gpu", Status: "False", LastHeartbeatTime: fresh},
		}},
	}, now)

	if len(events) != 3 ||
		events[0].Node != "node-a" || events[0].Category != "gpu" ||
		events[1].Node != "node-a" || events[1].Category != "infiniband" ||
		events[2].Node != "node-b" {
		t.Fatalf("events are not deterministically sorted: %+v", events)
	}
}
