// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package queuequota

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

type fetchRunner struct {
	responses map[string]string
	calls     []string
}

func (r *fetchRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	key := strings.Join(args, " ")
	r.calls = append(r.calls, key)
	if response, ok := r.responses[key]; ok {
		return response, nil
	}
	return "", fmt.Errorf("unexpected call %s", key)
}

func TestFetchReadsWorkspaceClusterQueueTeamCohortAndFlavors(t *testing.T) {
	runner := &fetchRunner{responses: map[string]string{
		"get clusterqueue.kueue.x-k8s.io tau-ws-vision -o json": `{
			"metadata":{"name":"tau-ws-vision","labels":{"tau.azure.com/team":"research"}},
			"spec":{"cohortName":"tau-team-research","resourceGroups":[{"flavors":[{
				"name":"h200","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]
			}]}]}
		}`,
		"get cohort.kueue.x-k8s.io tau-team-research -o json": `{
			"metadata":{"name":"tau-team-research"},
			"spec":{"resourceGroups":[{"flavors":[{
				"name":"a100","resources":[{"name":"nvidia.com/gpu","nominalQuota":"12"}]
			}]}]}
		}`,
		"get resourceflavor.kueue.x-k8s.io h200 -o json": `{"spec":{}}`,
		"get resourceflavor.kueue.x-k8s.io a100 -o json": `{"spec":{}}`,
	}}
	report, err := Fetch(context.Background(), runner, FetchOptions{
		Workspace:    "vision",
		ClusterQueue: "tau-ws-vision",
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.TeamShared == nil || len(report.TeamShared.Flavors) != 1 ||
		report.TeamShared.Flavors[0].Name != "a100" {
		t.Fatalf("team shared quota = %#v", report.TeamShared)
	}
	for _, want := range []string{
		"get cohort.kueue.x-k8s.io tau-team-research -o json",
		"get resourceflavor.kueue.x-k8s.io a100 -o json",
	} {
		found := false
		for _, call := range runner.calls {
			if call == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing call %q in %#v", want, runner.calls)
		}
	}
}
