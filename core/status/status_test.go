// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestRender_NotFound(t *testing.T) {
	out := Render(Snapshot{Name: "ghost", Namespace: "tau"})
	if !strings.Contains(out, "no batch/v1 Job or RayJob found") {
		t.Errorf("expected not-found marker, got:\n%s", out)
	}
}

func TestRender_Pending_Suspended(t *testing.T) {
	out := Render(Snapshot{
		Name: "j", Namespace: "tau",
		JobFound: true, JobSuspended: true,
		Workloads: []Workload{{Name: "w-1", Queue: "training-queue", Phase: "Pending"}},
	})
	if !strings.Contains(out, "Pending") || !strings.Contains(out, "suspended") {
		t.Errorf("expected suspended/pending headline, got:\n%s", out)
	}
	if !strings.Contains(out, "training-queue") {
		t.Errorf("expected queue name in workload row, got:\n%s", out)
	}
}

func TestRender_Running_StateAndPods(t *testing.T) {
	out := Render(Snapshot{
		Name: "j", Namespace: "tau",
		JobFound: true, JobActive: 2,
		Workloads: []Workload{{Name: "w-1", Queue: "training-queue", Phase: "Admitted", Admitted: true}},
		Pods: []Pod{
			{Name: "j-xyz", Phase: "Running", Node: "node-a", Ready: "1/1"},
			{Name: "j-abc", Phase: "Pending", Node: "", Ready: "0/1"},
		},
	})
	if !strings.Contains(out, "Running") {
		t.Errorf("expected Running headline, got:\n%s", out)
	}
	// Sort: Pending pods come before Running alphabetically — verify deterministic ordering.
	pendingIdx := strings.Index(out, "j-abc")
	runningIdx := strings.Index(out, "j-xyz")
	if pendingIdx == -1 || runningIdx == -1 || pendingIdx > runningIdx {
		t.Errorf("pods not sorted by phase: %d vs %d\n%s", pendingIdx, runningIdx, out)
	}
}

func TestRender_MultiKueueSectionShowsPlacementWorkerAndChecks(t *testing.T) {
	out := Render(Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobSuspended: true,
		Workloads: []Workload{{
			Name:        "train-001",
			Queue:       "sample-h100",
			ClusterName: "worker-a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Ready", Message: "Reservation acquired on worker-a", ControllerName: multiKueueControllerName},
				{Name: "prov-worker-a", State: "Ready", Message: "Worker accepted the workload", ControllerName: multiKueueControllerName},
			},
		}},
	})
	for _, want := range []string{
		"state:     Ready",
		"MultiKueue:",
		"placement: Ready",
		"worker:    worker-a",
		"train-001/multikueue=Ready controller=kueue.x-k8s.io/multikueue msg=Reservation acquired on worker-a",
		"train-001/prov-worker-a=Ready controller=kueue.x-k8s.io/multikueue msg=Worker accepted the workload",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
}

func TestRender_MultiKueueSelectedWithoutAdmissionChecks(t *testing.T) {
	out := Render(Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		Workloads: []Workload{{
			Name:        "train-001",
			Queue:       "sample-h100",
			ClusterName: "worker-b",
		}},
	})
	for _, want := range []string{
		"state:     Selected",
		"placement: Selected",
		"worker:    worker-b",
		"admission checks:\n    (none observed)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q:\n%s", want, out)
		}
	}
}

func TestRender_MultiKueueUnknownControllerRendersUnknown(t *testing.T) {
	out := Render(Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name:        "train-001",
			ClusterName: "worker-a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "custom-placement", State: "Pending", Message: "waiting for controller lookup", ControllerLookupFailed: true},
			},
		}},
	})
	if !strings.Contains(out, "train-001/custom-placement=Pending controller=unknown (lookup failed) msg=waiting for controller lookup") {
		t.Fatalf("expected unknown admission-check controller to be rendered explicitly:\n%s", out)
	}
	if !strings.Contains(out, "placement: Selected") {
		t.Fatalf("unknown controller should not be inferred as placement ready or rejected:\n%s", out)
	}
}

func TestRender_MultiKueueFailedLookupExactNameFallback(t *testing.T) {
	out := Render(Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name: "train-001",
			AdmissionChecks: []AdmissionCheck{{
				Name:                   multiKueueAdmissionCheckName,
				State:                  "Rejected",
				Message:                "quota exceeded",
				ControllerLookupFailed: true,
			}},
		}},
	})
	for _, want := range []string{
		"state:     Rejected",
		"placement: Rejected",
		"controller=unknown (lookup failed)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected failed-lookup exact-name fallback to preserve MultiKueue rejection:\n%s", out)
		}
	}
}

func TestRender_MultiKueueTerminalPlacementPrecedence(t *testing.T) {
	tests := []struct {
		name           string
		snap           Snapshot
		wantPlacement  string
		wantState      string
		unwantedPhrase string
	}{
		{
			name: "complete beats stale rejected placement",
			snap: Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				JobFound:  true,
				JobConditions: []Condition{
					{Type: "Complete", Status: "True"},
				},
				Workloads: []Workload{{
					Name:        "train-001",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{{
						Name:           multiKueueAdmissionCheckName,
						State:          "Rejected",
						Message:        "stale quota exceeded",
						ControllerName: multiKueueControllerName,
					}},
				}},
			},
			wantState:      "Complete",
			wantPlacement:  "Complete",
			unwantedPhrase: "placement: Rejected",
		},
		{
			name: "failed beats stale ready placement",
			snap: Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				JobFound:  true,
				JobConditions: []Condition{
					{Type: "Failed", Status: "True"},
				},
				Workloads: []Workload{{
					Name:        "train-001",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{{
						Name:           multiKueueAdmissionCheckName,
						State:          "Ready",
						Message:        "stale reservation acquired",
						ControllerName: multiKueueControllerName,
					}},
				}},
			},
			wantState:      "Failed",
			wantPlacement:  "Failed",
			unwantedPhrase: "placement: Ready",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := Render(tt.snap)
			if !strings.Contains(out, "state:     "+tt.wantState) || !strings.Contains(out, "placement: "+tt.wantPlacement) {
				t.Fatalf("terminal placement precedence not reflected in render:\n%s", out)
			}
			if strings.Contains(out, tt.unwantedPhrase) {
				t.Fatalf("render kept stale placement state %q:\n%s", tt.unwantedPhrase, out)
			}
			if !strings.Contains(out, "worker:    worker-a") || !strings.Contains(out, "admission checks:") {
				t.Fatalf("terminal render should retain worker and check details:\n%s", out)
			}
		})
	}
}

func TestRender_GenericAdmissionChecksDoNotRenderMultiKueueSection(t *testing.T) {
	out := Render(Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		Workloads: []Workload{{
			Name:  "train-001",
			Queue: "sample-h100",
			AdmissionChecks: []AdmissionCheck{
				{Name: "quota-check", State: "Rejected", Message: "quota full"},
			},
		}},
	})
	if strings.Contains(out, "MultiKueue:") {
		t.Fatalf("generic workload admission checks must not render MultiKueue state:\n%s", out)
	}
	if strings.Contains(out, "manager view only; worker-cluster pod lifecycle is not visible here") {
		t.Fatalf("generic workload admission checks must not trigger manager-view startup rendering:\n%s", out)
	}
}

func TestRender_RayJobOnlyMultiKueueWithoutWorkloadPreservesRayJobStatus(t *testing.T) {
	out := Render(Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		RayJob: RayJob{
			Found:               true,
			Name:                "train-001",
			ManagedBy:           multiKueueManagedBy,
			JobDeploymentStatus: "Running",
		},
	})
	if !strings.Contains(out, "status:    Running") {
		t.Fatalf("expected raw RayJob status to win when MultiKueue placement data is absent:\n%s", out)
	}
	for _, want := range []string{
		"MultiKueue:",
		"placement: Pending",
		"admission checks:\n    (none observed)",
		"[-]  Pod scheduling           manager view only; worker-cluster pod lifecycle is not visible here",
		"(none — manager view only; worker-cluster pods are not visible here)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("expected manager-view pod messaging for MultiKueue RayJob without local pods:\n%s", out)
		}
	}
}

func TestRender_MultiKueueWithLocalPodsMatchesOrdinaryOutputExact(t *testing.T) {
	base := Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		JobActive: 1,
		Workloads: []Workload{{
			Name:     "train-001",
			Queue:    "sample-h100",
			Phase:    "Admitted",
			Admitted: true,
		}},
		Pods: []Pod{{
			Name:  "train-001-head",
			Phase: "Running",
			Node:  "node-a",
			Ready: "1/1",
		}},
	}
	multiKueue := Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobActive:    1,
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name:        "train-001",
			Queue:       "sample-h100",
			Phase:       "Admitted",
			Admitted:    true,
			ClusterName: "worker-a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: multiKueueControllerName},
			},
		}},
		Pods: []Pod{{
			Name:  "train-001-head",
			Phase: "Running",
			Node:  "node-a",
			Ready: "1/1",
		}},
	}
	if got, want := Render(multiKueue), Render(base); got != want {
		t.Fatalf("local-pod MultiKueue render must match ordinary output exactly:\n--- ordinary ---\n%s\n--- multikueue ---\n%s", want, got)
	}
}

func TestRender_ManagerOnlyMultiKueueRayJobPrefersMirroredRayJobStatus(t *testing.T) {
	out := Render(Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		RayJob: RayJob{
			Found:               true,
			Name:                "train-001",
			ManagedBy:           multiKueueManagedBy,
			JobDeploymentStatus: "Running",
		},
		Workloads: []Workload{{
			Name:        "train-001",
			Admitted:    true,
			Phase:       "Admitted",
			ClusterName: "worker-a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: multiKueueControllerName},
			},
		}},
	})
	if !strings.Contains(out, "status:    Running") {
		t.Fatalf("expected mirrored RayJob status to win once execution has started:\n%s", out)
	}
	if !strings.Contains(out, "placement: Ready") {
		t.Fatalf("expected MultiKueue section to remain visible:\n%s", out)
	}
}

func TestRender_ManagerOnlyMultiKueueRayJobSuspendedUsesPendingHeadline(t *testing.T) {
	out := Render(Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		RayJob: RayJob{
			Found:               true,
			Name:                "train-001",
			ManagedBy:           multiKueueManagedBy,
			JobDeploymentStatus: "Suspended",
		},
	})
	if !strings.Contains(out, "status:    Pending (suspended; Kueue not yet admitted)") {
		t.Fatalf("expected suspended manager-only RayJob to preserve pending headline:\n%s", out)
	}
}

func TestRender_ManagerOnlyMultiKueueBatchPrefersMirroredRunningStatus(t *testing.T) {
	out := Render(Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobActive:    1,
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name:        "train-001",
			Admitted:    true,
			Phase:       "Admitted",
			ClusterName: "worker-a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: multiKueueControllerName},
			},
		}},
	})
	if !strings.Contains(out, "state:     Running") {
		t.Fatalf("expected mirrored batch JobActive to win once manager execution is visible:\n%s", out)
	}
	if !strings.Contains(out, "placement: Ready") {
		t.Fatalf("expected MultiKueue placement details to remain visible in manager view:\n%s", out)
	}
}

func TestRender_ManagerOnlyMultiKueueMixedReadyAndSelectedShowsSelectedWorker(t *testing.T) {
	out := Render(Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{
				Name:        "a",
				ClusterName: "worker-a",
				AdmissionChecks: []AdmissionCheck{
					{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: multiKueueControllerName},
				},
			},
			{
				Name:        "b",
				ClusterName: "worker-b",
			},
		},
	})
	if !strings.Contains(out, "placement: Selected") {
		t.Fatalf("expected selected aggregate placement:\n%s", out)
	}
	if !strings.Contains(out, "worker:    worker-b") {
		t.Fatalf("expected selected worker from the unfinished placement workload:\n%s", out)
	}
}

func TestRender_ManagerOnlyMultiKueueConflictingSelectedWorkersOmitsWorker(t *testing.T) {
	out := Render(Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{
			{Name: "a", ClusterName: "worker-a"},
			{Name: "b", ClusterName: "worker-b"},
		},
	})
	if !strings.Contains(out, "placement: Selected") {
		t.Fatalf("expected selected aggregate placement:\n%s", out)
	}
	if strings.Contains(out, "worker:") {
		t.Fatalf("expected conflicting worker assignments to omit worker detail:\n%s", out)
	}
}

func TestRender_NonMultiKueueOutputExact(t *testing.T) {
	snap := Snapshot{Name: "ghost", Namespace: "tau"}
	phaseLine := func(marker, name, detail string) string {
		return "  " + marker + "  " + fmt.Sprintf("%-24s", name) + " " + detail
	}
	want := strings.Join([]string{
		"Job: tau/ghost",
		"  (no batch/v1 Job or RayJob found with that name)",
		"",
		"Startup phases:",
		phaseLine("[ ]", "Submitted", "no batch/v1 Job or RayJob found"),
		phaseLine("[ ]", "Kueue admission", "workload not observed"),
		"      tip: check the kueue.x-k8s.io/queue-name label if this stays empty",
		phaseLine("[ ]", "Pod scheduling", "no pods observed yet"),
		phaseLine("[-]", "DRA allocation", "no ResourceClaims requested"),
		phaseLine("[ ]", "Image pull", "waiting for pods"),
		phaseLine("[-]", "Init containers", "no init containers"),
		phaseLine("[ ]", "Container start", "waiting for pods"),
		phaseLine("[ ]", "Ready", "waiting for pods"),
		"",
		"Kueue Workloads:",
		"  (none — Kueue did not see this workload; check the queue label)",
		"",
		"Pods:",
		"  (none — workload is suspended or not yet admitted)",
		"",
	}, "\n")
	if got := Render(snap); got != want {
		t.Fatalf("non-MultiKueue output changed:\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

func TestRenderStartupPhases_ShowsDRAImageAndContainerProgress(t *testing.T) {
	now := mustTime("2026-04-27T10:05:00Z")
	created := now.Add(-90 * time.Second)
	out := renderStartupPhasesAt(Snapshot{
		Name: "train-001", Namespace: "ray",
		JobFound:     true,
		JobCreatedAt: created,
		Workloads:    []Workload{{Name: "train-001", Queue: "sample-h100", Phase: "Admitted", Admitted: true}},
		Pods: []Pod{{
			Name:           "train-001-abc",
			CreatedAt:      created.Add(10 * time.Second),
			Phase:          "Pending",
			Node:           "node-a",
			Ready:          "0/1",
			ResourceClaims: []string{"train-001-gpu-w4wlp"},
			Containers: []Container{{
				Name:   "main",
				Image:  "nvcr.io/nvidia/cuda:13.0.0-devel-ubuntu24.04",
				State:  "waiting",
				Reason: "ContainerCreating",
			}},
		}},
		ResourceClaims: []ResourceClaim{{
			Name:       "train-001-gpu-w4wlp",
			CreatedAt:  created.Add(12 * time.Second),
			LastReason: "NotAllocated",
		}},
		Events: []Event{{
			InvolvedKind: "Pod",
			InvolvedName: "train-001-abc",
			Reason:       "Pulling",
			Message:      "Pulling image \"nvcr.io/nvidia/cuda:13.0.0-devel-ubuntu24.04\"",
			FirstSeen:    created.Add(20 * time.Second),
			LastSeen:     created.Add(20 * time.Second),
		}},
	}, now)
	for _, want := range []string{
		"[x]  Submitted",
		"[x]  Kueue admission",
		"[x]  Pod scheduling",
		"[>]  DRA allocation",
		"NotAllocated",
		"[>]  Image pull",
		"Pulling image",
		"[>]  Container start",
		"[>]  Ready",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup phases missing %q:\n%s", want, out)
		}
	}
}

func TestRenderStartupPhases_ReadyJobCompletesStartup(t *testing.T) {
	now := mustTime("2026-04-27T10:05:00Z")
	created := now.Add(-2 * time.Minute)
	snap := Snapshot{
		Name: "train-001", Namespace: "ray",
		JobFound:     true,
		JobCreatedAt: created,
		Workloads:    []Workload{{Name: "train-001", Queue: "sample-h100", Phase: "Admitted", Admitted: true}},
		Pods: []Pod{{
			Name:      "train-001-abc",
			CreatedAt: created.Add(10 * time.Second),
			Phase:     "Running",
			Node:      "node-a",
			Ready:     "1/1",
			Conditions: []Condition{{
				Type:   "Ready",
				Status: "True",
			}},
			ResourceClaims: []string{"train-001-gpu-w4wlp"},
			Containers: []Container{{
				Name:      "main",
				Image:     "acr.io/train:v1",
				State:     "running",
				Ready:     true,
				StartedAt: created.Add(40 * time.Second),
			}},
		}},
		ResourceClaims: []ResourceClaim{{
			Name:       "train-001-gpu-w4wlp",
			CreatedAt:  created.Add(12 * time.Second),
			Allocated:  true,
			Allocation: "pool-a/gpu-0",
		}},
		Events: []Event{{
			InvolvedKind: "Pod",
			InvolvedName: "train-001-abc",
			Reason:       "Pulled",
			Message:      "Successfully pulled image \"acr.io/train:v1\"",
			FirstSeen:    created.Add(20 * time.Second),
			LastSeen:     created.Add(20 * time.Second),
		}},
	}
	out := renderStartupPhasesAt(snap, now)
	for _, want := range []string{
		"[x]  DRA allocation",
		"pool-a/gpu-0",
		"[x]  Image pull",
		"[x]  Container start",
		"[x]  Ready",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup phases missing %q:\n%s", want, out)
		}
	}
	if !StartupComplete(snap) {
		t.Fatalf("expected startup complete:\n%s", out)
	}
}

func TestRenderStartupPhases_MultiKueueNoLocalPodsSkipsPodPhases(t *testing.T) {
	out := renderStartupPhasesAt(Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		Workloads: []Workload{{
			Name:                  "train-001",
			Queue:                 "sample-h100",
			ClusterName:           "worker-a",
			NominatedClusterNames: []string{"worker-a", "worker-b"},
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Selected", Message: "worker-a selected", ControllerName: multiKueueControllerName},
			},
		}},
	}, mustTime("2026-07-09T10:05:00Z"))
	for _, want := range []string{
		"MultiKueue placement",
		"selected worker cluster worker-a",
		"[-]  Pod scheduling",
		"manager view only; worker-cluster pod lifecycle is not visible here",
		"[-]  Ready",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup phases missing %q:\n%s", want, out)
		}
	}
}

func TestRenderStartupPhases_MultiKueueWithLocalPodsMatchesOrdinaryTreeExact(t *testing.T) {
	now := mustTime("2026-07-09T10:05:00Z")
	created := now.Add(-time.Minute)
	base := Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		Workloads: []Workload{{
			Name:  "train-001",
			Queue: "sample-h100",
		}},
		Pods: []Pod{{
			Name:      "train-001-head",
			CreatedAt: created,
			Phase:     "Running",
			Node:      "node-a",
			Ready:     "1/1",
			Conditions: []Condition{{
				Type:   "Ready",
				Status: "True",
			}},
			Containers: []Container{{
				Name:      "main",
				State:     "running",
				Ready:     true,
				StartedAt: created.Add(10 * time.Second),
			}},
		}},
	}
	multiKueue := Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name:        "train-001",
			Queue:       "sample-h100",
			ClusterName: "worker-a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: multiKueueControllerName},
			},
		}},
		Pods: []Pod{{
			Name:      "train-001-head",
			CreatedAt: created,
			Phase:     "Running",
			Node:      "node-a",
			Ready:     "1/1",
			Conditions: []Condition{{
				Type:   "Ready",
				Status: "True",
			}},
			Containers: []Container{{
				Name:      "main",
				State:     "running",
				Ready:     true,
				StartedAt: created.Add(10 * time.Second),
			}},
		}},
	}
	if got, want := renderStartupPhasesAt(multiKueue, now), renderStartupPhasesAt(base, now); got != want {
		t.Fatalf("local-pod MultiKueue startup tree must match ordinary output exactly:\n--- ordinary ---\n%s\n--- multikueue ---\n%s", want, got)
	}
}

func TestRender_MultiKueueWithLocalPodsKeepsRunningHeadline(t *testing.T) {
	out := Render(Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		JobFound:  true,
		JobActive: 1,
		Workloads: []Workload{{
			Name:        "train-001",
			ClusterName: "worker-a",
		}},
		Pods: []Pod{{
			Name:  "train-001-head",
			Phase: "Running",
			Ready: "1/1",
			Node:  "node-a",
		}},
	})
	if !strings.Contains(out, "state:     Running") {
		t.Fatalf("expected running headline once local execution is visible:\n%s", out)
	}
}

func TestDeriveState_MultiKueueTerminalPrecedence(t *testing.T) {
	tests := []struct {
		name string
		snap Snapshot
		want string
	}{
		{
			name: "failed beats ready",
			snap: Snapshot{
				JobFound: true,
				JobConditions: []Condition{
					{Type: "Failed", Status: "True"},
				},
				Workloads: []Workload{{
					Name: "w",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: "Failed",
		},
		{
			name: "complete beats rejected",
			snap: Snapshot{
				JobFound: true,
				JobConditions: []Condition{
					{Type: "Complete", Status: "True"},
				},
				Workloads: []Workload{{
					Name: "w",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Rejected", Message: "quota exceeded", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: "Complete",
		},
		{
			name: "manager-only running beats ready placement",
			snap: Snapshot{
				JobFound:     true,
				JobActive:    1,
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					Name:        "w",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: "Running",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := deriveState(tt.snap); got != tt.want {
				t.Fatalf("deriveState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWatchState_MultiKueueManagerView(t *testing.T) {
	tests := []struct {
		name         string
		snap         Snapshot
		wantComplete bool
		wantFailed   bool
	}{
		{
			name: "selected keeps watching",
			snap: Snapshot{
				JobFound: true,
				Workloads: []Workload{{
					Name:        "w",
					ClusterName: "worker-a",
				}},
			},
		},
		{
			name: "ready succeeds without local pods",
			snap: Snapshot{
				JobFound:     true,
				JobActive:    1,
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					Name:        "w",
					Admitted:    true,
					Phase:       "Admitted",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
			wantComplete: true,
		},
		{
			name: "mixed ready and empty workload keeps watching",
			snap: Snapshot{
				JobFound:     true,
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{
					{
						Name:        "a",
						ClusterName: "worker-a",
						AdmissionChecks: []AdmissionCheck{
							{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
						},
					},
					{Name: "b"},
				},
			},
		},
		{
			name: "finished workload completes manager-only batch watch",
			snap: Snapshot{
				JobFound: true,
				Workloads: []Workload{{
					Name:        "w",
					Phase:       "Finished",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
			wantComplete: true,
		},
		{
			name: "rejected fails without local pods",
			snap: Snapshot{
				JobFound:     true,
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					Name: "w",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Rejected", Message: "quota exceeded", ControllerName: multiKueueControllerName},
					},
				}},
			},
			wantFailed: true,
		},
		{
			name: "rayjob running without placement completes",
			snap: Snapshot{
				RayJob: RayJob{
					Found:               true,
					ManagedBy:           multiKueueManagedBy,
					JobDeploymentStatus: "Running",
				},
			},
			wantComplete: true,
		},
		{
			name: "rayjob suspended without placement keeps watching",
			snap: Snapshot{
				RayJob: RayJob{
					Found:               true,
					ManagedBy:           multiKueueManagedBy,
					JobDeploymentStatus: "Suspended",
				},
			},
		},
		{
			name: "rayjob suspended with ready placement keeps watching",
			snap: Snapshot{
				RayJob: RayJob{
					Found:               true,
					ManagedBy:           multiKueueManagedBy,
					JobDeploymentStatus: "Suspended",
				},
				Workloads: []Workload{{
					Name:        "w",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
		},
		{
			name: "ready placement plus generic pending keeps watching",
			snap: Snapshot{
				JobFound:     true,
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					Name:        "w",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
						{Name: "quota-check", State: "Pending", ControllerName: "kueue.x-k8s.io/provisioning"},
					},
				}},
			},
		},
		{
			name: "ready placement plus generic rejected fails",
			snap: Snapshot{
				JobFound:     true,
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					Name:        "w",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
						{Name: "quota-check", State: "Rejected", ControllerName: "kueue.x-k8s.io/provisioning"},
					},
				}},
			},
			wantFailed: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := WatchComplete(tt.snap); got != tt.wantComplete {
				t.Fatalf("WatchComplete() = %v, want %v", got, tt.wantComplete)
			}
			if got := WatchFailed(tt.snap); got != tt.wantFailed {
				t.Fatalf("WatchFailed() = %v, want %v", got, tt.wantFailed)
			}
		})
	}
}

func TestStartupState_MultiKueueReadyManagerViewDoesNotCountAsCompletion(t *testing.T) {
	snap := Snapshot{
		JobFound: true,
		Workloads: []Workload{{
			Name:        "w",
			Admitted:    true,
			Phase:       "Admitted",
			ClusterName: "worker-a",
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
			},
		}},
	}
	if StartupComplete(snap) {
		t.Fatal("manager-only MultiKueue placement ready must not look like workload completion")
	}
}

func TestWorkloadFailed_MultiKueueRejectedManagerView(t *testing.T) {
	snap := Snapshot{
		JobFound:     true,
		JobManagedBy: multiKueueManagedBy,
		Workloads: []Workload{{
			Name: "w",
			AdmissionChecks: []AdmissionCheck{
				{Name: "multikueue", State: "Rejected", Message: "quota exceeded", ControllerName: multiKueueControllerName},
			},
		}},
	}
	if !WorkloadFailed(snap) {
		t.Fatal("manager-only MultiKueue rejection must be terminal for workload failure checks")
	}
}

func TestExperimentRunState_MultiKueueMappings(t *testing.T) {
	tests := []struct {
		name string
		snap Snapshot
		want string
	}{
		{
			name: "rejected remains pending for expstore mapping",
			snap: Snapshot{
				JobFound: true,
				Workloads: []Workload{{
					Name:            "w",
					AdmissionChecks: []AdmissionCheck{{Name: "multikueue", State: "Rejected", ControllerName: multiKueueControllerName}},
				}},
			},
			want: "pending",
		},
		{
			name: "ready remains pending for expstore mapping",
			snap: Snapshot{
				JobFound: true,
				Workloads: []Workload{{
					Name:        "w",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: "pending",
		},
		{
			name: "manager-only running maps to running",
			snap: Snapshot{
				JobFound:     true,
				JobActive:    1,
				JobManagedBy: multiKueueManagedBy,
				Workloads: []Workload{{
					Name:        "w",
					ClusterName: "worker-a",
					AdmissionChecks: []AdmissionCheck{
						{Name: "multikueue", State: "Ready", ControllerName: multiKueueControllerName},
					},
				}},
			},
			want: "running",
		},
		{
			name: "selected stays pending",
			snap: Snapshot{
				JobFound: true,
				Workloads: []Workload{{
					Name:        "w",
					ClusterName: "worker-a",
				}},
			},
			want: "pending",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := experimentRunState(tt.snap); got != tt.want {
				t.Fatalf("experimentRunState() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRenderStartupPhases_DoesNotCompleteWhenReferencedDRAClaimMissing(t *testing.T) {
	now := mustTime("2026-04-27T10:05:00Z")
	created := now.Add(-2 * time.Minute)
	snap := Snapshot{
		Name: "train-001", Namespace: "ray",
		JobFound:     true,
		JobCreatedAt: created,
		Workloads:    []Workload{{Name: "train-001", Queue: "sample-h100", Phase: "Admitted", Admitted: true}},
		Pods: []Pod{{
			Name:           "train-001-abc",
			CreatedAt:      created.Add(10 * time.Second),
			Phase:          "Running",
			Node:           "node-a",
			Ready:          "1/1",
			ResourceClaims: []string{"train-001-gpu-a", "train-001-gpu-b"},
			Conditions: []Condition{{
				Type:   "Ready",
				Status: "True",
			}},
			Containers: []Container{{
				Name:  "main",
				State: "running",
				Ready: true,
			}},
		}},
		ResourceClaims: []ResourceClaim{{
			Name:       "train-001-gpu-a",
			CreatedAt:  created.Add(12 * time.Second),
			Allocated:  true,
			Allocation: "pool-a/gpu-0",
		}},
	}
	out := renderStartupPhasesAt(snap, now)
	for _, want := range []string{
		"[>]  DRA allocation",
		"1/2 allocated",
		"train-001-gpu-b=not observed",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup phases missing %q:\n%s", want, out)
		}
	}
	if StartupComplete(snap) {
		t.Fatalf("startup should not complete while a referenced ResourceClaim is not observed:\n%s", out)
	}
}

func TestStartupFailedIgnoresHistoricalContainerFailureOnRunningContainer(t *testing.T) {
	if StartupFailed(Snapshot{
		Pods: []Pod{{
			Name:  "train-001-abc",
			Phase: "Running",
			Containers: []Container{{
				Name:       "main",
				State:      "running",
				Ready:      true,
				Reason:     "Error",
				LastReason: "Error",
			}},
		}},
	}) {
		t.Fatal("running container with historical Error reason must not make startup terminally failed")
	}
}

func TestStartupCompleteAcceptsSucceededRayJobAfterPodCleanup(t *testing.T) {
	snap := Snapshot{
		Name: "ray-train", Namespace: "ray",
		RayJob: RayJob{
			Found:               true,
			Name:                "ray-train",
			RayClusterName:      "ray-train-cluster",
			JobDeploymentStatus: "Complete",
		},
	}
	if !StartupComplete(snap) {
		t.Fatal("succeeded RayJob should complete watch even after KubeRay deletes pods")
	}
}

func TestRenderSuccessfulRayJobTeardownUsesTerminalSuccessPrecedence(t *testing.T) {
	finished := mustTime("2026-07-14T06:51:46Z")
	snap := Snapshot{
		Name:      "tau-vit-enc-vision-smoke",
		Namespace: "taugrid-e2e",
		RayJob: RayJob{
			Found:               true,
			Name:                "tau-vit-enc-vision-smoke",
			RayClusterName:      "tau-vit-enc-vision-smoke-6wtnp",
			JobDeploymentStatus: "Complete",
			JobStatus:           "SUCCEEDED",
			FinishedAt:          finished,
			Reason:              "Job finished successfully.",
		},
		Workloads: []Workload{{
			Name:     "rayjob-tau-vit-enc-vision-smoke-38b7e",
			Queue:    "jobqueue",
			Admitted: true,
			Phase:    "Finished",
			Reason:   "Succeeded",
		}},
		Pods: []Pod{{
			Name:            "tau-vit-enc-vision-smoke-6wtnp-head-b5bnt",
			CreatedAt:       finished.Add(-time.Minute),
			Phase:           "Failed",
			Node:            "cpu-node",
			Ready:           "0/2",
			ContainerReason: "Error",
			Containers: []Container{{
				Name:     "ray-head",
				State:    "terminated",
				Reason:   "Error",
				ExitCode: int32Ptr(137),
			}},
		}},
	}

	out := Render(snap)
	for _, want := range []string{
		"status:    Complete",
		"1/1 finished; quota released",
		"[x]  Container start",
		"[x]  Ready",
		"post-run RayCluster teardown",
		"[x]  Compute release",
		"quota released; resources reusable",
		"tau-vit-enc-vision-smoke-6wtnp-head-b5bnt  Teardown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("successful teardown status missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "[!]") {
		t.Fatalf("successful terminal RayJob must not render startup warnings:\n%s", out)
	}

	snap.Pods = nil
	snap.PodsObserved = true
	out = Render(snap)
	if !strings.Contains(out, "(none — RayCluster pods cleaned up after successful completion)") {
		t.Fatalf("successful terminal RayJob without pods should explain cleanup:\n%s", out)
	}
	if strings.Contains(out, "suspended or not yet admitted") {
		t.Fatalf("successful terminal RayJob without pods must not look pending:\n%s", out)
	}
}

func TestRenderSuccessfulRayJobDoesNotClaimReuseWhenPodsUnavailable(t *testing.T) {
	finished := mustTime("2026-08-04T20:00:00Z")
	out := renderStartupPhasesAt(Snapshot{
		Name:      "whole-node-h200",
		Namespace: "tau",
		RayJob: RayJob{
			Found:               true,
			Name:                "whole-node-h200",
			JobDeploymentStatus: "Complete",
			JobStatus:           "SUCCEEDED",
			FinishedAt:          finished,
		},
		Workloads: []Workload{{Name: "rayjob-whole-node-h200", Phase: "Finished"}},
	}, finished.Add(8*time.Second))

	for _, want := range []string{
		"[!]  Compute release",
		"Ray pod state unavailable; physical resource reusability unknown",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("unavailable pod status missing %q:\n%s", want, out)
		}
	}
}

func TestRenderSuccessfulRayJobDistinguishesQuotaFromComputeRelease(t *testing.T) {
	finished := mustTime("2026-08-04T20:00:00Z")
	out := renderStartupPhasesAt(Snapshot{
		Name:      "whole-node-h200",
		Namespace: "tau",
		RayJob: RayJob{
			Found:               true,
			Name:                "whole-node-h200",
			RayClusterName:      "whole-node-h200-cluster",
			JobDeploymentStatus: "Complete",
			JobStatus:           "SUCCEEDED",
			FinishedAt:          finished,
		},
		Workloads: []Workload{{
			Name:  "rayjob-whole-node-h200",
			Queue: "jobqueue",
			Phase: "Finished",
		}},
		Pods: []Pod{
			{Name: "head", Phase: "Running", Node: "h200-a"},
			{Name: "worker", Phase: "Running", Node: "h200-b"},
		},
	}, finished.Add(8*time.Second))

	for _, want := range []string{
		"[x]  Kueue admission",
		"1/1 finished; quota released",
		"[>]  Compute release",
		"quota released; resources NOT reusable; 2 Ray pod(s) still hold 2 node(s): h200-a, h200-b",
		"wait for KubeRay to delete the RayCluster",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("teardown status missing %q:\n%s", want, out)
		}
	}
}

func TestRenderManagerRayJobDoesNotClaimWorkerComputeReleased(t *testing.T) {
	finished := mustTime("2026-08-04T20:00:00Z")
	out := renderStartupPhasesAt(Snapshot{
		Name:      "multikueue-h200",
		Namespace: "tau",
		RayJob: RayJob{
			Found:               true,
			Name:                "multikueue-h200",
			ManagedBy:           "kueue.x-k8s.io/multikueue",
			JobDeploymentStatus: "Complete",
			JobStatus:           "SUCCEEDED",
			FinishedAt:          finished,
		},
		Workloads: []Workload{{
			Name:        "rayjob-multikueue-h200",
			Phase:       "Finished",
			ClusterName: "worker-h200",
		}},
	}, finished.Add(8*time.Second))

	for _, want := range []string{
		"[-]  Compute release",
		"manager view only; Kueue quota can be released before worker-cluster GPUs are reusable",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("manager teardown status missing %q:\n%s", want, out)
		}
	}
}

func TestRenderRayJobFailureBeforeCompletionRemainsVisible(t *testing.T) {
	snap := Snapshot{
		Name:      "tau-broken",
		Namespace: "ray",
		RayJob: RayJob{
			Found:               true,
			Name:                "tau-broken",
			RayClusterName:      "tau-broken-cluster",
			JobDeploymentStatus: "Running",
			JobStatus:           "RUNNING",
		},
		Pods: []Pod{{
			Name:            "tau-broken-worker",
			Phase:           "Failed",
			Ready:           "0/1",
			ContainerReason: "Error",
			Containers: []Container{{
				Name:     "worker",
				State:    "terminated",
				Reason:   "Error",
				ExitCode: int32Ptr(1),
			}},
		}},
	}

	out := Render(snap)
	for _, want := range []string{
		"[!]  Container start",
		"[!]  Ready",
		"tau-broken-worker  Failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("pre-success RayJob failure missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Teardown") {
		t.Fatalf("pre-success failure must not be relabeled as teardown:\n%s", out)
	}
}

func TestRenderFailedRayJobAfterPodCleanupDoesNotLookPending(t *testing.T) {
	finished := mustTime("2026-08-06T17:00:00Z")
	out := Render(Snapshot{
		Name:         "failed-train",
		Namespace:    "ray",
		PodsObserved: true,
		RayJob: RayJob{
			Found:               true,
			Name:                "failed-train",
			RayClusterName:      "failed-train-cluster",
			JobDeploymentStatus: "Failed",
			JobStatus:           "FAILED",
			FinishedAt:          finished,
			Reason:              "AppFailed",
			Message:             "entrypoint exited with code 1",
		},
		Workloads: []Workload{{
			Name:     "rayjob-failed-train",
			Queue:    "jobqueue",
			Admitted: true,
			Phase:    "Finished",
			Reason:   "Failed",
		}},
	})

	for _, want := range []string{
		"status:    Failed",
		"[!]  RayCluster",
		"[-]  Pod scheduling",
		"RayJob failed; post-run RayCluster teardown removed pod evidence",
		"[!]  RayJob status",
		"[x]  Compute release",
		"(none — RayCluster pods cleaned up after terminal failure)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("failed terminal RayJob output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{
		"no pods observed yet",
		"no ResourceClaims requested",
		"no init containers",
		"waiting for pods",
		"workload is suspended or not yet admitted",
	} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("failed terminal RayJob output retained pending message %q:\n%s", unwanted, out)
		}
	}
}

func TestRenderFailedRayJobWithUnavailablePodStateDoesNotInferLifecycle(t *testing.T) {
	out := Render(Snapshot{
		Name:      "failed-train",
		Namespace: "ray",
		RayJob: RayJob{
			Found:               true,
			Name:                "failed-train",
			RayClusterName:      "failed-train-cluster",
			JobDeploymentStatus: "Failed",
			JobStatus:           "FAILED",
		},
	})
	for _, want := range []string{
		"[-]  Pod scheduling",
		"pod state unavailable after terminal RayJob failure",
		"(none — pod state unavailable for failed RayJob)",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("failed RayJob with unavailable pod state missing %q:\n%s", want, out)
		}
	}
	for _, unsupported := range []string{"no pods observed yet", "no ResourceClaims requested", "no init containers", "waiting for pods"} {
		if strings.Contains(out, unsupported) {
			t.Fatalf("failed RayJob with unavailable pod state inferred %q:\n%s", unsupported, out)
		}
	}
}

func TestStartupFailedStopsOnCurrentImagePullFailure(t *testing.T) {
	snap := Snapshot{
		Pods: []Pod{{
			Name:  "train-001-abc",
			Phase: "Pending",
			Containers: []Container{{
				Name:   "main",
				State:  "waiting",
				Reason: "ImagePullBackOff",
			}},
		}},
	}
	if !StartupFailed(snap) {
		t.Fatal("current ImagePullBackOff should make watch exit failed")
	}
}

func TestStartupFailedDoesNotStopRetryingBatchJobOnFailedPod(t *testing.T) {
	snap := Snapshot{
		JobFound: true,
		Pods: []Pod{{
			Name:  "train-001-retry-1",
			Phase: "Failed",
			Containers: []Container{{
				Name:     "main",
				State:    "terminated",
				Reason:   "Error",
				ExitCode: int32Ptr(1),
			}},
		}},
	}
	if StartupFailed(snap) {
		t.Fatal("batch Job watch should wait for the authoritative Job Failed condition before exiting failed")
	}
	snap.JobConditions = []Condition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}}
	if !StartupFailed(snap) {
		t.Fatal("batch Job Failed=True condition should make watch exit failed")
	}
}

func TestStartupFailedStopsOnImagePullFailureEvenWhenJobExists(t *testing.T) {
	snap := Snapshot{
		JobFound: true,
		Pods: []Pod{{
			Name:  "train-001-abc",
			Phase: "Pending",
			Containers: []Container{{
				Name:   "main",
				State:  "waiting",
				Reason: "ImagePullBackOff",
			}},
		}},
	}
	if !StartupFailed(snap) {
		t.Fatal("current image pull failures should stop watch even while the Job object still exists")
	}
}

func TestStartupFailedTreatsStoppedRayJobAsTerminalFailure(t *testing.T) {
	snap := Snapshot{
		RayJob: RayJob{
			Found:               true,
			JobDeploymentStatus: "Complete",
			JobStatus:           "STOPPED",
		},
	}
	if StartupComplete(snap) {
		t.Fatal("STOPPED RayJob must not be treated as successful startup completion")
	}
	if !StartupFailed(snap) {
		t.Fatal("STOPPED RayJob should make watch exit failed")
	}
}

func int32Ptr(value int32) *int32 {
	return &value
}

func TestRenderStartupPhases_RayJobIncludesClusterAndSubmitter(t *testing.T) {
	now := mustTime("2026-04-27T10:05:00Z")
	created := now.Add(-time.Minute)
	out := renderStartupPhasesAt(Snapshot{
		Name: "ray-train", Namespace: "ray",
		RayJob: RayJob{
			Found:               true,
			Name:                "ray-train",
			CreatedAt:           created,
			RayClusterName:      "ray-train-cluster",
			JobDeploymentStatus: "Running",
			JobID:               "raysubmit_123",
		},
		Workloads: []Workload{{Name: "ray-train", Queue: "sample-h100", Phase: "Admitted", Admitted: true}},
		Pods: []Pod{{
			Name:      "ray-train-head",
			CreatedAt: created.Add(5 * time.Second),
			Phase:     "Running",
			Node:      "node-a",
			Ready:     "1/1",
			Conditions: []Condition{{
				Type:   "Ready",
				Status: "True",
			}},
			Containers: []Container{{
				Name:  "ray-head",
				State: "running",
				Ready: true,
			}},
		}},
	}, now)
	for _, want := range []string{
		"[x]  RayCluster",
		"ray-train-cluster",
		"[x]  RayJob status",
		"Running",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("startup phases missing %q:\n%s", want, out)
		}
	}
}

func TestRender_Failed_BeatsComplete(t *testing.T) {
	out := Render(Snapshot{
		Name: "j", Namespace: "tau", JobFound: true,
		JobConditions: []Condition{
			{Type: "Complete", Status: "True"},
			{Type: "Failed", Status: "True", Reason: "DeadlineExceeded"},
		},
	})
	if !strings.Contains(out, "state:     Failed") {
		t.Errorf("Failed must beat Complete in state headline:\n%s", out)
	}
	if !strings.Contains(out, "DeadlineExceeded") {
		t.Errorf("expected reason in conditions section:\n%s", out)
	}
}

func TestRender_NoWorkloads_HintsAboutQueueLabel(t *testing.T) {
	out := Render(Snapshot{Name: "j", Namespace: "tau", JobFound: true})
	if !strings.Contains(out, "Kueue did not see this workload") {
		t.Errorf("expected Kueue-not-seen hint, got:\n%s", out)
	}
}

func TestRenderRunProfile(t *testing.T) {
	created := mustTime("2026-04-27T10:00:00Z")
	started := mustTime("2026-04-27T10:02:00Z")
	finished := mustTime("2026-04-27T10:32:00Z")
	out := RenderRunProfile(Snapshot{
		Name:          "train-001",
		Namespace:     "ray",
		Labels:        map[string]string{workloadmeta.LabelRunID: "train-001", workloadmeta.LabelWorkloadKind: "job", workloadmeta.LabelProfile: "research-train-gpu", workloadmeta.LabelPreset: "azure.research.training.xl", "kueue.x-k8s.io/queue-name": "training-queue", workloadmeta.LabelTeam: "research", workloadmeta.LabelLane: "training", workloadmeta.LabelTopology: "single-node-nvlink", workloadmeta.LabelGPUClass: "a100-nvlink-80gb"},
		Annotations:   map[string]string{workloadmeta.AnnotationCaptureVersion: "v1alpha1", workloadmeta.AnnotationNamespace: "ray", workloadmeta.AnnotationTauCommand: "tau submit train-001", workloadmeta.AnnotationImage: "acr.io/train:v1", workloadmeta.AnnotationConfigHash: "abc123", workloadmeta.AnnotationGPUCount: "8", workloadmeta.AnnotationDRAClaim: "ds-8gpus", workloadmeta.AnnotationStorageMounts: `[{"source":"pvc","path":"/data","source_ref":"training-nfs"}]`, workloadmeta.AnnotationResultPath: "/data/evals/train-001", workloadmeta.AnnotationResultArtifacts: "metrics.json, track.png", workloadmeta.AnnotationPresetExplain: "A100 NVLink protected queue"},
		JobFound:      true,
		JobCreatedAt:  created,
		JobStartedAt:  started,
		JobFinishedAt: finished,
		Workloads:     []Workload{{Name: "w", Queue: "training-queue", Phase: "Finished", Admitted: true}},
		Pods:          []Pod{{Name: "p", UID: "pod-uid", Node: "node-a", Phase: "Succeeded", Ready: "0/1", StartedAt: started.Add(30 * time.Second), ResourceClaims: []string{"claim-a"}, ContainerReason: "Completed"}},
	}, CostProfile{
		Profile:    "research-train-gpu",
		GPUType:    "h100",
		GPUsPerPod: 1,
		Pods:       1,
		Hours:      0.5,
		UsdPerHour: 6.98,
		TotalUsd:   3.49,
	})
	for _, want := range []string{
		"Run profile:",
		"queue_wait",
		"queue_wait_seconds",
		"120",
		"2m00s",
		"run_id",
		"workload_kind",
		"capture_version",
		"v1alpha1",
		"tau submit train-001",
		"acr.io/train:v1",
		"config_hash",
		"abc123",
		"gpu_count",
		"8",
		"ds-8gpus",
		"storage_mounts",
		"kueue_workload",
		"pod_uid",
		"pod-uid",
		"resource_claims",
		"claim-a",
		"container_reason",
		"Completed",
		"runtime_seconds",
		"1800",
		"phase=Finished admitted=true queue=training-queue",
		"research",
		"azure.research.training.xl",
		"single-node-nvlink",
		"a100-80gb",
		"30m00s (finished)",
		"1 pod(s) x 1 x h100 = 1 GPU(s)",
		"$3.49 total",
		"/data/evals/train-001",
		"metrics.json, track.png",
		"A100 NVLink protected queue",
		"gpu_utilization",
		"not collected",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("run profile missing %q:\n%s", want, out)
		}

	}
}

func TestExperimentRunProfileGPUClassContract(t *testing.T) {
	for _, tc := range []struct {
		name      string
		label     string
		costType  string
		wantClass string
	}{
		{name: "legacy alias", label: "h100-standalone-95gb", wantClass: "h100-95gb"},
		{name: "explicit any", label: "any", wantClass: "any"},
		{name: "missing label does not infer from cost", costType: "h100", wantClass: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			labels := map[string]string{workloadmeta.LabelRunID: "run-1"}
			if tc.label != "" {
				labels[workloadmeta.LabelGPUClass] = tc.label
			}
			record, err := ExperimentRunProfile(
				Snapshot{Name: "run-1", Namespace: "ray", JobFound: true, Labels: labels},
				CostProfile{GPUType: tc.costType},
				ExperimentRunDataOptions{},
			)
			if err != nil {
				t.Fatal(err)
			}
			if record.GPUClass != tc.wantClass {
				t.Fatalf("GPUClass = %q, want %q", record.GPUClass, tc.wantClass)
			}
		})
	}
}

func TestRender_RayJob_Running(t *testing.T) {
	out := Render(Snapshot{
		Name: "train-llm", Namespace: "ray",
		RayJobFound:    true,
		RayJobStatus:   "Running",
		RayClusterName: "train-llm-raycluster-abc",
		RayJobID:       "rayjob-train-llm-12345",
		Pods: []Pod{
			{Name: "train-llm-head-xyz", Phase: "Running", Node: "gpu-node-1", Ready: "1/1"},
			{Name: "train-llm-worker-0", Phase: "Running", Node: "gpu-node-2", Ready: "1/1"},
		},
	})
	if !strings.Contains(out, "RayJob: ray/train-llm") {
		t.Errorf("expected RayJob header, got:\n%s", out)
	}
	if !strings.Contains(out, "status:    Running") {
		t.Errorf("expected Running status, got:\n%s", out)
	}
	if !strings.Contains(out, "cluster:   train-llm-raycluster-abc") {
		t.Errorf("expected cluster name, got:\n%s", out)
	}
	if !strings.Contains(out, "jobId:     rayjob-train-llm-12345") {
		t.Errorf("expected jobId, got:\n%s", out)
	}
	if strings.Contains(out, "Job:") && !strings.Contains(out, "RayJob:") {
		t.Errorf("should say RayJob, not Job:\n%s", out)
	}
	if !strings.Contains(out, "train-llm-head-xyz") {
		t.Errorf("expected pods in output:\n%s", out)
	}
}

func TestRender_RayJob_Failed_WithReason(t *testing.T) {
	out := Render(Snapshot{
		Name: "broken", Namespace: "ray",
		RayJobFound:  true,
		RayJobStatus: "Failed",
		RayJobReason: "SubmissionFailed",
	})
	if !strings.Contains(out, "RayJob: ray/broken") {
		t.Errorf("expected RayJob header:\n%s", out)
	}
	if !strings.Contains(out, "status:    Failed") {
		t.Errorf("expected Failed status:\n%s", out)
	}
	if !strings.Contains(out, "reason:    SubmissionFailed") {
		t.Errorf("expected reason:\n%s", out)
	}
}

func TestRender_RayJob_Complete_NoJobSection(t *testing.T) {
	out := Render(Snapshot{
		Name: "done", Namespace: "ray",
		RayJobFound:    true,
		RayJobStatus:   "Complete",
		RayClusterName: "done-raycluster-xyz",
	})
	if !strings.Contains(out, "RayJob: ray/done") {
		t.Errorf("expected RayJob header:\n%s", out)
	}
	if strings.Contains(out, "no batch/v1 Job") {
		t.Errorf("should not show batch Job not-found when RayJob exists:\n%s", out)
	}
}

func TestDeriveState_RayJob(t *testing.T) {
	tests := []struct {
		status string
		want   string
	}{
		{"Running", "Running"},
		{"Complete", "Complete"},
		{"Failed", "Failed"},
		{"New", "New"},
		{"Initializing", "Initializing"},
		{"Suspended", "Pending (suspended; Kueue not yet admitted)"},
	}
	for _, tt := range tests {
		got := deriveState(Snapshot{RayJobFound: true, RayJobStatus: tt.status})
		if got != tt.want {
			t.Errorf("deriveState(RayJob %s) = %q, want %q", tt.status, got, tt.want)
		}
	}
}

func TestRenderRunProfile_RayJob_CompletedDurationFixed(t *testing.T) {
	created := mustTime("2026-04-27T10:00:00Z")
	started := mustTime("2026-04-27T10:02:00Z")
	finished := mustTime("2026-04-27T10:32:00Z")
	out := RenderRunProfile(Snapshot{
		Name:             "ray-train-002",
		Namespace:        "ray",
		Labels:           map[string]string{workloadmeta.LabelRunID: "ray-train-002"},
		RayJobFound:      true,
		RayJobStatus:     "Complete",
		RayJobCreatedAt:  created,
		RayJobStartedAt:  started,
		RayJobFinishedAt: finished,
	}, CostProfile{})
	if !strings.Contains(out, "30m00s (finished)") {
		t.Errorf("expected fixed 30m00s duration for completed RayJob, got:\n%s", out)
	}
	if !strings.Contains(out, "runtime_seconds") && !strings.Contains(out, "1800") {
		t.Errorf("expected runtime_seconds=1800 for completed RayJob, got:\n%s", out)
	}
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestRender_NotAdmittedWorkloadShowsKueueMessageWithoutBreakingColumns(t *testing.T) {
	out := Render(Snapshot{
		Name: "taudemo-train", Namespace: "demo",
		JobFound: true, JobSuspended: true,
		Workloads: []Workload{
			{
				Name: "job-taudemo-train-d6b8e", Queue: "jobqueue", Phase: "Pending",
				Reason:  "Pending",
				Message: "couldn't assign flavors to pod set main:\ntopology \"default-node-topology\" doesn't allow to fit any of 1 pod(s).\nTotal nodes: 4; excluded: resource \"cpu\": 3",
			},
			{Name: "job-other", Queue: "jobqueue", Phase: "Admitted", Admitted: true},
		},
	})

	if !strings.Contains(out, "excluded: resource \"cpu\": 3") {
		t.Errorf("expected Kueue message detail in output, got:\n%s", out)
	}
	// Newlines in the message would split the tabwriter row.
	if strings.Contains(out, "└─ couldn't assign flavors to pod set main:\n") {
		t.Errorf("message was not collapsed to a single line, got:\n%s", out)
	}

	// The long message must not break the table. A continuation line with
	// too few tabs ends tabwriter's column block, and every row after it
	// re-aligns to its own widths. Checking one named column would miss
	// that: the drift lands in the columns *after* the break, so a column
	// chosen up front stays put while the table is visibly broken. Assert
	// instead that every row still starts each of its cells at the header's
	// column offsets — true regardless of how many columns the table grows.
	var header, pending, admitted string
	for _, line := range strings.Split(out, "\n") {
		switch {
		case strings.Contains(line, "NAME") && strings.Contains(line, "ADMITTED"):
			header = line
		case strings.HasPrefix(line, "  job-taudemo-train-d6b8e "):
			pending = line
		case strings.HasPrefix(line, "  job-other "):
			admitted = line
		}
	}
	if header == "" || pending == "" || admitted == "" {
		t.Fatalf("missing expected rows in output:\n%s", out)
	}
	// Header cell offsets are the column grid every data row must land on.
	var columns []int
	for i := range header {
		if header[i] != ' ' && (i == 0 || header[i-1] == ' ') {
			columns = append(columns, i)
		}
	}
	// Every non-empty cell in a data row must begin exactly on one of them.
	for _, row := range []struct{ name, line string }{{"pending", pending}, {"admitted", admitted}} {
		for i := range row.line {
			if row.line[i] == ' ' || (i > 0 && row.line[i-1] != ' ') {
				continue
			}
			if !slices.Contains(columns, i) {
				t.Errorf("%s row starts a cell at offset %d, off the header grid %v\n%s",
					row.name, i, columns, out)
				break
			}
		}
	}
}

func TestRender_AdmittedWorkloadOmitsMessageLine(t *testing.T) {
	out := Render(Snapshot{
		Name: "j", Namespace: "tau",
		JobFound: true,
		Workloads: []Workload{
			{Name: "w-1", Queue: "jobqueue", Phase: "Admitted", Admitted: true, Message: "stale"},
		},
	})
	if strings.Contains(out, "└─") {
		t.Errorf("admitted workload should not render a message line, got:\n%s", out)
	}
}
