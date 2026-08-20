// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installationcheck

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

type fakeRunner map[string]fakeResponse

type fakeResponse struct {
	output string
	err    error
}

func (f fakeRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	response, ok := f[strings.Join(args, " ")]
	if !ok {
		return "", errors.New("unexpected kubectl args: " + strings.Join(args, " "))
	}
	return response.output, response.err
}

func TestCheckReportsReadyInstallation(t *testing.T) {
	report := Check(context.Background(), readyRunner(), testOptions())
	if !report.Ready() {
		t.Fatalf("report not ready:\n%s", report.Summary())
	}
	if err := report.Err(); err != nil {
		t.Fatalf("ready report error: %v", err)
	}
	summary := report.Summary()
	for _, want := range []string{
		"PASS  Kubernetes",
		"PASS  Kueue",
		"PASS  KubeRay",
		"PASS  Portal",
		"PASS  Tau controller",
		"PASS  TauCluster",
		"PASS  Baseline queue",
		"PASS  Quota guard",
		"READY: 8/8 checks passed",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestCheckReportsActionableReadinessFailures(t *testing.T) {
	runner := readyRunner()
	runner["get deployments --namespace tau-system --selector app.kubernetes.io/instance=taugrid --output=json"] = fakeResponse{
		output: `{"items":[
			{"metadata":{"name":"taugrid-kueue-controller-manager","generation":2,"labels":{"helm.sh/chart":"kueue-0.18.2"}},"spec":{"replicas":1},"status":{"observedGeneration":2,"updatedReplicas":1,"readyReplicas":0,"availableReplicas":0}},
			{"metadata":{"name":"taugrid-kuberay-operator","generation":1,"labels":{"helm.sh/chart":"kuberay-operator-1.6.2"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}},
			{"metadata":{"name":"tau-portal","generation":1,"labels":{"helm.sh/chart":"taugrid-core-0.3.0","app.kubernetes.io/component":"portal"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}
		]}`,
	}
	runner["get clusters.tau.azure.com cluster --output=json"] = fakeResponse{
		output: `{"metadata":{"generation":3},"spec":{"managementMode":"Reconcile","deletionPolicy":"Retain","queues":{"ownership":"External"},"storage":{"ownership":"External"}},"status":{"phase":"Pending","observedGeneration":3,"conditions":[{"type":"NodesReady","status":"False","reason":"NoMatchingNodes","message":"no nodes match the configured VM-size rules","observedGeneration":3}]}}`,
	}
	runner["get validatingadmissionpolicybinding tau-quota-approval-guard --output=json"] = fakeResponse{
		output: `{"spec":{"policyName":"tau-quota-approval-guard","validationActions":["Warn"]}}`,
	}

	report := Check(context.Background(), runner, testOptions())
	if report.Ready() {
		t.Fatalf("report unexpectedly ready:\n%s", report.Summary())
	}
	summary := report.Summary()
	for _, want := range []string{
		"Deployment taugrid-kueue-controller-manager is not ready",
		"NodesReady=False (NoMatchingNodes: no nodes match the configured VM-size rules)",
		"validationActions=[Warn]",
		"NOT READY: 5/8 checks passed",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
	err := report.Err()
	if err == nil || !strings.Contains(err.Error(), "TauGrid installation is not ready") ||
		!strings.Contains(err.Error(), "review TauCluster node label rules and matching nodes") {
		t.Fatalf("failure error is not actionable: %v", err)
	}
}

func TestCheckRejectsBroadQuotaGuard(t *testing.T) {
	runner := readyRunner()
	runner["get validatingadmissionpolicy tau-quota-approval-guard --output=json"] = fakeResponse{
		output: `{"spec":{"failurePolicy":"Fail","matchConstraints":{"resourceRules":[{"apiGroups":["tau.azure.com"],"apiVersions":["v1alpha1"],"operations":["CREATE","UPDATE"],"resources":["*"]}]}}}`,
	}

	report := Check(context.Background(), runner, testOptions())
	if report.Ready() || !strings.Contains(report.Summary(), "policy scope is broader than TauQuotaRequest CREATE/UPDATE") {
		t.Fatalf("broad quota guard was not rejected:\n%s", report.Summary())
	}
}

func TestCheckSkipsDisabledComponents(t *testing.T) {
	runner := readyRunner()
	runner["get deployments --namespace tau-system --selector app.kubernetes.io/instance=taugrid --output=json"] = fakeResponse{
		output: `{"items":[
			{"metadata":{"name":"tau-portal","generation":1,"labels":{"helm.sh/chart":"taugrid-core-0.3.0","app.kubernetes.io/component":"portal"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}
		]}`,
	}
	opts := testOptions()
	opts.DisabledComponents = []Component{ComponentKueue, ComponentKubeRay}

	report := Check(context.Background(), runner, opts)
	if !report.Ready() {
		t.Fatalf("disabled components blocked readiness:\n%s", report.Summary())
	}
	if err := report.Err(); err != nil {
		t.Fatalf("skipped report error: %v", err)
	}
	summary := report.Summary()
	for _, want := range []string{
		"SKIP  Kueue                components.kueue.enabled is false in the Helm release",
		"SKIP  KubeRay              components.kuberayOperator.enabled is false in the Helm release",
		"READY: 6/6 checks passed",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestCheckSkipsDisabledPortalWithoutReadingItsDeployment(t *testing.T) {
	runner := readyRunner()
	opts := testOptions()
	opts.DisabledComponents = []Component{ComponentPortal}

	report := Check(context.Background(), runner, opts)
	if !report.Ready() {
		t.Fatalf("disabled Portal blocked readiness:\n%s", report.Summary())
	}
	if !strings.Contains(report.Summary(), "SKIP  Portal") {
		t.Fatalf("summary missing Portal skip:\n%s", report.Summary())
	}
}

func TestCheckFindsPortalByStableComponentLabel(t *testing.T) {
	runner := readyRunner()
	key := "get deployments --namespace tau-system --selector app.kubernetes.io/instance=taugrid --output=json"
	runner[key] = fakeResponse{output: `{"items":[
		{"metadata":{"name":"taugrid-kueue-controller-manager","generation":1,"labels":{"helm.sh/chart":"kueue-0.18.2"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}},
		{"metadata":{"name":"taugrid-kuberay-operator","generation":1,"labels":{"helm.sh/chart":"kuberay-operator-1.6.2"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}},
		{"metadata":{"name":"custom-portal-name","generation":1,"labels":{"helm.sh/chart":"taugrid-core-0.3.0","app.kubernetes.io/component":"portal"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}
	]}`}

	report := Check(context.Background(), runner, testOptions())
	if !report.Ready() {
		t.Fatalf("Portal with a customized resource name was not ready:\n%s", report.Summary())
	}
}

func TestCheckUsesConfiguredSystemNamespaceForPortalAndController(t *testing.T) {
	runner := fakeRunner{}
	for key, response := range readyRunner() {
		runner[strings.ReplaceAll(key, "tau-system", "custom-system")] = response
	}
	opts := testOptions()
	opts.SystemNamespace = "custom-system"

	report := Check(context.Background(), runner, opts)
	if !report.Ready() {
		t.Fatalf("custom namespace report not ready:\n%s", report.Summary())
	}
}

func TestCheckStillValidatesEnabledComponentsWhenAnotherIsDisabled(t *testing.T) {
	runner := readyRunner()
	runner["get deployments --namespace tau-system --selector app.kubernetes.io/instance=taugrid --output=json"] = fakeResponse{
		output: `{"items":[
			{"metadata":{"name":"tau-portal","generation":1,"labels":{"helm.sh/chart":"taugrid-core-0.3.0","app.kubernetes.io/component":"portal"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}
		]}`,
	}
	opts := testOptions()
	opts.DisabledComponents = []Component{ComponentKueue}

	report := Check(context.Background(), runner, opts)
	if report.Ready() {
		t.Fatalf("missing KubeRay Deployment did not fail validation:\n%s", report.Summary())
	}
	summary := report.Summary()
	if !strings.Contains(summary, "SKIP  Kueue") {
		t.Fatalf("summary missing Kueue skip:\n%s", summary)
	}
	if !strings.Contains(summary, "FAIL  KubeRay") || !strings.Contains(summary, "NOT READY: 6/7 checks passed") {
		t.Fatalf("summary did not fail the enabled component:\n%s", summary)
	}
}

func TestCheckSkipsDisabledTauCoreSurfaces(t *testing.T) {
	runner := readyRunner()
	delete(runner, "get deployment tau-core-controller --namespace tau-system --output=json")
	delete(runner, "get clusters.tau.azure.com cluster --output=json")
	delete(runner, "get validatingadmissionpolicy tau-quota-approval-guard --output=json")
	delete(runner, "get validatingadmissionpolicybinding tau-quota-approval-guard --output=json")
	opts := testOptions()
	opts.DisabledComponents = []Component{ComponentTauCore}

	report := Check(context.Background(), runner, opts)
	if !report.Ready() {
		t.Fatalf("disabled tau-core-controller blocked readiness:\n%s", report.Summary())
	}
	summary := report.Summary()
	for _, want := range []string{
		"SKIP  Tau controller       components.tauCoreController.enabled is false in the Helm release",
		"SKIP  TauCluster           components.tauCoreController.enabled is false in the Helm release",
		"SKIP  Quota guard          components.tauCoreController.enabled is false in the Helm release",
		"READY: 5/5 checks passed",
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary missing %q:\n%s", want, summary)
		}
	}
}

func TestCheckFailsDisabledComponentPeersWhenDeploymentListErrors(t *testing.T) {
	runner := readyRunner()
	runner["get deployments --namespace tau-system --selector app.kubernetes.io/instance=taugrid --output=json"] = fakeResponse{
		err: errors.New("connection refused"),
	}
	opts := testOptions()
	opts.DisabledComponents = []Component{ComponentKueue}

	report := Check(context.Background(), runner, opts)
	summary := report.Summary()
	if !strings.Contains(summary, "SKIP  Kueue") {
		t.Fatalf("disabled component was not skipped despite list error:\n%s", summary)
	}
	if !strings.Contains(summary, "FAIL  KubeRay") || !strings.Contains(summary, "connection refused") {
		t.Fatalf("enabled component did not surface the list error:\n%s", summary)
	}
}

func TestDisabledComponentsReadsChartSwitches(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		values string
		want   []Component
	}{
		{
			name:   "defaults enable everything",
			values: `{"components":{"kueue":{"enabled":true},"kuberayOperator":{"enabled":true}}}`,
		},
		{
			name:   "absent components block nothing",
			values: `{"baselineQueue":{"enabled":true}}`,
		},
		{
			name:   "all validated components disabled",
			values: `{"components":{"kueue":{"enabled":false},"kuberayOperator":{"enabled":false},"tauCoreController":{"enabled":false},"taugridCore":{"enabled":false}}}`,
			want:   []Component{ComponentKueue, ComponentKubeRay, ComponentTauCore, ComponentPortal},
		},
		{
			name:   "only kuberay disabled",
			values: `{"components":{"kueue":{"enabled":true},"kuberayOperator":{"enabled":false}}}`,
			want:   []Component{ComponentKubeRay},
		},
		{
			name:   "only tau core disabled",
			values: `{"components":{"tauCoreController":{"enabled":false}}}`,
			want:   []Component{ComponentTauCore},
		},
		{
			name:   "only portal disabled",
			values: `{"taugrid-core":{"portal":{"enabled":false}}}`,
			want:   []Component{ComponentPortal},
		},
		{
			name:   "shorthand bool leaves the subchart installed",
			values: `{"components":{"kueue":false}}`,
		},
		{
			name:   "set-string leaves the subchart installed",
			values: `{"components":{"kueue":{"enabled":"false"}}}`,
		},
		{
			name:   "numeric switch leaves the subchart installed",
			values: `{"components":{"kueue":{"enabled":0}}}`,
		},
		{
			name:   "null components block nothing",
			values: `{"components":null}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, err := DisabledComponents([]byte(testCase.values))
			if err != nil {
				t.Fatalf("DisabledComponents errored: %v", err)
			}
			if !slices.Equal(got, testCase.want) {
				t.Fatalf("DisabledComponents() = %v, want %v", got, testCase.want)
			}
		})
	}
}

func TestDisabledComponentsRejectsUnreadableValues(t *testing.T) {
	if _, err := DisabledComponents([]byte("not json")); err == nil {
		t.Fatal("DisabledComponents accepted unreadable Helm values")
	}
}

func TestWaitReturnsSuccessfulReadinessReport(t *testing.T) {
	report, err := Wait(context.Background(), readyRunner(), testOptions())
	if err != nil {
		t.Fatalf("Wait errored: %v", err)
	}
	if !report.Ready() {
		t.Fatalf("Wait report not ready:\n%s", report.Summary())
	}
}

func TestWaitTimeoutCancelsBlockedKubectlCall(t *testing.T) {
	opts := testOptions()
	opts.Timeout = 20 * time.Millisecond

	started := time.Now()
	report, err := Wait(context.Background(), blockingRunner{}, opts)
	if err == nil || !strings.Contains(err.Error(), "did not become ready within") {
		t.Fatalf("Wait error = %v; want readiness timeout", err)
	}
	if time.Since(started) > time.Second {
		t.Fatalf("Wait did not cancel blocked kubectl call promptly")
	}
	if report.Ready() {
		t.Fatalf("timed-out report unexpectedly ready:\n%s", report.Summary())
	}
}

func TestNumericVersionPartAcceptsKubernetesMinorSuffix(t *testing.T) {
	got, err := numericVersionPart("30+")
	if err != nil || got != 30 {
		t.Fatalf("numericVersionPart() = %d, %v; want 30, nil", got, err)
	}
}

type blockingRunner struct{}

func (blockingRunner) Raw(ctx context.Context, _ []string, _ []byte) (string, error) {
	<-ctx.Done()
	return "", ctx.Err()
}

func testOptions() Options {
	return Options{
		Release:         "taugrid",
		SystemNamespace: "tau-system",
		Timeout:         time.Second,
		PollInterval:    time.Millisecond,
	}
}

func readyRunner() fakeRunner {
	return fakeRunner{
		"version --output=json": {
			output: `{"serverVersion":{"major":"1","minor":"30+","gitVersion":"v1.30.7"}}`,
		},
		"get deployments --namespace tau-system --selector app.kubernetes.io/instance=taugrid --output=json": {
			output: `{"items":[
				{"metadata":{"name":"taugrid-kueue-controller-manager","generation":2,"labels":{"helm.sh/chart":"kueue-0.18.2"}},"spec":{"replicas":1},"status":{"observedGeneration":2,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}},
				{"metadata":{"name":"taugrid-kuberay-operator","generation":1,"labels":{"helm.sh/chart":"kuberay-operator-1.6.2"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}},
				{"metadata":{"name":"tau-portal","generation":1,"labels":{"helm.sh/chart":"taugrid-core-0.3.0","app.kubernetes.io/component":"portal"}},"spec":{"replicas":1},"status":{"observedGeneration":1,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}
			]}`,
		},
		"get deployment tau-core-controller --namespace tau-system --output=json": {
			output: `{"metadata":{"name":"tau-core-controller","generation":4},"spec":{"replicas":1},"status":{"observedGeneration":4,"updatedReplicas":1,"readyReplicas":1,"availableReplicas":1}}`,
		},
		"get clusters.tau.azure.com cluster --output=json": {
			output: `{"metadata":{"generation":3},"status":{"phase":"Pending","observedGeneration":3,"conditions":[{"type":"NodesReady","status":"True","reason":"NodeLabelsReady","message":"2 matching nodes have the configured topology labels","observedGeneration":3}]}}`,
		},
		"get clusterqueues --selector app.kubernetes.io/instance=taugrid,app.kubernetes.io/part-of=taugrid --output=json": {
			output: `{"items":[{"metadata":{"name":"jobqueue"},"status":{"conditions":[{"type":"Active","status":"True","reason":"Ready","message":"Can admit new workloads"}]}}]}`,
		},
		"get validatingadmissionpolicy tau-quota-approval-guard --output=json": {
			output: `{"spec":{"failurePolicy":"Fail","matchConstraints":{"resourceRules":[{"apiGroups":["tau.azure.com"],"apiVersions":["v1alpha1"],"operations":["CREATE","UPDATE"],"resources":["quotarequests"]}]}}}`,
		},
		"get validatingadmissionpolicybinding tau-quota-approval-guard --output=json": {
			output: `{"spec":{"policyName":"tau-quota-approval-guard","validationActions":["Deny"]}}`,
		},
	}
}
