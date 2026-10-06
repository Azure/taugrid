// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package queuequota

import (
	"strings"
	"testing"
)

func TestBuildReportsWorkspaceAndTeamQuotaLevels(t *testing.T) {
	report, err := Build(Input{
		Workspace:    "vision",
		Namespace:    "vision-runs",
		LocalQueue:   "jobqueue",
		ClusterQueue: "tau-ws-vision",
		ClusterQueueRaw: []byte(`{
			"metadata":{"name":"tau-ws-vision","labels":{"tau.azure.com/team":"research"}},
			"spec":{
				"cohortName":"tau-team-research",
				"resourceGroups":[{"coveredResources":["nvidia.com/gpu"],"flavors":[{
					"name":"h200","resources":[{
						"name":"nvidia.com/gpu","nominalQuota":"8","borrowingLimit":"4","lendingLimit":"2"
					}]
				}]}]
			},
			"status":{
				"flavorsReservation":[{"name":"h200","resources":[{"name":"nvidia.com/gpu","total":"6"}]}],
				"flavorsUsage":[{"name":"h200","resources":[{"name":"nvidia.com/gpu","total":"5"}]}]
			}
		}`),
		CohortRaw: []byte(`{
			"metadata":{"name":"tau-team-research","labels":{"tau.azure.com/team":"research"}},
			"spec":{"resourceGroups":[{"coveredResources":["nvidia.com/gpu"],"flavors":[{
				"name":"h200","resources":[{"name":"nvidia.com/gpu","nominalQuota":"20"}]
			}]}]}
		}`),
		FlavorsRaw: map[string][]byte{
			"h200": []byte(`{"spec":{"nodeLabels":{"tau.azure.com/gpu-class":"h200-141gb"}}}`),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Schema != SchemaVersion || report.Team != "research" || report.Cohort != "tau-team-research" {
		t.Fatalf("hierarchy = %#v", report)
	}
	if len(report.Flavors) != 1 || report.Flavors[0].Resources[0].Nominal != "8" ||
		report.Flavors[0].Resources[0].Reserved != "6" {
		t.Fatalf("workspace quota = %#v", report.Flavors)
	}
	if report.TeamShared == nil || report.TeamShared.Name != "tau-team-research" ||
		report.TeamShared.Flavors[0].Resources[0].Nominal != "20" {
		t.Fatalf("team shared quota = %#v", report.TeamShared)
	}

	table := RenderTable(report)
	for _, want := range []string{
		"Workspace allocation by flavor",
		"Team shared allocation tau-team-research",
		"administrative quota, not physical capacity",
	} {
		if !strings.Contains(table, want) {
			t.Fatalf("table missing %q:\n%s", want, table)
		}
	}
}

func TestBuildLegacyClusterQueueOmitsTeamLevel(t *testing.T) {
	report, err := Build(Input{
		ClusterQueue: "jobqueue",
		ClusterQueueRaw: []byte(`{
			"metadata":{"name":"jobqueue"},
			"spec":{"resourceGroups":[]}
		}`),
		FlavorsRaw: map[string][]byte{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Team != "" || report.Cohort != "" || report.TeamShared != nil {
		t.Fatalf("legacy report gained team hierarchy: %#v", report)
	}
}
