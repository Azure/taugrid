package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/kustoquery"
	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestRunLogsCommand_LocalRayJobPreserved(t *testing.T) {
	var out bytes.Buffer
	var queryCalled bool
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace: "ray",
		Tail:      200,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found: true,
					Name:  "train-001",
				},
			}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "local ray logs\n", nil
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			t.Fatal("job fallback must not run when local RayJob logs succeed")
			return "", nil
		},
		queryADXLogs: func(context.Context, kustoLogsQuery) ([]kustoquery.Row, error) {
			queryCalled = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("expected local RayJob logs to succeed, got %v", err)
	}
	if queryCalled {
		t.Fatal("central ADX query must not run for local RayJob logs")
	}
	if got := out.String(); got != "local ray logs\n" {
		t.Fatalf("unexpected local RayJob logs output: %q", got)
	}
}

func TestRunLogsCommand_MultiKueueWorkerCopyKeepsLocalRayLogs(t *testing.T) {
	var out bytes.Buffer
	var queryCalled bool
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace: "ray",
		Tail:      50,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueWorkerCopyLogSnapshot(), nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "worker-local ray logs\n", nil
		},
		queryADXLogs: func(context.Context, kustoLogsQuery) ([]kustoquery.Row, error) {
			queryCalled = true
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("expected worker-copy MultiKueue RayJob logs to stay local, got %v", err)
	}
	if queryCalled {
		t.Fatal("central ADX query must not run for worker-local RayJob copies without manager placement markers")
	}
	if got := out.String(); got != "worker-local ray logs\n" {
		t.Fatalf("unexpected worker-copy RayJob logs output: %q", got)
	}
}

func TestRunLogsCommand_LocalJobFallbackPreserved(t *testing.T) {
	var out bytes.Buffer
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace: "ray",
		Follow:    true,
		Tail:      10,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{Name: "train-001", Namespace: "ray"}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", errors.New("not a rayjob")
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			return "batch job logs\n", nil
		},
	})
	if err != nil {
		t.Fatalf("expected batch job fallback logs to succeed, got %v", err)
	}
	if got := out.String(); got != "batch job logs\n" {
		t.Fatalf("unexpected batch job logs output: %q", got)
	}
}

func TestLocalJobLogsTailMinusOneIsPassedExplicitly(t *testing.T) {
	var gotArgs []string
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		gotArgs = append([]string(nil), args...)
		return "", nil
	})

	if _, err := localJobLogs(context.Background(), runner, "ray", "train-001", false, -1); err != nil {
		t.Fatal(err)
	}
	want := []string{"-n", "ray", "logs", "-l", "job-name=train-001", "--tail=-1"}
	if strings.Join(gotArgs, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("kubectl args = %v, want %v", gotArgs, want)
	}
}

func TestRunLogsCommand_FoundRayJobNeverFallsBackToJobNameSelector(t *testing.T) {
	var out bytes.Buffer
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace: "ray",
		Tail:      200,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					JobDeploymentStatus: "Initializing",
					RayClusterStatus:    "",
				},
			}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", errors.New("not a RayJob or jobId not available")
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			t.Fatal("batch/v1 job-name fallback must not run for a RayJob; it matches no pods and reports empty success")
			return "", nil
		},
	})
	if err == nil {
		t.Fatal("expected an error explaining the RayJob is not ready, got nil")
	}
	if got := out.String(); got != "" {
		t.Fatalf("expected no log output for a not-ready RayJob, got %q", got)
	}
	for _, want := range []string{"status.jobId", "tau run status train-001"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not mention %q", err.Error(), want)
		}
	}
}

func TestRunLogsCommand_FoundRayJobWithJobIDOmitsStartupHint(t *testing.T) {
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace: "ray",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found: true,
					Name:  "train-001",
					JobID: "raysubmit_abc123",
				},
			}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", errors.New("head pod not found for RayJob train-001")
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			t.Fatal("batch/v1 job-name fallback must not run for a RayJob")
			return "", nil
		},
	})
	if err == nil {
		t.Fatal("expected the underlying RayJob logs error to surface, got nil")
	}
	if !strings.Contains(err.Error(), "head pod not found") {
		t.Fatalf("expected the underlying cause to survive, got %q", err.Error())
	}
	if strings.Contains(err.Error(), "status.jobId") {
		t.Fatalf("startup hint must not appear once jobId is populated, got %q", err.Error())
	}
}

func TestRunLogsCommand_TerminalLocalRayJobFallsBackToADXAfterPodCleanup(t *testing.T) {
	var out bytes.Buffer
	var gotQuery kustoLogsQuery
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		Tail:          2,
		KustoCluster:  "aks-ai-runtime-eastus2",
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					JobID:               "raysubmit_abc123",
					RayClusterName:      "train-001-cluster",
					JobDeploymentStatus: "Complete",
					JobStatus:           "SUCCEEDED",
				},
			}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", errors.New("head pod not found for RayJob train-001")
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			t.Fatal("batch/v1 fallback must not run for a terminal RayJob")
			return "", nil
		},
		queryADXLogs: func(_ context.Context, spec kustoLogsQuery) ([]kustoquery.Row, error) {
			gotQuery = spec
			return []kustoquery.Row{
				{"Timestamp": "2026-08-06T17:00:01Z", "Body": "line-1"},
				{"Timestamp": "2026-08-06T17:00:02Z", "Body": "line-2"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("expected terminal local ADX fallback to succeed, got %v", err)
	}
	for _, want := range []string{
		"| where Cluster == @'aks-ai-runtime-eastus2'",
		"| where Namespace == @'ray'",
		"| where Pod startswith @'train-001-cluster-head'",
	} {
		if !strings.Contains(gotQuery.Query, want) {
			t.Fatalf("terminal local query missing %q:\n%s", want, gotQuery.Query)
		}
	}
	if got := out.String(); got != "line-1\nline-2\n" {
		t.Fatalf("unexpected terminal local logs output: %q", got)
	}
}

func TestRunLogsCommand_TerminalLocalRayJobRequiresExplicitADXCluster(t *testing.T) {
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found:               true,
					Name:                "train-001",
					JobID:               "raysubmit_abc123",
					RayClusterName:      "train-001-cluster",
					JobDeploymentStatus: "Failed",
				},
			}, nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", errors.New("head pod not found for RayJob train-001")
		},
	})
	if err == nil {
		t.Fatal("expected missing local ADX cluster metadata to fail")
	}
	for _, want := range []string{"local pod logs unavailable", "--kusto-cluster"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("terminal local fallback error %q missing %q", err.Error(), want)
		}
	}
}

func TestRunLogsCommand_ManagerMultiKueueUsesADXAnnotationAndTail(t *testing.T) {
	var out bytes.Buffer
	var gotQuery kustoLogsQuery
	var localCalled bool
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		Tail:          2,
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueManagerLogSnapshot("worker-a"), nil
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			localCalled = true
			return "unexpected local logs", nil
		},
		resolveMultiKueueWorker: func(context.Context, kubeRawRunner, string) (multiKueueWorkerRef, error) {
			return multiKueueWorkerRef{
				Name:        "worker-a",
				Annotations: map[string]string{workloadmeta.AnnotationClusterName: "taugrid-flex"},
			}, nil
		},
		queryADXLogs: func(_ context.Context, spec kustoLogsQuery) ([]kustoquery.Row, error) {
			gotQuery = spec
			return []kustoquery.Row{
				{"Timestamp": "2026-01-01T00:00:01Z", "Body": "line-1"},
				{"Timestamp": "2026-01-01T00:00:02Z", "Body": "line-2"},
				{"Timestamp": "2026-01-01T00:00:03Z", "Body": "line-3"},
			}, nil
		},
	})
	if err != nil {
		t.Fatalf("expected manager-side ADX logs query to succeed, got %v", err)
	}
	if localCalled {
		t.Fatal("local RayJob logs must not run from manager-only MultiKueue view")
	}
	if gotQuery.Endpoint != "https://adx.example" {
		t.Fatalf("unexpected ADX endpoint: %q", gotQuery.Endpoint)
	}
	if gotQuery.Database != "Logs" {
		t.Fatalf("unexpected ADX database: %q", gotQuery.Database)
	}
	for _, needle := range []string{
		"| where Cluster == @'taugrid-flex'",
		"| where Namespace == @'ray'",
		"| where Container == @'ray-driver-log-offload'",
		"| where Pod startswith @'train-001-raycluster-head'",
		"| order by Timestamp desc",
		"| take 2",
		"| order by Timestamp asc",
	} {
		if !strings.Contains(gotQuery.Query, needle) {
			t.Fatalf("expected query to contain %q, got:\n%s", needle, gotQuery.Query)
		}
	}
	if got := out.String(); got != "line-2\nline-3\n" {
		t.Fatalf("unexpected manager-side logs output: %q", got)
	}
}

func TestRunLogsCommand_ManagerMultiKueueRejectsFollow(t *testing.T) {
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		Follow:        true,
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueManagerLogSnapshot("worker-a"), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "--follow is not supported") {
		t.Fatalf("expected actionable --follow rejection, got %v", err)
	}
}

func TestRunLogsCommand_ManagerMultiKueueRequiresKustoConfig(t *testing.T) {
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace: "ray",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueManagerLogSnapshot("worker-a"), nil
		},
	})
	if err == nil {
		t.Fatal("expected missing ADX config to fail")
	}
	if !strings.Contains(err.Error(), "--kusto-endpoint") || !strings.Contains(err.Error(), "--kusto-database") {
		t.Fatalf("expected missing ADX config error, got %v", err)
	}
}

func TestRunLogsCommand_ManagerMultiKueueRequiresSelectedWorker(t *testing.T) {
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			snap := multiKueueManagerLogSnapshot("worker-a")
			snap.Workloads[0].ClusterName = ""
			snap.Workloads[0].NominatedClusterNames = []string{"worker-a", "worker-b"}
			snap.Workloads[0].AdmissionChecks[0].State = "Pending"
			return snap, nil
		},
	})
	if err == nil {
		t.Fatal("expected missing selected worker to fail")
	}
	if !strings.Contains(err.Error(), "has not been assigned to a MultiKueue worker yet") || !strings.Contains(err.Error(), "worker-a, worker-b") {
		t.Fatalf("expected actionable selected-worker error, got %v", err)
	}
}

func TestRunLogsCommand_ManagerMultiKueueRequiresADXClusterAnnotation(t *testing.T) {
	var queryCalled bool
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		Tail:          200,
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueManagerLogSnapshot("worker-a"), nil
		},
		resolveMultiKueueWorker: func(context.Context, kubeRawRunner, string) (multiKueueWorkerRef, error) {
			return multiKueueWorkerRef{Name: "worker-a"}, nil
		},
		queryADXLogs: func(context.Context, kustoLogsQuery) ([]kustoquery.Row, error) {
			queryCalled = true
			return nil, nil
		},
	})
	if err == nil {
		t.Fatal("expected missing MKC annotation to fail closed")
	}
	if queryCalled {
		t.Fatal("ADX query must not run when the selected MultiKueueCluster is missing the required annotation")
	}
	if !strings.Contains(err.Error(), `selected MultiKueueCluster "worker-a" is missing required metadata.annotations["`+workloadmeta.AnnotationClusterName+`"]`) {
		t.Fatalf("expected actionable missing-annotation error, got %v", err)
	}
}

func TestRunLogsCommand_ManagerMultiKueueFetchErrorsSurfaceBeforeLocalFallback(t *testing.T) {
	var localCalled bool
	var queryCalled bool
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace: "ray",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			snap := multiKueueManagerLogSnapshot("worker-a")
			snap.Workloads = nil
			return snap, errors.New("forbidden workloads list")
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			localCalled = true
			return "unexpected local logs", nil
		},
		queryADXLogs: func(context.Context, kustoLogsQuery) ([]kustoquery.Row, error) {
			queryCalled = true
			return nil, nil
		},
	})
	if err == nil {
		t.Fatal("expected manager placement fetch error to be surfaced")
	}
	if localCalled {
		t.Fatal("local RayJob logs must not run when manager-side placement fetch fails for a MultiKueue RayJob")
	}
	if queryCalled {
		t.Fatal("ADX query must not run when manager-side placement fetch fails")
	}
	if !strings.Contains(err.Error(), "resolve manager-side MultiKueue placement for RayJob train-001") || !strings.Contains(err.Error(), "forbidden workloads list") {
		t.Fatalf("expected actionable manager placement fetch error, got %v", err)
	}
}

func TestRunLogsCommand_LocalRayJobFetchErrorsStillAllowLocalLogs(t *testing.T) {
	var out bytes.Buffer
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace: "ray",
		Tail:      25,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:      "train-001",
				Namespace: "ray",
				RayJob: status.RayJob{
					Found: true,
					Name:  "train-001",
				},
			}, errors.New("workloads list forbidden")
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "local ray logs after fetch error\n", nil
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			t.Fatal("job fallback must not run when local RayJob logs succeed")
			return "", nil
		},
	})
	if err != nil {
		t.Fatalf("expected local RayJob logs to survive non-manager fetch errors, got %v", err)
	}
	if got := out.String(); got != "local ray logs after fetch error\n" {
		t.Fatalf("unexpected local RayJob logs output: %q", got)
	}
}

func TestRunLogsCommand_MultiKueueBatchJobFetchErrorsStillUseLocalJobLogs(t *testing.T) {
	var out bytes.Buffer
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace: "ray",
		Tail:      5,
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{
				Name:         "train-001",
				Namespace:    "ray",
				JobFound:     true,
				JobManagedBy: "kueue.x-k8s.io/multikueue",
			}, errors.New("workloads list forbidden")
		},
		rayJobLogs: func(context.Context, *kube.Runner, string, string, bool) (string, error) {
			return "", errors.New("not a rayjob")
		},
		jobLogs: func(context.Context, kubeRawRunner, string, string, bool, int) (string, error) {
			return "batch job logs after fetch error\n", nil
		},
	})
	if err != nil {
		t.Fatalf("expected MultiKueue batch job logs to keep local fallback behavior, got %v", err)
	}
	if got := out.String(); got != "batch job logs after fetch error\n" {
		t.Fatalf("unexpected batch job logs output: %q", got)
	}
}

func TestRunLogsCommand_ManagerMultiKueuePropagatesADXQueryErrors(t *testing.T) {
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		Tail:          200,
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueManagerLogSnapshot("worker-a"), nil
		},
		resolveMultiKueueWorker: func(context.Context, kubeRawRunner, string) (multiKueueWorkerRef, error) {
			return multiKueueWorkerRef{Name: "worker-a", Annotations: map[string]string{workloadmeta.AnnotationClusterName: "taugrid-flex"}}, nil
		},
		queryADXLogs: func(context.Context, kustoLogsQuery) ([]kustoquery.Row, error) {
			return nil, errors.New("forbidden")
		},
	})
	if err == nil {
		t.Fatal("expected ADX query failure to be returned")
	}
	if !strings.Contains(err.Error(), "query ADX Logs.ContainerLogs") || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("expected actionable ADX query error, got %v", err)
	}
}

func TestRunLogsCommand_ManagerMultiKueueFailsWhenNoADXRowsYet(t *testing.T) {
	err := runLogsCommandWithHooks(context.Background(), &bytes.Buffer{}, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		Tail:          200,
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueManagerLogSnapshot("worker-a"), nil
		},
		resolveMultiKueueWorker: func(context.Context, kubeRawRunner, string) (multiKueueWorkerRef, error) {
			return multiKueueWorkerRef{Name: "worker-a", Annotations: map[string]string{workloadmeta.AnnotationClusterName: "taugrid-flex"}}, nil
		},
		queryADXLogs: func(context.Context, kustoLogsQuery) ([]kustoquery.Row, error) {
			return nil, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "no manager-side driver logs were found in ADX") {
		t.Fatalf("expected missing-row ADX error, got %v", err)
	}
}

func TestRunLogsCommand_ManagerMultiKueueTailZeroReturnsEmptyWithoutADXQuery(t *testing.T) {
	var out bytes.Buffer
	var queryCalled bool
	err := runLogsCommandWithHooks(context.Background(), &out, nil, "train-001", runLogsOptions{
		Namespace:     "ray",
		Tail:          0,
		KustoEndpoint: "https://adx.example",
		KustoDatabase: "Logs",
	}, runLogsHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return multiKueueManagerLogSnapshot("worker-a"), nil
		},
		resolveMultiKueueWorker: func(context.Context, kubeRawRunner, string) (multiKueueWorkerRef, error) {
			return multiKueueWorkerRef{Name: "worker-a", Annotations: map[string]string{workloadmeta.AnnotationClusterName: "taugrid-flex"}}, nil
		},
		queryADXLogs: func(context.Context, kustoLogsQuery) ([]kustoquery.Row, error) {
			queryCalled = true
			return []kustoquery.Row{{"Body": "line-1"}}, nil
		},
	})
	if err != nil {
		t.Fatalf("expected tail=0 manager-side logs to succeed without ADX query, got %v", err)
	}
	if queryCalled {
		t.Fatal("tail=0 should avoid the ADX logs query after placement/config validation")
	}
	if got := out.String(); got != "" {
		t.Fatalf("expected empty output for tail=0, got %q", got)
	}
}

func TestBuildMultiKueueRayDriverLogsQueryTailTwoEscapesFilters(t *testing.T) {
	query := buildMultiKueueRayDriverLogsQuery("aks'o\\east", "team'ns", "ray'cluster", 2)
	want := strings.Join([]string{
		"ContainerLogs",
		"| where Cluster == @'aks''o\\east'",
		"| where Namespace == @'team''ns'",
		"| where Container == @'ray-driver-log-offload'",
		"| where Pod startswith @'ray''cluster-head'",
		"| project Timestamp, Body=tostring(Body)",
		"| order by Timestamp desc",
		"| take 2",
		"| order by Timestamp asc",
	}, "\n")
	if query != want {
		t.Fatalf("unexpected KQL query:\nwant:\n%s\n\ngot:\n%s", want, query)
	}
}

func TestBuildMultiKueueRayDriverLogsQueryTailMinusOneReturnsFullHistory(t *testing.T) {
	query := buildMultiKueueRayDriverLogsQuery("taugrid-flex", "ray", "train-001-raycluster", -1)
	want := strings.Join([]string{
		"ContainerLogs",
		"| where Cluster == @'taugrid-flex'",
		"| where Namespace == @'ray'",
		"| where Container == @'ray-driver-log-offload'",
		"| where Pod startswith @'train-001-raycluster-head'",
		"| project Timestamp, Body=tostring(Body)",
		"| order by Timestamp asc",
	}, "\n")
	if query != want {
		t.Fatalf("unexpected full-history KQL query:\nwant:\n%s\n\ngot:\n%s", want, query)
	}
}

func TestFormatCentralLogRowsAppliesTailAndNormalizesNewlines(t *testing.T) {
	rows := []kustoquery.Row{
		{"Timestamp": "2026-01-01T00:00:01Z", "Body": "line-1\n"},
		{"Timestamp": "2026-01-01T00:00:02Z", "Body": "line-2"},
		{"Timestamp": "2026-01-01T00:00:03Z", "Body": "line-3"},
	}
	if got := formatCentralLogRows(rows, 2); got != "line-2\nline-3\n" {
		t.Fatalf("unexpected tailed central logs output: %q", got)
	}
	if got := formatCentralLogRows(rows, -1); got != "line-1\nline-2\nline-3\n" {
		t.Fatalf("unexpected full central logs output: %q", got)
	}
}

func TestMultiKueueWorkerRefADXClusterNamePrefersAnnotation(t *testing.T) {
	ref := multiKueueWorkerRef{
		Name:        "worker-a",
		Annotations: map[string]string{workloadmeta.AnnotationClusterName: "worker-b"},
	}
	if got := ref.ADXClusterName(); got != "worker-b" {
		t.Fatalf("expected annotation-backed ADX cluster name, got %q", got)
	}

	ref = multiKueueWorkerRef{Name: "worker-a"}
	if got := ref.ADXClusterName(); got != "" {
		t.Fatalf("expected missing annotation to resolve no ADX cluster name, got %q", got)
	}
}

func multiKueueManagerLogSnapshot(worker string) status.Snapshot {
	return status.Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		RayJob: status.RayJob{
			Found:          true,
			Name:           "train-001",
			ManagedBy:      "kueue.x-k8s.io/multikueue",
			RayClusterName: "train-001-raycluster",
		},
		Workloads: []status.Workload{{
			Name:        "train-001",
			ClusterName: worker,
			AdmissionChecks: []status.AdmissionCheck{{
				Name:           "multikueue",
				State:          "Selected",
				ControllerName: "kueue.x-k8s.io/multikueue",
			}},
		}},
	}
}

func multiKueueWorkerCopyLogSnapshot() status.Snapshot {
	return status.Snapshot{
		Name:      "train-001",
		Namespace: "ray",
		RayJob: status.RayJob{
			Found:          true,
			Name:           "train-001",
			ManagedBy:      "",
			RayClusterName: "train-001-raycluster",
		},
	}
}
