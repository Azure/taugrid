// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/Azure/taugrid/core/queue"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// fakeReader records the arguments it was called with and returns canned JSON,
// so Board can be exercised without a live Kubernetes API.
type fakeReader struct {
	localJSON, clusterJSON, workloadJSON []byte
	localErr, clusterErr, workloadErr    error
	localByNS, workloadByNS              map[string][]byte

	localNS    []string
	workloadNS []string
	clusterHit bool
}

func (f *fakeReader) ListLocalQueues(_ context.Context, ns string) ([]byte, error) {
	f.localNS = append(f.localNS, ns)
	if raw, ok := f.localByNS[ns]; ok {
		return raw, f.localErr
	}
	return f.localJSON, f.localErr
}

func (f *fakeReader) ListClusterQueues(_ context.Context) ([]byte, error) {
	f.clusterHit = true
	return f.clusterJSON, f.clusterErr
}

func (f *fakeReader) ListWorkloads(_ context.Context, ns string) ([]byte, error) {
	f.workloadNS = append(f.workloadNS, ns)
	if raw, ok := f.workloadByNS[ns]; ok {
		return raw, f.workloadErr
	}
	return f.workloadJSON, f.workloadErr
}

// writeTestPolicy writes a minimal TopologyPolicy whose single preset maps the
// queue/clusterQueue used by the fixtures below, and returns its path.
func writeTestPolicy(t *testing.T) string {
	t.Helper()
	const policy = `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata:
  name: test-jobs
spec:
  description: "test policy for the portal jobs board"
  presets:
    azure.research.training.l:
      team: research
      lane: training
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: research-training
      clusterQueue: team-research-reserved-cq
      namespace: ray
`
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(policy), 0o600); err != nil {
		t.Fatalf("write policy: %v", err)
	}
	return path
}

const (
	fixtureLocalQueues = `{"items":[
      {"metadata":{"name":"research-training"},"spec":{"clusterQueue":"team-research-reserved-cq"},
       "status":{"pendingWorkloads":1,"admittedWorkloads":0,"reservingWorkloads":0}}
    ]}`
	fixtureClusterQueues = `{"items":[
      {"metadata":{"name":"team-research-reserved-cq"},
       "spec":{"resourceGroups":[{"coveredResources":["gpu.nvidia.com"],
         "flavors":[{"name":"gpu-any","resources":[{"name":"gpu.nvidia.com","nominalQuota":"8"}]}]}]},
       "status":{"flavorsReservation":[{"name":"gpu-any","resources":[{"name":"gpu.nvidia.com","total":"0","borrowed":"0"}]}],
                 "flavorsUsage":[{"name":"gpu-any","resources":[{"name":"gpu.nvidia.com","total":"0","borrowed":"0"}]}]}}
    ]}`
	fixtureWorkloads = `{"items":[
      {"metadata":{"name":"wait-1","namespace":"ray","creationTimestamp":"2026-05-03T20:00:00Z",
        "labels":{"` + workloadmeta.LabelTeam + `":"research","` + workloadmeta.LabelLane + `":"training","` + workloadmeta.LabelPreset + `":"azure.research.training.l"}},
       "spec":{"queueName":"research-training","podSets":[{"count":1,
         "template":{"spec":{"containers":[{"resources":{"requests":{"gpu.nvidia.com":"1"}}}]}}}]},
       "status":{"conditions":[{"type":"Admitted","status":"False","reason":"QuotaNotReserved","message":"insufficient quota"}]}}
    ]}`
)

// TestBoardFetchesThreeListsAndAggregates verifies the client-go seam: Board
// reads localqueues + workloads in the target namespace, clusterqueues cluster
// wide, and passes the bytes to queue.BuildSnapshot() to produce a Snapshot.
func TestBoardFetchesThreeListsAndAggregates(t *testing.T) {
	r := &fakeReader{
		localJSON:    []byte(fixtureLocalQueues),
		clusterJSON:  []byte(fixtureClusterQueues),
		workloadJSON: []byte(fixtureWorkloads),
	}
	snap, err := Board(context.Background(), r, Options{
		Scopes:     []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
		PolicyPath: writeTestPolicy(t),
	})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}

	if len(r.localNS) != 1 || r.localNS[0] != "ray" || len(r.workloadNS) != 1 || r.workloadNS[0] != "ray" {
		t.Fatalf("namespaced reads used ns local=%q workload=%q, want ray", r.localNS, r.workloadNS)
	}
	if !r.clusterHit {
		t.Fatal("clusterqueues were not read")
	}
	if snap.Namespace != "ray" {
		t.Fatalf("snapshot namespace = %q, want ray", snap.Namespace)
	}
	if len(snap.Groups) != 1 {
		t.Fatalf("groups = %d, want 1: %#v", len(snap.Groups), snap.Groups)
	}
	g := snap.Groups[0]
	if g.Queue != "research-training" || g.Pending != 1 {
		t.Fatalf("group = %#v, want queue=research-training pending=1", g)
	}
	if len(g.PendingWorkloads) != 1 || g.PendingWorkloads[0].Name != "wait-1" {
		t.Fatalf("pending workloads = %#v, want [wait-1]", g.PendingWorkloads)
	}
}

func TestBoardRejectsMissingScopes(t *testing.T) {
	r := &fakeReader{
		localJSON:    []byte(`{"items":[]}`),
		clusterJSON:  []byte(`{"items":[]}`),
		workloadJSON: []byte(`{"items":[]}`),
	}
	if _, err := Board(context.Background(), r, Options{PolicyPath: writeTestPolicy(t)}); err == nil {
		t.Fatal("Board succeeded without an explicit scope")
	}
}

func TestBoardReadsOnlyConfiguredScope(t *testing.T) {
	r := &fakeReader{
		localJSON:    []byte(`{"items":[]}`),
		clusterJSON:  []byte(`{"items":[]}`),
		workloadJSON: []byte(`{"items":[]}`),
	}
	_, _ = Board(context.Background(), r, Options{
		Scopes:     []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
		PolicyPath: writeTestPolicy(t),
	})
	if len(r.localNS) != 1 || r.localNS[0] != "ray" || len(r.workloadNS) != 1 || r.workloadNS[0] != "ray" {
		t.Fatalf("namespace not applied: local=%q workload=%q want ray", r.localNS, r.workloadNS)
	}
}

func TestBoardRejectsScopeWithoutPolicyGroup(t *testing.T) {
	r := &fakeReader{
		localJSON:    []byte(fixtureLocalQueues),
		clusterJSON:  []byte(fixtureClusterQueues),
		workloadJSON: []byte(fixtureWorkloads),
	}
	_, err := Board(context.Background(), r, Options{
		Scopes:     []Scope{{Team: "research", Namespace: "ray", Queue: "other-queue"}},
		PolicyPath: writeTestPolicy(t),
	})
	if err == nil {
		t.Fatal("Board accepted a scope with no matching policy group")
	}
}

func TestBoardRejectsPolicyAndLiveLocalQueueClusterQueueMismatch(t *testing.T) {
	r := &fakeReader{
		localJSON: []byte(`{"items":[
			{"metadata":{"name":"research-training","namespace":"ray"},"spec":{"clusterQueue":"other-cq"}}
		]}`),
		clusterJSON:  []byte(fixtureClusterQueues),
		workloadJSON: []byte(`{"items":[]}`),
	}
	_, err := Board(context.Background(), r, Options{
		Scopes:     []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
		PolicyPath: writeTestPolicy(t),
	})
	if err == nil {
		t.Fatal("Board accepted a policy ClusterQueue that differs from the live LocalQueue binding")
	}
}

func TestBoardRejectsMissingLiveLocalQueue(t *testing.T) {
	r := &fakeReader{
		localJSON:    []byte(`{"items":[]}`),
		clusterJSON:  []byte(fixtureClusterQueues),
		workloadJSON: []byte(`{"items":[]}`),
	}
	_, err := Board(context.Background(), r, Options{
		Scopes:     []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
		PolicyPath: writeTestPolicy(t),
	})
	if err == nil {
		t.Fatal("Board accepted a configured scope without a live LocalQueue")
	}
}

// TestBoardPropagatesFetchError verifies a reader error surfaces (a queue view
// without Kueue data is more misleading than an error, matching queue.Fetch()).
func TestBoardPropagatesFetchError(t *testing.T) {
	sentinel := errors.New("boom")
	r := &fakeReader{
		localJSON:   []byte(`{"items":[]}`),
		clusterErr:  sentinel,
		localErr:    nil,
		workloadErr: nil,
	}
	_, err := Board(context.Background(), r, Options{
		Scopes:     []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
		PolicyPath: writeTestPolicy(t),
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
	}
}

func TestBoardCorrelatesSharedQueueNameByExplicitTeamAndNamespace(t *testing.T) {
	const policy = `apiVersion: tau.azure.com/v1alpha1
kind: TopologyPolicy
metadata:
  name: teams
spec:
  presets:
    team-a.train:
      team: team-a
      lane: training
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: jobqueue
      clusterQueue: shared-cq
      resourceFlavor: gpu
    team-b.train:
      team: team-b
      lane: training
      mode: fixed
      placement: independent
      shape: 1xgpu
      gpuClass: any
      queue: jobqueue
      clusterQueue: shared-cq
      resourceFlavor: gpu
`
	policyPath := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(policyPath, []byte(policy), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &fakeReader{
		localByNS: map[string][]byte{
			"team-a": []byte(`{"items":[{"metadata":{"name":"jobqueue","namespace":"team-a"},"spec":{"clusterQueue":"shared-cq"},"status":{"pendingWorkloads":2,"admittedWorkloads":1}}]}`),
			"team-b": []byte(`{"items":[{"metadata":{"name":"jobqueue","namespace":"team-b"},"spec":{"clusterQueue":"shared-cq"},"status":{"pendingWorkloads":3,"admittedWorkloads":4}}]}`),
		},
		workloadByNS: map[string][]byte{
			"team-a": []byte(`{"items":[]}`),
			"team-b": []byte(`{"items":[]}`),
		},
		clusterJSON: []byte(`{"items":[{"metadata":{"name":"shared-cq"},"spec":{"resourceGroups":[{"coveredResources":["gpu.nvidia.com"],"flavors":[{"name":"gpu","resources":[{"name":"gpu.nvidia.com","nominalQuota":"8"}]}]}]},"status":{"flavorsReservation":[{"name":"gpu","resources":[{"name":"gpu.nvidia.com","total":"6"}]}],"flavorsUsage":[{"name":"gpu","resources":[{"name":"gpu.nvidia.com","total":"5"}]}]}}]}`),
	}
	snapshot, err := Board(context.Background(), r, Options{
		Scopes: []Scope{
			{Team: "team-a", Namespace: "team-a", Queue: "jobqueue"},
			{Team: "team-b", Namespace: "team-b", Queue: "jobqueue"},
		},
		PolicyPath: policyPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Groups) != 2 {
		t.Fatalf("groups = %#v, want exactly two", snapshot.Groups)
	}
	for i, want := range []Scope{
		{Team: "team-a", Namespace: "team-a", Queue: "jobqueue"},
		{Team: "team-b", Namespace: "team-b", Queue: "jobqueue"},
	} {
		got := snapshot.Groups[i]
		if got.Team != want.Team || got.Namespace != want.Namespace || got.Queue != want.Queue {
			t.Fatalf("group %d = %#v, want scope %#v", i, got, want)
		}
	}
	summary := Summarize(snapshot)
	if summary.Pending != 5 || summary.Admitted != 5 {
		t.Fatalf("queue summary = %#v, want pending=5 admitted=5", summary)
	}
	if summary.GPUUsed != 5 || summary.GPUHeadroom != 2 {
		t.Fatalf("GPU summary = %#v, want shared quota counted once", summary)
	}
}

func TestBoardRejectsConflictingScopeTeams(t *testing.T) {
	_, err := Board(context.Background(), &fakeReader{}, Options{
		Scopes: []Scope{
			{Team: "team-a", Namespace: "shared", Queue: "jobqueue"},
			{Team: "team-b", Namespace: "shared", Queue: "jobqueue"},
		},
	})
	if err == nil {
		t.Fatal("Board accepted conflicting teams for one LocalQueue")
	}
}

func TestSummarizeDoesNotAddNamedFlavorsToAggregateClusterQueueQuota(t *testing.T) {
	summary := Summarize(queue.Snapshot{Groups: []queue.Group{
		{ClusterQueue: "shared-cq", ResourceFlavor: "h200", GPUUsed: 2, GPUHeadroom: 4},
		{ClusterQueue: "shared-cq", GPUUsed: 6, GPUHeadroom: 10},
		{ClusterQueue: "shared-cq", ResourceFlavor: "a100", GPUUsed: 4, GPUHeadroom: 6},
	}})
	if summary.GPUUsed != 6 || summary.GPUHeadroom != 10 {
		t.Fatalf("GPU summary = %#v, want aggregate ClusterQueue quota counted once", summary)
	}
}
