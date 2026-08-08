// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package links

import (
	"context"
	"github.com/Azure/taugrid/core/workloadmeta"
	"testing"
)

// stubReader returns canned Kueue Workloads JSON so the projection can be
// tested without a live API. It records the namespace it was asked for.
type stubReader struct {
	json   string
	err    error
	lastNS string
}

func (s *stubReader) ListWorkloads(_ context.Context, namespace string) ([]byte, error) {
	s.lastNS = namespace
	if s.err != nil {
		return nil, s.err
	}
	return []byte(s.json), nil
}

func TestListWorkloadsProjectsJoinKeys(t *testing.T) {
	// One admitted workload with run-id/job labels, one pending without them,
	// one finished. The projection must keep run-id (which queue.Snapshot drops)
	// and classify admission state.
	const raw = `{"items":[
      {"metadata":{"name":"wl-admitted","namespace":"ray","creationTimestamp":"2026-01-02T03:04:05Z",
        "labels":{"` + workloadmeta.LabelRunID + `":"train-77","` + workloadmeta.LabelJob + `":"phi-finetune"}},
       "spec":{"queueName":"jobqueue"},
       "status":{"admission":{"clusterQueue":"taugrid-cq"},
         "conditions":[{"type":"Admitted","status":"True"}]}},
      {"metadata":{"name":"wl-pending","namespace":"ray"},
       "spec":{"queueName":"jobqueue"},
       "status":{"conditions":[{"type":"Admitted","status":"False"}]}},
      {"metadata":{"name":"wl-done","namespace":"ray",
        "labels":{"` + workloadmeta.LabelRunID + `":"train-01","` + workloadmeta.LabelJob + `":"old"}},
       "spec":{"queueName":"jobqueue"},
       "status":{"conditions":[{"type":"Admitted","status":"True"},{"type":"Finished","status":"True"}]}}
    ]}`

	r := &stubReader{json: raw}
	got, err := ListWorkloads(context.Background(), r, "ray")
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if r.lastNS != "ray" {
		t.Fatalf("reader namespace = %q, want ray", r.lastNS)
	}
	if len(got) != 3 {
		t.Fatalf("got %d workloads, want 3: %+v", len(got), got)
	}

	byName := map[string]Workload{}
	for _, w := range got {
		byName[w.Name] = w
	}

	admitted := byName["wl-admitted"]
	if admitted.RunID != "train-77" || admitted.Job != "phi-finetune" {
		t.Fatalf("admitted join keys = %q/%q, want train-77/phi-finetune", admitted.RunID, admitted.Job)
	}
	if !admitted.Admitted || admitted.Finished || !admitted.Running() {
		t.Fatalf("admitted state = admitted:%v finished:%v running:%v, want running", admitted.Admitted, admitted.Finished, admitted.Running())
	}
	if admitted.ClusterQueue != "taugrid-cq" || admitted.Queue != "jobqueue" {
		t.Fatalf("admitted queue context = %q/%q, want jobqueue/taugrid-cq", admitted.Queue, admitted.ClusterQueue)
	}
	if admitted.CreatedAt.IsZero() {
		t.Fatal("admitted CreatedAt should be parsed")
	}

	pending := byName["wl-pending"]
	if pending.Admitted || pending.Running() {
		t.Fatalf("pending should not be admitted/running: %+v", pending)
	}
	if pending.RunID != "" {
		t.Fatalf("pending RunID = %q, want empty", pending.RunID)
	}

	done := byName["wl-done"]
	if !done.Finished || done.Running() {
		t.Fatalf("finished workload should not be running: %+v", done)
	}
}

func TestListWorkloadsSortsByNamespaceThenJob(t *testing.T) {
	const raw = `{"items":[
      {"metadata":{"name":"z","namespace":"ray","labels":{"` + workloadmeta.LabelJob + `":"zeta"}}},
      {"metadata":{"name":"a","namespace":"ray","labels":{"` + workloadmeta.LabelJob + `":"alpha"}}},
      {"metadata":{"name":"m","namespace":"kube-system"}}
    ]}`
	got, err := ListWorkloads(context.Background(), &stubReader{json: raw}, "")
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	order := []string{got[0].Namespace + "/" + got[0].sortKey(), got[1].Namespace + "/" + got[1].sortKey(), got[2].Namespace + "/" + got[2].sortKey()}
	want := []string{"kube-system/m", "ray/alpha", "ray/zeta"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("sort order = %v, want %v", order, want)
		}
	}
}

func TestListWorkloadsParsesOwnerReferences(t *testing.T) {
	// A Kueue Workload admitted for a RayJob carries no tau.azure.com labels but
	// does have an ownerReference back to the RayJob object. The projection must
	// surface those owner names so the Portal can join by ownership.
	const raw = `{"items":[
      {"metadata":{"name":"rayjob-portal-e2e-abcde","namespace":"ray",
        "ownerReferences":[{"name":"portal-e2e"},{"name":""}]},
       "spec":{"queueName":"team-a"},
       "status":{"conditions":[{"type":"Admitted","status":"True"}]}}
    ]}`
	got, err := ListWorkloads(context.Background(), &stubReader{json: raw}, "ray")
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d workloads, want 1", len(got))
	}
	if len(got[0].Owners) != 1 || got[0].Owners[0] != "portal-e2e" {
		t.Fatalf("owners = %v, want [portal-e2e] (empty names dropped)", got[0].Owners)
	}
}

func TestExperimentProjectPath(t *testing.T) {
	if got := ExperimentProjectPath("", "proj"); got != "" {
		t.Fatalf("empty runID = %q, want empty", got)
	}
	if got := ExperimentProjectPath("train-77", ""); got != "/stellar?target=train-77" {
		t.Fatalf("empty project = %q", got)
	}
	if got := ExperimentProjectPath("train-77", "sample"); got != "/stellar?target=train-77&project=sample" {
		t.Fatalf("project path = %q", got)
	}
	if got := ExperimentProjectPath("train-77", "sample", "team alpha"); got != "/stellar?project=sample&target=train-77&workspace=team+alpha" {
		t.Fatalf("project+workspace path = %q", got)
	}
}

func TestExperimentPath(t *testing.T) {
	cases := map[string]string{
		"":               "",
		"  ":             "",
		"train-77":       "/stellar?target=train-77",
		"proj/run space": "/stellar?target=proj%2Frun+space",
		"tau-abc-123def": "/stellar?target=tau-abc-123def",
	}
	for in, want := range cases {
		if got := ExperimentPath(in); got != want {
			t.Fatalf("ExperimentPath(%q) = %q, want %q", in, got, want)
		}
		if got := ExperimentPath("train-77", "team alpha"); got != "/stellar?target=train-77&workspace=team+alpha" {
			t.Fatalf("workspace ExperimentPath = %q", got)
		}
	}
}

func TestClusterInstancePath(t *testing.T) {
	if got := ClusterInstancePath(""); got != "" {
		t.Fatalf("empty instance = %q, want empty", got)
	}
	if got := ClusterInstancePath("aks-gpu-0"); got != "/portal/cluster?instance=aks-gpu-0" {
		t.Fatalf("ClusterInstancePath = %q", got)
	}
	if got := ClusterInstancePath("aks-gpu-0", "alpha"); got != "/portal/cluster?instance=aks-gpu-0&workspace=alpha" {
		t.Fatalf("workspace ClusterInstancePath = %q", got)
	}
}

func TestRayDashboardPath(t *testing.T) {
	if got := RayDashboardPath("", "ray-cl"); got != "" {
		t.Fatalf("empty namespace = %q, want empty", got)
	}
	if got := RayDashboardPath("ray", ""); got != "" {
		t.Fatalf("empty cluster = %q, want empty", got)
	}
	if got := RayDashboardPath("ray", "train-raycluster"); got != "/api/portal/ray/proxy/ray/train-raycluster/" {
		t.Fatalf("RayDashboardPath = %q", got)
	}
	if got := RayDashboardPath("ray", "train-raycluster", "alpha"); got != "/api/portal/ray/proxy/ray/train-raycluster/?workspace=alpha" {
		t.Fatalf("workspace RayDashboardPath = %q", got)
	}
}

func TestWorkspacePathPreservesExistingQuery(t *testing.T) {
	if got := WorkspacePath("/portal/fleet?view=util", "alpha"); got != "/portal/fleet?view=util&workspace=alpha" {
		t.Fatalf("WorkspacePath = %q", got)
	}
}

func TestListWorkloadsPropagatesError(t *testing.T) {
	_, err := ListWorkloads(context.Background(), &stubReader{err: context.Canceled}, "ray")
	if err == nil {
		t.Fatal("expected error to propagate")
	}
}

// TestListWorkloadsProjectsStellarIdentity locks the alignment between what
// `tau run` stamps and what the portal reads: every run path merges
// experiment.Metadata onto the Job, so the Stellar project/experiment/group and
// the workspace ride along on the Workload Kueue admits. Dropping them left the
// overview showing a bare run-id for a row whose experiment identity was
// already on the object.
func TestListWorkloadsProjectsStellarIdentity(t *testing.T) {
	const raw = `{"items":[
      {"metadata":{"name":"wl","namespace":"ray","labels":{
         "` + workloadmeta.LabelRunID + `":"train-77",
         "` + workloadmeta.LabelJob + `":"phi-finetune",
         "` + workloadmeta.LabelStellarProject + `":"nanogpt-fineweb",
         "` + workloadmeta.LabelStellarExperiment + `":"nanogpt-api-surface",
         "` + workloadmeta.LabelStellarGroup + `":"safe-stack-h200",
         "` + workloadmeta.LabelWorkspace + `":"aurora"}},
       "spec":{"queueName":"jobqueue"},
       "status":{"conditions":[{"type":"Admitted","status":"True"}]}}
    ]}`

	got, err := ListWorkloads(context.Background(), &stubReader{json: raw}, "ray")
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d workloads, want 1", len(got))
	}
	w := got[0]
	if w.Project != "nanogpt-fineweb" || w.Experiment != "nanogpt-api-surface" ||
		w.Group != "safe-stack-h200" || w.Workspace != "aurora" {
		t.Fatalf("stellar identity = %+v, want project/experiment/group/workspace projected", w)
	}
}

// TestListWorkloadsWithoutStellarIdentity guards the non-tau case: a Workload
// admitted for an object nobody stamped must leave the identity fields empty
// (they are omitempty) rather than inventing a project.
func TestListWorkloadsWithoutStellarIdentity(t *testing.T) {
	const raw = `{"items":[{"metadata":{"name":"wl","namespace":"ray"},
      "spec":{"queueName":"jobqueue"},"status":{"conditions":[]}}]}`

	got, err := ListWorkloads(context.Background(), &stubReader{json: raw}, "ray")
	if err != nil {
		t.Fatalf("ListWorkloads: %v", err)
	}
	if got[0].Project != "" || got[0].Experiment != "" || got[0].Group != "" || got[0].Workspace != "" {
		t.Fatalf("identity = %+v, want all empty for an unstamped workload", got[0])
	}
}
