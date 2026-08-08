package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/status"
)

func TestRunStatusRegistersRunProfileFlag(t *testing.T) {
	if flag := newRunStatusCmd().Flags().Lookup("run-profile"); flag == nil {
		t.Fatal("tau run status must support the --run-profile handoff")
	}
}

func TestRunLifecycleQueriesRegisterWorkspaceFlag(t *testing.T) {
	tests := map[string]func() *cobra.Command{
		"status":  newRunStatusCmd,
		"logs":    newRunLogsCmd,
		"get":     newRunGetCmd,
		"cancel":  newRunCancelCmd,
		"delete":  newRunDeleteCmd,
		"archive": newRunArchiveCmd,
	}
	for name, build := range tests {
		t.Run(name, func(t *testing.T) {
			cmd := build()
			flag := cmd.Flags().Lookup("workspace")
			if flag == nil {
				t.Fatalf("tau run %s must accept --workspace", name)
			}
			if flag.DefValue != "" {
				t.Fatalf("tau run %s --workspace default = %q, want empty", name, flag.DefValue)
			}
			if err := cmd.Flags().Set("workspace", "research"); err != nil {
				t.Fatalf("set tau run %s --workspace: %v", name, err)
			}
			if got, err := cmd.Flags().GetString("workspace"); err != nil || got != "research" {
				t.Fatalf("tau run %s --workspace = %q, %v", name, got, err)
			}
		})
	}
}

func TestWatchStatusCommand_MultiKueueProgressesUntilReady(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	snapshots := []status.Snapshot{
		multiKueueWatchSnapshot(status.MultiKueueStatePending),
		multiKueueWatchSnapshot(status.MultiKueueStateNominated),
		multiKueueWatchSnapshot(status.MultiKueueStateSelected),
		multiKueueWatchSnapshot(status.MultiKueueStateRetry),
		multiKueueWatchSnapshot(status.MultiKueueStateReady),
	}
	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace: "ray",
		Interval:  time.Millisecond,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			snap := snapshots[calls]
			calls++
			return snap, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("watch should stop on Ready, got %v", err)
	}
	if calls != len(snapshots) {
		t.Fatalf("expected %d snapshots, got %d", len(snapshots), calls)
	}
}

func TestWatchStatusCommand_MultiKueueRejectedFails(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace: "ray",
		Interval:  time.Millisecond,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			return multiKueueWatchSnapshot(status.MultiKueueStateRejected), nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err == nil || !strings.Contains(err.Error(), "startup phase failed") {
		t.Fatalf("expected rejected placement failure, got %v", err)
	}
}

func TestWatchStatusCommand_MultiKueueManagerViewWithoutLocalPodsDoesNotExitEarly(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return multiKueueWatchSnapshot(status.MultiKueueStateSelected), nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("selected manager-view status should keep watching until max iterations, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_GenericAdmissionChecksDoNotTriggerMultiKueueTermination(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				JobFound:  true,
				Workloads: []status.Workload{{
					Name: "train-001",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "quota-check", State: "Rejected", Message: "quota full"},
					},
				}},
			}, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("generic admission checks should not be treated as MultiKueue watch failure, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueReadyPlusGenericPendingKeepsWatching(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			snap := multiKueueWatchSnapshot(status.MultiKueueStateReady)
			snap.JobActive = 0
			snap.Workloads[0].AdmissionChecks = append(snap.Workloads[0].AdmissionChecks,
				status.AdmissionCheck{Name: "quota-check", State: "Pending", ControllerName: "kueue.x-k8s.io/provisioning"},
			)
			return snap, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("generic pending admission checks should keep watching, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueReadyPlusGenericRejectedFails(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace: "ray",
		Interval:  time.Millisecond,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			snap := multiKueueWatchSnapshot(status.MultiKueueStateReady)
			snap.JobActive = 0
			snap.Workloads[0].AdmissionChecks = append(snap.Workloads[0].AdmissionChecks,
				status.AdmissionCheck{Name: "quota-check", State: "Rejected", ControllerName: "kueue.x-k8s.io/provisioning"},
			)
			return snap, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err == nil || !strings.Contains(err.Error(), "startup phase failed") {
		t.Fatalf("generic rejected admission checks should fail watch even when placement is ready, got %v", err)
	}
}

func TestWatchStatusCommand_MultiKueueReadyStopsWithoutWaitingForMirroredRayJobRunning(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace: "ray",
		Interval:  time.Millisecond,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					ManagedBy:           "kueue.x-k8s.io/multikueue",
					JobDeploymentStatus: "Initializing",
				},
				Workloads: []status.Workload{{
					Name:        "train-001",
					Admitted:    true,
					Phase:       "Admitted",
					ClusterName: "worker-a",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: "kueue.x-k8s.io/multikueue"},
					},
				}},
			}, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("watch should stop on MultiKueue placement Ready, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected ready placement to stop after one snapshot, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueSuspendedRayJobKeepsWatching(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					ManagedBy:           "kueue.x-k8s.io/multikueue",
					JobDeploymentStatus: "Suspended",
				},
			}, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("suspended manager-view RayJob should keep watching until max iterations, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueSuspendedReadyRayJobKeepsWatching(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetContext(context.Background())

	calls := 0
	err := watchStatusCommandWithHooks(cmd, statusRunOptions{
		Namespace:     "ray",
		Interval:      time.Millisecond,
		MaxIterations: 2,
	}, "train-001", watchStatusHooks{
		fetch: func(context.Context) (status.Snapshot, error) {
			calls++
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					ManagedBy:           "kueue.x-k8s.io/multikueue",
					JobDeploymentStatus: "Suspended",
				},
				Workloads: []status.Workload{{
					Name:        "train-001",
					ClusterName: "worker-a",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: "kueue.x-k8s.io/multikueue"},
					},
				}},
			}, nil
		},
		wait:        func(context.Context, time.Duration) error { return nil },
		clearScreen: func(io.Writer) {},
	})
	if err != nil {
		t.Fatalf("suspended manager-view RayJob with ready placement should keep watching until max iterations, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected watch to continue through max iterations, got %d fetches", calls)
	}
}

func TestWatchStatusCommand_MultiKueueTerminalPrecedence(t *testing.T) {
	tests := []struct {
		name    string
		snap    status.Snapshot
		wantErr string
	}{
		{
			name: "complete beats rejected placement",
			snap: status.Snapshot{
				Name:          "train-001",
				Namespace:     "ray",
				JobFound:      true,
				JobConditions: []status.Condition{{Type: "Complete", Status: "True"}},
				Workloads: []status.Workload{{
					Name: "train-001",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "multikueue", State: "Rejected", Message: "quota exceeded", ControllerName: "kueue.x-k8s.io/multikueue"},
					},
				}},
			},
		},
		{
			name: "failed beats ready placement",
			snap: status.Snapshot{
				Name:          "train-001",
				Namespace:     "ray",
				JobFound:      true,
				JobConditions: []status.Condition{{Type: "Failed", Status: "True", Reason: "BackoffLimitExceeded"}},
				Workloads: []status.Workload{{
					Name: "train-001",
					AdmissionChecks: []status.AdmissionCheck{
						{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: "kueue.x-k8s.io/multikueue"},
					},
				}},
			},
			wantErr: "startup phase failed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetContext(context.Background())
			err := watchStatusCommandWithHooks(cmd, statusRunOptions{
				Namespace: "ray",
				Interval:  time.Millisecond,
			}, "train-001", watchStatusHooks{
				fetch: func(context.Context) (status.Snapshot, error) {
					return tt.snap, nil
				},
				wait:        func(context.Context, time.Duration) error { return nil },
				clearScreen: func(io.Writer) {},
			})
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func multiKueueWatchSnapshot(state status.MultiKueueState) status.Snapshot {
	snap := status.Snapshot{
		Name:         "train-001",
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: "kueue.x-k8s.io/multikueue",
	}
	workload := status.Workload{Name: "train-001"}
	switch state {
	case status.MultiKueueStateNominated:
		workload.NominatedClusterNames = []string{"worker-a", "worker-b"}
	case status.MultiKueueStateSelected:
		workload.ClusterName = "worker-a"
	case status.MultiKueueStateRetry:
		workload.ClusterName = "worker-a"
		workload.AdmissionChecks = []status.AdmissionCheck{{Name: "multikueue", State: "Retry", Message: "retrying reservation", ControllerName: "kueue.x-k8s.io/multikueue"}}
	case status.MultiKueueStateReady:
		snap.JobActive = 1
		workload.Admitted = true
		workload.Phase = "Admitted"
		workload.ClusterName = "worker-a"
		workload.AdmissionChecks = []status.AdmissionCheck{{Name: "multikueue", State: "Ready", Message: "reservation acquired", ControllerName: "kueue.x-k8s.io/multikueue"}}
	case status.MultiKueueStateRejected:
		workload.AdmissionChecks = []status.AdmissionCheck{{Name: "multikueue", State: "Rejected", Message: "quota exceeded", ControllerName: "kueue.x-k8s.io/multikueue"}}
	}
	if state != status.MultiKueueStatePending {
		snap.Workloads = []status.Workload{workload}
	}
	return snap
}
