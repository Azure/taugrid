// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobs

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/queue"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
		Scopes:   []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
		Lane:     "training",
		GPUClass: "h200",
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
	if _, err := Board(context.Background(), r, Options{}); err == nil {
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
		Scopes: []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
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
		Scopes: []Scope{{Team: "research", Namespace: "ray", Queue: "other-queue"}},
	})
	if err == nil {
		t.Fatal("Board accepted a scope with no matching policy group")
	}
}

func TestBoardUsesLiveLocalQueueClusterQueueWithoutPolicyFallback(t *testing.T) {
	r := &fakeReader{
		localJSON: []byte(`{"items":[
			{"metadata":{"name":"research-training","namespace":"ray"},"spec":{"clusterQueue":"other-cq"}}
		]}`),
		clusterJSON:  []byte(fixtureClusterQueues),
		workloadJSON: []byte(`{"items":[]}`),
	}
	snapshot, err := Board(context.Background(), r, Options{
		Scopes: []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
	})
	if err != nil {
		t.Fatalf("Board: %v", err)
	}
	if len(snapshot.Groups) != 1 || snapshot.Groups[0].ClusterQueue != "other-cq" {
		t.Fatalf("snapshot = %#v, want authoritative live binding", snapshot)
	}
}

func TestBoardRejectsMissingLiveLocalQueue(t *testing.T) {
	r := &fakeReader{
		localJSON:    []byte(`{"items":[]}`),
		clusterJSON:  []byte(fixtureClusterQueues),
		workloadJSON: []byte(`{"items":[]}`),
	}
	_, err := Board(context.Background(), r, Options{
		Scopes: []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
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
		Scopes: []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want it to wrap %v", err, sentinel)
	}
}

func TestBoardCorrelatesSharedQueueNameByExplicitTeamAndNamespace(t *testing.T) {
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
	summary := Summarize(snapshot.Snapshot)
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

type fakeProfileReader struct {
	sets  []profile.ProfileSet
	errs  []error
	calls int
}

func (f *fakeProfileReader) ProfileSet(context.Context) (profile.ProfileSet, error) {
	index := f.calls
	f.calls++
	if index >= len(f.sets) {
		index = len(f.sets) - 1
	}
	var err error
	if index >= 0 && index < len(f.errs) {
		err = f.errs[index]
	}
	if index < 0 {
		return profile.ProfileSet{}, err
	}
	return f.sets[index], err
}

func readyProfile(name string, target profile.ExecutionTarget, generation int64, namespaces, teams []string) profile.ResolvedWorkloadProfile {
	return profile.ResolvedWorkloadProfile{
		WorkloadProfile: profile.WorkloadProfile{
			Name: name, ExecutionTarget: target, Placement: profile.PlacementIndependent,
			DefaultLocalQueue: "research-training", GPUsPerWorker: 1, WorkerCount: 1,
			Applicability: profile.ProfileApplicability{Namespaces: namespaces, Teams: teams},
		},
		Conditions: []metav1.Condition{{
			Type: profile.ConditionReady, Status: metav1.ConditionTrue, ObservedGeneration: generation,
		}},
	}
}

func profileBoardReader() *fakeReader {
	return &fakeReader{
		localJSON: []byte(fixtureLocalQueues), clusterJSON: []byte(fixtureClusterQueues),
		workloadJSON: []byte(fixtureWorkloads),
	}
}

func TestBoardAttachesReadyProfilesWithAuthoritativeMetadata(t *testing.T) {
	const generation = 17
	reader := &fakeProfileReader{sets: []profile.ProfileSet{{
		Generation: generation, ProfileSetHash: "sha256:full-profile-set-hash",
		Profiles: []profile.ResolvedWorkloadProfile{
			readyProfile("stable", profile.ExecutionTargetSingleCluster, generation, []string{"ray"}, []string{"research"}),
			readyProfile("federated", profile.ExecutionTargetMultiKueue, generation, []string{"ray"}, []string{"research"}),
		},
	}}}
	snapshot, err := Board(context.Background(), profileBoardReader(), Options{
		Scopes:   []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
		Profiles: reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := snapshot.WorkloadProfiles
	if !got.Available || got.Generation != generation || got.ProfileSetHash != "sha256:full-profile-set-hash" ||
		!got.ReadOnly || got.SelectionEnabled || len(got.ReadyProfiles) != 2 {
		t.Fatalf("profile availability = %#v", got)
	}
	if got.ReadyProfiles[0].ExecutionTarget != profile.ExecutionTargetMultiKueue ||
		got.ReadyProfiles[1].ExecutionTarget != profile.ExecutionTargetSingleCluster {
		t.Fatalf("profile execution targets = %#v", got.ReadyProfiles)
	}
}

func TestBoardProfileFailuresAndUnreadyProfilesDoNotHideJobs(t *testing.T) {
	for _, tt := range []struct {
		name    string
		reader  *fakeProfileReader
		wantErr string
	}{
		{"forbidden", &fakeProfileReader{sets: []profile.ProfileSet{{}}, errs: []error{errors.New("access to TauCluster \"cluster\" is forbidden")}}, "forbidden"},
		{"stale", &fakeProfileReader{sets: []profile.ProfileSet{{}}, errs: []error{errors.New("workload profiles are stale")}}, "stale"},
		{"unready set", &fakeProfileReader{sets: []profile.ProfileSet{{}}, errs: []error{errors.New("condition WorkloadProfilesReady is False")}}, "Ready is False"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			snapshot, err := Board(context.Background(), profileBoardReader(), Options{
				Scopes:   []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
				Profiles: tt.reader,
			})
			if err != nil {
				t.Fatalf("Board lost existing jobs: %v", err)
			}
			if len(snapshot.Groups) != 1 || snapshot.Groups[0].Pending != 1 {
				t.Fatalf("groups = %#v, want existing queue observation", snapshot.Groups)
			}
			if snapshot.WorkloadProfiles.Available || !strings.Contains(snapshot.WorkloadProfiles.Error, tt.wantErr) {
				t.Fatalf("profile availability = %#v", snapshot.WorkloadProfiles)
			}
		})
	}

	unready := readyProfile("not-ready", profile.ExecutionTargetSingleCluster, 9, []string{"ray"}, []string{"research"})
	unready.Conditions[0].Status = metav1.ConditionFalse
	snapshot, err := Board(context.Background(), profileBoardReader(), Options{
		Scopes: []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}},
		Profiles: &fakeProfileReader{sets: []profile.ProfileSet{{
			Generation: 9, ProfileSetHash: "hash", Profiles: []profile.ResolvedWorkloadProfile{unready},
		}}},
	})
	if err != nil || len(snapshot.Groups) != 1 || len(snapshot.WorkloadProfiles.ReadyProfiles) != 0 {
		t.Fatalf("unready profile handling = snapshot %#v, error %v", snapshot, err)
	}
}

func TestReadProfilesFiltersApplicabilityByNamespaceAndTeam(t *testing.T) {
	set := profile.ProfileSet{Generation: 4, ProfileSetHash: "hash", Profiles: []profile.ResolvedWorkloadProfile{
		readyProfile("allowed", profile.ExecutionTargetSingleCluster, 4, []string{"ray"}, []string{"research"}),
		readyProfile("wrong-namespace", profile.ExecutionTargetSingleCluster, 4, []string{"other"}, []string{"research"}),
		readyProfile("wrong-team", profile.ExecutionTargetSingleCluster, 4, []string{"ray"}, []string{"other"}),
	}}
	got := ReadProfiles(context.Background(), &fakeProfileReader{sets: []profile.ProfileSet{set}},
		[]Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}}, "")
	if len(got.ReadyProfiles) != 1 || got.ReadyProfiles[0].Name != "allowed" {
		t.Fatalf("ready profiles = %#v", got.ReadyProfiles)
	}
}

func TestReadProfilesRefreshesOnConsecutiveRequests(t *testing.T) {
	reader := &fakeProfileReader{sets: []profile.ProfileSet{
		{Generation: 1, ProfileSetHash: "one"},
		{Generation: 2, ProfileSetHash: "two"},
	}}
	scopes := []Scope{{Team: "research", Namespace: "ray", Queue: "research-training"}}
	first := ReadProfiles(context.Background(), reader, scopes, "")
	second := ReadProfiles(context.Background(), reader, scopes, "")
	if reader.calls != 2 || first.Generation != 1 || second.Generation != 2 ||
		first.ProfileSetHash != "one" || second.ProfileSetHash != "two" {
		t.Fatalf("refresh calls=%d first=%#v second=%#v", reader.calls, first, second)
	}
}
