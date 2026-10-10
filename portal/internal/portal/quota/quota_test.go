// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package quota

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeReader struct {
	localQueue, clusterQueue, cohort []byte
	clusterQueues                    map[string][]byte
	localQueueNamespaces             []string
	localQueueNames                  []string
	clusterQueueNames, cohortNames   []string
	cohortErr                        error
}

func (f *fakeReader) GetLocalQueue(_ context.Context, namespace, name string) ([]byte, error) {
	f.localQueueNamespaces = append(f.localQueueNamespaces, namespace)
	f.localQueueNames = append(f.localQueueNames, name)
	return f.localQueue, nil
}
func (f *fakeReader) GetClusterQueue(_ context.Context, name string) ([]byte, error) {
	f.clusterQueueNames = append(f.clusterQueueNames, name)
	if f.clusterQueues != nil {
		if raw, ok := f.clusterQueues[name]; ok {
			return raw, nil
		}
		return nil, apierrors.NewNotFound(
			schema.GroupResource{Group: "kueue.x-k8s.io", Resource: "clusterqueues"},
			name,
		)
	}
	return f.clusterQueue, nil
}
func (f *fakeReader) GetCohort(_ context.Context, name string) ([]byte, error) {
	f.cohortNames = append(f.cohortNames, name)
	return f.cohort, f.cohortErr
}

func TestReadReportsWorkspaceAndTeamQuota(t *testing.T) {
	reader := &fakeReader{
		localQueue: []byte(`{"spec":{"clusterQueue":"tau-ws-vision"}}`),
		clusterQueue: []byte(`{
			"metadata":{"name":"tau-ws-vision"},
			"spec":{"cohortName":"tau-team-research","resourceGroups":[{"flavors":[{"name":"h200","resources":[
				{"name":"nvidia.com/gpu","nominalQuota":"8","borrowingLimit":"2","lendingLimit":"1"}
			]}]}]},
			"status":{"pendingWorkloads":2,"reservingWorkloads":3,"admittedWorkloads":2,
				"flavorsReservation":[{"name":"h200","resources":[{"name":"nvidia.com/gpu","total":"6"}]}],
				"flavorsUsage":[{"name":"h200","resources":[{"name":"nvidia.com/gpu","total":"5","borrowed":"1"}]}]}
		}`),
		cohort: []byte(`{
			"metadata":{"name":"tau-team-research"},
			"spec":{"resourceGroups":[{"flavors":[{"name":"h200","resources":[{"name":"nvidia.com/gpu","nominalQuota":"24"}]}]}]},
			"status":{"fairSharing":{"weightedShare":"1500m"}}
		}`),
	}
	got, err := Read(context.Background(), reader, Scope{
		Workspace: "vision", Team: "research", Namespace: "vision", LocalQueue: "jobqueue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Legacy || got.Workspace.Name != "tau-ws-vision" || got.Team == nil {
		t.Fatalf("snapshot = %+v", got)
	}
	resource := got.Workspace.Resources[0]
	if resource.Nominal != "8" || resource.Reserved != "6" || resource.Used != "5" || resource.Borrowed != "1" {
		t.Fatalf("workspace resource = %+v", resource)
	}
	if got.Team.Resources[0].Nominal != "24" || got.Team.UsageAvailable || got.Team.WeightedShare != "1500m" {
		t.Fatalf("team quota = %+v", got.Team)
	}
	if fmt.Sprint(reader.cohortNames) != "[tau-team-research]" {
		t.Fatalf("cohort reads = %v", reader.cohortNames)
	}
}

func TestReadFallsBackToLegacyLocalQueueAndToleratesMissingCohort(t *testing.T) {
	reader := &fakeReader{
		localQueue:   []byte(`{"spec":{"clusterQueue":"tau-cq"}}`),
		clusterQueue: []byte(`{"metadata":{"name":"tau-cq"},"spec":{"cohortName":"tau-team-research","resourceGroups":[]}}`),
		cohortErr: apierrors.NewNotFound(
			schema.GroupResource{Group: "kueue.x-k8s.io", Resource: "cohorts"}, "tau-team-research",
		),
	}
	got, err := Read(context.Background(), reader, Scope{
		Workspace: "vision", Team: "research", Namespace: "vision", LocalQueue: "jobqueue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Legacy || got.Workspace.Name != "tau-cq" || got.Team != nil {
		t.Fatalf("legacy snapshot = %+v", got)
	}
	if fmt.Sprint(reader.clusterQueueNames) != "[tau-cq]" {
		t.Fatalf("cluster queue reads = %v", reader.clusterQueueNames)
	}
}

func TestReadUsesAuthorizedLocalQueueWhenDirectoryIDCollides(t *testing.T) {
	reader := &fakeReader{
		localQueue: []byte(`{"spec":{"clusterQueue":"authorized-cq"}}`),
		clusterQueues: map[string][]byte{
			"tau-ws-collision": []byte(`{
				"metadata":{"name":"tau-ws-collision"},
				"spec":{"resourceGroups":[]},
				"status":{"admittedWorkloads":99}
			}`),
			"authorized-cq": []byte(`{
				"metadata":{"name":"authorized-cq"},
				"spec":{"resourceGroups":[]},
				"status":{"admittedWorkloads":1}
			}`),
		},
	}
	got, err := Read(context.Background(), reader, Scope{
		Workspace: "collision", Namespace: "authorized-ns", LocalQueue: "authorized-lq",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Workspace.Name != "authorized-cq" || got.Workspace.AdmittedWorkloads != 1 {
		t.Fatalf("snapshot = %+v", got)
	}
	if fmt.Sprint(reader.clusterQueueNames) != "[authorized-cq]" {
		t.Fatalf("cluster queue reads = %v", reader.clusterQueueNames)
	}
	if fmt.Sprint(reader.localQueueNamespaces) != "[authorized-ns]" ||
		fmt.Sprint(reader.localQueueNames) != "[authorized-lq]" {
		t.Fatalf(
			"LocalQueue reads namespaces=%v names=%v",
			reader.localQueueNamespaces,
			reader.localQueueNames,
		)
	}
}

func TestReadDoesNotFetchTeamCohortWhenQueueDoesNotReferenceExpectedTeam(t *testing.T) {
	reader := &fakeReader{
		localQueue: []byte(`{"spec":{"clusterQueue":"tau-ws-vision"}}`),
		clusterQueue: []byte(`{
			"metadata":{"name":"tau-ws-vision"},
			"spec":{"cohortName":"foreign-cohort","resourceGroups":[]}
		}`),
	}
	got, err := Read(context.Background(), reader, Scope{
		Workspace: "vision", Team: "research", Namespace: "vision", LocalQueue: "jobqueue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Legacy || len(reader.cohortNames) != 0 {
		t.Fatalf("snapshot = %+v, cohort reads = %v", got, reader.cohortNames)
	}
}
