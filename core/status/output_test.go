package status

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNewOutputStateMatrixForJobAndRayJob(t *testing.T) {
	for _, kind := range []string{"Job", "RayJob"} {
		for _, state := range []string{"pending", "admitted", "running", "failed-sidecar", "succeeded"} {
			t.Run(kind+"/"+state, func(t *testing.T) {
				snapshot := outputFixture(kind, state)
				out := NewOutput(snapshot)

				if out.SchemaVersion != OutputSchemaVersion {
					t.Fatalf("schemaVersion = %q", out.SchemaVersion)
				}
				if out.Kind != kind {
					t.Fatalf("kind = %q, want %q", out.Kind, kind)
				}
				if len(out.Workloads) != 1 {
					t.Fatalf("workloads = %+v", out.Workloads)
				}

				switch state {
				case "pending":
					if out.Workloads[0].Admitted || out.Workloads[0].Phase != "Pending" {
						t.Fatalf("pending workload = %+v", out.Workloads[0])
					}
				case "admitted":
					if !out.Workloads[0].Admitted {
						t.Fatalf("admitted workload = %+v", out.Workloads[0])
					}
				case "running":
					assertPlacementAndContainer(t, out, false)
				case "failed-sidecar":
					assertPlacementAndContainer(t, out, true)
					rendered := Render(snapshot)
					for _, want := range []string{
						"health:    degraded",
						"Containers:",
						"sidecar",
						"3",
						"17",
						"Error",
					} {
						if !strings.Contains(rendered, want) {
							t.Fatalf("failed-sidecar table missing %q:\n%s", want, rendered)
						}
					}
				case "succeeded":
					if out.State != "Complete" {
						t.Fatalf("succeeded state = %q", out.State)
					}
				}

				raw, err := json.Marshal(out)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(raw), `"schemaVersion":"v1alpha1"`) {
					t.Fatalf("JSON missing schema version: %s", raw)
				}
			})
		}
	}
}

func TestNewOutputMissingAndDeniedResourcesStayDistinct(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		snapshot := Snapshot{
			Name: "missing", Namespace: "tau",
			Observations: Observations{
				Job:       ResourceObservation{State: ObservationNotFound},
				RayJob:    ResourceObservation{State: ObservationNotFound},
				Workloads: ResourceObservation{State: ObservationObserved},
				Pods:      ResourceObservation{State: ObservationObserved},
			},
		}
		out := NewOutput(snapshot)
		if out.State != "NotFound" || out.Kind != "Unknown" {
			t.Fatalf("missing output = %+v", out)
		}
		if !strings.Contains(Render(snapshot), "no batch/v1 Job or RayJob found") {
			t.Fatalf("missing resource was not rendered as absent:\n%s", Render(snapshot))
		}
	})

	t.Run("denied", func(t *testing.T) {
		snapshot := Snapshot{
			Name: "private", Namespace: "tau",
			Observations: Observations{
				Job:       ResourceObservation{State: ObservationUnavailable, Reason: "access denied by Kubernetes RBAC"},
				RayJob:    ResourceObservation{State: ObservationNotFound},
				Workloads: ResourceObservation{State: ObservationUnavailable, Reason: "access denied by Kubernetes RBAC"},
				Pods:      ResourceObservation{State: ObservationUnavailable, Reason: "access denied by Kubernetes RBAC"},
			},
		}
		out := NewOutput(snapshot)
		if out.State != "Unknown" {
			t.Fatalf("denied state = %q", out.State)
		}
		rendered := Render(snapshot)
		for _, want := range []string{"status unavailable", "access denied by Kubernetes RBAC"} {
			if !strings.Contains(rendered, want) {
				t.Fatalf("denied output missing %q:\n%s", want, rendered)
			}
		}
		for _, misleading := range []string{
			"no batch/v1 Job or RayJob found",
			"Kueue did not see this workload",
			"workload is suspended or not yet admitted",
		} {
			if strings.Contains(rendered, misleading) {
				t.Fatalf("denied output falsely claims %q:\n%s", misleading, rendered)
			}
		}
	})

	t.Run("batch-only-cluster", func(t *testing.T) {
		snapshot := Snapshot{
			Name: "missing", Namespace: "tau",
			Observations: Observations{
				Job:       ResourceObservation{State: ObservationNotFound},
				RayJob:    ResourceObservation{State: ObservationUnavailable, Reason: "resource type is not installed"},
				Workloads: ResourceObservation{State: ObservationObserved},
				Pods:      ResourceObservation{State: ObservationObserved},
			},
		}
		if got := NewOutput(snapshot).State; got != "NotFound" {
			t.Fatalf("batch-only JSON state = %q", got)
		}
		rendered := Render(snapshot)
		if !strings.Contains(rendered, "no batch/v1 Job or RayJob found") {
			t.Fatalf("batch-only table did not report the absent run:\n%s", rendered)
		}
		if strings.Contains(rendered, "status unavailable") {
			t.Fatalf("optional RayJob CRD absence was rendered as an unavailable run:\n%s", rendered)
		}
	})
}

func TestHydratePodsPreservesSidecarRestartAndTerminationEvidence(t *testing.T) {
	pods := hydratePods([]byte(`{"items":[{
		"metadata":{"name":"train-abc","uid":"pod-1"},
		"spec":{"nodeName":"h200-node-7"},
		"status":{
			"phase":"Failed",
			"initContainerStatuses":[
				{"name":"native-sidecar","ready":true,"restartCount":2,"state":{"running":{}}}
			],
			"containerStatuses":[
				{"name":"main","ready":false,"restartCount":0,"state":{"terminated":{"reason":"Completed","exitCode":0}}},
				{"name":"sidecar","ready":false,"restartCount":3,"state":{"terminated":{"reason":"Error","message":"upload failed","exitCode":17}}}
			]
		}
	}]}`))
	if len(pods) != 1 || len(pods[0].Containers) != 2 {
		t.Fatalf("hydrated pods = %+v", pods)
	}
	sidecar := pods[0].Containers[1]
	if sidecar.Name != "sidecar" || sidecar.RestartCount != 3 || sidecar.ExitCode == nil || *sidecar.ExitCode != 17 {
		t.Fatalf("sidecar evidence = %+v", sidecar)
	}
	if pods[0].Restarts != 5 {
		t.Fatalf("pod restarts = %d, want app+init total 5", pods[0].Restarts)
	}
}

func TestReadObservationClassifiesNotFoundAndForbidden(t *testing.T) {
	notFound := observeObjectRead(
		errors.New(`Error from server (NotFound): jobs.batch "gone" not found`),
		"gone", false, "job.batch", "jobs.batch",
	)
	if notFound.State != ObservationNotFound {
		t.Fatalf("not-found observation = %+v", notFound)
	}
	forbidden := observeObjectRead(
		errors.New(`Error from server (Forbidden): jobs.batch "private" is forbidden`),
		"private", false, "job.batch", "jobs.batch",
	)
	if forbidden.State != ObservationUnavailable || !strings.Contains(forbidden.Reason, "RBAC") {
		t.Fatalf("forbidden observation = %+v", forbidden)
	}
}

func TestNewOutputMarksReasonlessTerminalFailureDegraded(t *testing.T) {
	out := NewOutput(Snapshot{
		Name: "failed", Namespace: "tau", JobFound: true,
		JobConditions: []Condition{{Type: "Failed", Status: "True"}},
	})
	if out.State != "Failed" || !out.Degraded {
		t.Fatalf("reasonless failure output = %+v", out)
	}
}

func TestNewOutputStoppedRayJobUsesFailurePrecedence(t *testing.T) {
	out := NewOutput(Snapshot{
		Name: "stopped", Namespace: "tau",
		RayJob: RayJob{
			Found:               true,
			JobDeploymentStatus: "Complete",
			JobStatus:           "STOPPED",
		},
	})
	if out.State != "Failed" || !out.Degraded {
		t.Fatalf("stopped RayJob output = %+v", out)
	}
}

func TestNewOutputSameNameCollisionUsesBatchJobPrecedence(t *testing.T) {
	out := NewOutput(Snapshot{
		Name: "shared", Namespace: "tau",
		JobFound: true,
		JobConditions: []Condition{{
			Type: "Complete", Status: "True",
		}},
		RayJob: RayJob{
			Found:               true,
			JobDeploymentStatus: "Complete",
			JobStatus:           "STOPPED",
		},
	})
	if out.State != "Complete" {
		t.Fatalf("same-name collision state = %q, want batch Job Complete", out.State)
	}
}

func TestNewOutputRayJobNonterminalUsesControllerLifecycle(t *testing.T) {
	out := NewOutput(Snapshot{
		Name: "starting", Namespace: "tau",
		RayJob: RayJob{
			Found:               true,
			JobDeploymentStatus: "Initializing",
			JobStatus:           "RUNNING",
		},
	})
	if out.State != "Initializing" {
		t.Fatalf("RayJob nonterminal state = %q, want controller lifecycle Initializing", out.State)
	}
}

func TestNewOutputUnreconciledRayJobUsesNewState(t *testing.T) {
	out := NewOutput(Snapshot{
		Name: "new", Namespace: "tau",
		RayJob: RayJob{Found: true},
	})
	if out.State != "New" {
		t.Fatalf("new RayJob state = %q", out.State)
	}
}

func outputFixture(kind, state string) Snapshot {
	snapshot := Snapshot{
		Name: "train", Namespace: "tau",
		Observations: Observations{
			Job:       ResourceObservation{State: ObservationNotFound},
			RayJob:    ResourceObservation{State: ObservationNotFound},
			Workloads: ResourceObservation{State: ObservationObserved},
			Pods:      ResourceObservation{State: ObservationObserved},
		},
		Workloads: []Workload{{
			Name: "train", Queue: "h200", Phase: "Admitted", Admitted: true,
		}},
	}
	if kind == "Job" {
		snapshot.JobFound = true
		snapshot.Observations.Job = ResourceObservation{State: ObservationObserved}
	} else {
		snapshot.RayJob = RayJob{Found: true, Name: "train", JobDeploymentStatus: "New"}
		snapshot.Observations.RayJob = ResourceObservation{State: ObservationObserved}
	}

	switch state {
	case "pending":
		snapshot.Workloads[0].Phase = "Pending"
		snapshot.Workloads[0].Admitted = false
		snapshot.Workloads[0].Reason = "Pending"
		if kind == "Job" {
			snapshot.JobSuspended = true
		} else {
			snapshot.RayJob.JobDeploymentStatus = "Suspended"
		}
	case "admitted":
		// The admitted Workload and created controller object are sufficient.
	case "running", "failed-sidecar":
		if kind == "Job" {
			snapshot.JobActive = 1
		} else {
			snapshot.RayJob.JobDeploymentStatus = "Running"
		}
		snapshot.Pods = []Pod{{
			Name: "train-pod", Phase: "Running", Node: "h200-node-7", Ready: "1/2",
			Containers: []Container{
				{Name: "main", State: "running", Ready: true},
				{Name: "sidecar", State: "running", Ready: true},
			},
		}}
		if state == "failed-sidecar" {
			snapshot.Pods[0].Phase = "Failed"
			snapshot.Pods[0].Ready = "0/2"
			snapshot.Pods[0].Containers[1] = Container{
				Name: "sidecar", State: "terminated", Reason: "Error",
				RestartCount: 3, ExitCode: int32Ptr(17),
			}
		}
	case "succeeded":
		snapshot.Workloads[0].Phase = "Finished"
		snapshot.Workloads[0].Reason = "Succeeded"
		if kind == "Job" {
			snapshot.JobSucceeded = 1
			snapshot.JobConditions = []Condition{{Type: "Complete", Status: "True"}}
		} else {
			snapshot.RayJob.JobDeploymentStatus = "Complete"
			snapshot.RayJob.JobStatus = "SUCCEEDED"
		}
	}
	return snapshot
}

func assertPlacementAndContainer(t *testing.T, out Output, degraded bool) {
	t.Helper()
	if len(out.Pods) != 1 || out.Pods[0].Node != "h200-node-7" {
		t.Fatalf("pod placement = %+v", out.Pods)
	}
	if len(out.Pods[0].Containers) != 2 {
		t.Fatalf("containers = %+v", out.Pods[0].Containers)
	}
	if out.Degraded != degraded {
		t.Fatalf("degraded = %t, want %t; reason=%q", out.Degraded, degraded, out.Reason)
	}
	if degraded {
		sidecar := out.Pods[0].Containers[1]
		if sidecar.Name != "sidecar" || sidecar.RestartCount != 3 || sidecar.ExitCode == nil || *sidecar.ExitCode != 17 {
			t.Fatalf("failed sidecar = %+v", sidecar)
		}
	}
}
