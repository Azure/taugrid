package onboarding

import (
	"context"
	"github.com/Azure/taugrid/core/workloadmeta"
	"strings"
	"testing"
	"time"
)

type fakeSmokeRunner struct {
	calls     [][]string
	responses []string
}

func (f *fakeSmokeRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	if len(f.responses) == 0 {
		return "", nil
	}
	response := f.responses[0]
	f.responses = f.responses[1:]
	return response, nil
}

func TestRenderSmokeIsBoundedKueueJob(t *testing.T) {
	manifest, err := RenderSmoke(SmokeOptions{
		Name: "smoke-test", Namespace: "sample", Queue: "jobqueue", ServiceAccount: "tau-workload", Workspace: "sample",
		ResultScope: "/data/workspaces/sample",
	})
	if err != nil {
		t.Fatal(err)
	}
	text := string(manifest)
	for _, want := range []string{
		"kind: Job",
		"name: smoke-test",
		"namespace: sample",
		"kueue.x-k8s.io/queue-name: jobqueue",
		"serviceAccountName: tau-workload",
		workloadmeta.LabelWorkspace + ": sample",
		workloadmeta.LabelRunID + ": smoke-test",
		workloadmeta.LabelWorkloadKind + ": job",
		workloadmeta.AnnotationWorkspaceID + ": sample",
		workloadmeta.AnnotationResultScope + ": /data/workspaces/sample",
		"ttlSecondsAfterFinished: 600",
		"activeDeadlineSeconds: 300",
		"allowPrivilegeEscalation: false",
		"runAsNonRoot: true",
		"type: RuntimeDefault",
		SmokeImage,
		smokeLogMarker,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("smoke manifest missing %q:\n%s", want, text)
		}
	}
}

func TestSmokeClientDryRunDoesNotCallKubernetes(t *testing.T) {
	fake := &fakeSmokeRunner{}
	result, err := (SmokeRunner{Runner: fake}).Run(context.Background(), SmokeOptions{
		Name: "smoke-test", Namespace: "sample", Queue: "jobqueue", Workspace: "sample", DryRun: "client",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != "DryRun" || len(result.Manifest) == 0 || len(fake.calls) != 0 {
		t.Fatalf("result=%#v calls=%v", result, fake.calls)
	}
}

func TestSmokeRunAppliesWaitsAndReadsLogs(t *testing.T) {
	fake := &fakeSmokeRunner{responses: []string{
		"job.batch/smoke-test created\n",
		"job.batch/smoke-test condition met\n",
		smokeLogMarker + "\n",
	}}
	result, err := (SmokeRunner{Runner: fake}).Run(context.Background(), SmokeOptions{
		Name: "smoke-test", Namespace: "sample", Queue: "jobqueue", Workspace: "sample", Timeout: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Phase != "Succeeded" || len(fake.calls) != 3 {
		t.Fatalf("result=%#v calls=%v", result, fake.calls)
	}
	if got := strings.Join(fake.calls[1], " "); got != "-n sample wait --for=condition=complete job/smoke-test --timeout=1m0s" {
		t.Fatalf("wait args = %q", got)
	}
}

func TestRenderSmokeRequiresWorkspace(t *testing.T) {
	_, err := RenderSmoke(SmokeOptions{
		Name: "smoke-test", Namespace: "sample", Queue: "jobqueue",
	})
	if err == nil || !strings.Contains(err.Error(), "workspace is required") {
		t.Fatalf("expected workspace validation error, got %v", err)
	}
}
