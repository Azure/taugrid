// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package installationcheck validates the read-only readiness contract for a
// Helm-installed TauGrid control plane.
package installationcheck

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	minimumKubernetesMajor = 1
	minimumKubernetesMinor = 30

	tauPlatformNamespace = "tau-platform"
	tauControllerName    = "tau-core-controller"
	tauClusterName       = "cluster"
	quotaGuardName       = "tau-quota-approval-guard"
)

// Runner is the read-only kubectl surface used by installation validation.
type Runner interface {
	Raw(ctx context.Context, args []string, stdin []byte) (string, error)
}

// Options identifies the Helm release and bounds readiness polling.
type Options struct {
	Release               string
	ControlPlaneNamespace string
	Timeout               time.Duration
	PollInterval          time.Duration
	// DisabledComponents names chart components the release turned off. They
	// are reported as skipped instead of failing. The zero value validates
	// every component.
	DisabledComponents []Component
}

// Component identifies a chart component covered by installation validation.
type Component string

const (
	ComponentKueue   Component = "Kueue"
	ComponentKubeRay Component = "KubeRay"
	ComponentTauCore Component = "Tau controller"
)

type chartComponent struct {
	name Component
	// valuesKey is the components.<valuesKey>.enabled switch in the chart.
	valuesKey string
	// chartPrefix is the helm.sh/chart label prefix on the rendered Deployment.
	chartPrefix string
}

var chartComponents = []chartComponent{
	{name: ComponentKueue, valuesKey: "kueue", chartPrefix: "kueue-"},
	{name: ComponentKubeRay, valuesKey: "kuberayOperator", chartPrefix: "kuberay-operator-"},
}

var componentSwitches = []chartComponent{
	chartComponents[0],
	chartComponents[1],
	{name: ComponentTauCore, valuesKey: "tauCoreController"},
}

// DisabledComponents reports which validated components a release turned off,
// given the release's coalesced Helm values as JSON.
func DisabledComponents(helmValues []byte) ([]Component, error) {
	var values struct {
		Components map[string]any `json:"components"`
	}
	if err := json.Unmarshal(helmValues, &values); err != nil {
		return nil, fmt.Errorf("decode Helm release values: %w", err)
	}
	var disabled []Component
	for _, component := range componentSwitches {
		// Helm leaves a subchart installed unless its condition reads an
		// explicit boolean false, so any other shape means still enabled.
		settings, _ := values.Components[component.valuesKey].(map[string]any)
		if enabled, ok := settings["enabled"].(bool); ok && !enabled {
			disabled = append(disabled, component.name)
		}
	}
	return disabled, nil
}

// Status is the outcome of one readiness check.
type Status string

const (
	StatusPass Status = "PASS"
	StatusFail Status = "FAIL"
	StatusSkip Status = "SKIP"
)

// Result is one line in an installation validation report.
type Result struct {
	Name   string
	Status Status
	Detail string
}

// Report is a complete point-in-time TauGrid readiness report.
type Report struct {
	Results []Result
}

// Ready reports whether every required installation check passed. Checks
// skipped because their component is disabled do not block readiness.
func (r Report) Ready() bool {
	if len(r.Results) == 0 {
		return false
	}
	for _, result := range r.Results {
		if result.Status == StatusFail {
			return false
		}
	}
	return true
}

// Summary renders a concise report suitable for an infrastructure owner.
func (r Report) Summary() string {
	var b strings.Builder
	b.WriteString("TauGrid installation validation\n")
	passed, required := 0, 0
	for _, result := range r.Results {
		fmt.Fprintf(&b, "  %-4s  %-20s %s\n", result.Status, result.Name, result.Detail)
		if result.Status == StatusSkip {
			continue
		}
		required++
		if result.Status == StatusPass {
			passed++
		}
	}
	if r.Ready() {
		fmt.Fprintf(&b, "READY: %d/%d checks passed", passed, required)
	} else {
		fmt.Fprintf(&b, "NOT READY: %d/%d checks passed", passed, required)
	}
	return b.String()
}

// Err returns an actionable error when any required check failed.
func (r Report) Err() error {
	if r.Ready() {
		return nil
	}
	var failed []string
	for _, result := range r.Results {
		if result.Status == StatusFail {
			failed = append(failed, fmt.Sprintf("%s: %s", result.Name, result.Detail))
		}
	}
	if len(failed) == 0 {
		return errors.New("TauGrid installation validation produced no checks")
	}
	return fmt.Errorf("TauGrid installation is not ready: %s", strings.Join(failed, "; "))
}

// Wait polls the read-only readiness contract until it passes or the timeout
// expires. The last complete report is always returned.
func Wait(ctx context.Context, runner Runner, opts Options) (Report, error) {
	if err := validateOptions(opts); err != nil {
		return Report{}, err
	}

	waitCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	ticker := time.NewTicker(opts.PollInterval)
	defer ticker.Stop()

	for {
		report := Check(waitCtx, runner, opts)
		if report.Ready() {
			return report, nil
		}
		select {
		case <-waitCtx.Done():
			if err := ctx.Err(); err != nil {
				return report, fmt.Errorf("wait for TauGrid readiness: %w", err)
			}
			return report, fmt.Errorf("TauGrid did not become ready within %s: %w", opts.Timeout, report.Err())
		case <-ticker.C:
		}
	}
}

func validateOptions(opts Options) error {
	if strings.TrimSpace(opts.Release) == "" {
		return errors.New("release must not be empty")
	}
	if strings.TrimSpace(opts.ControlPlaneNamespace) == "" {
		return errors.New("control-plane namespace must not be empty")
	}
	if opts.Timeout <= 0 {
		return errors.New("timeout must be greater than zero")
	}
	if opts.PollInterval <= 0 {
		return errors.New("poll interval must be greater than zero")
	}
	return nil
}

// Check evaluates every required readiness surface once without mutating the
// cluster. Components the release disabled are reported as skipped.
func Check(ctx context.Context, runner Runner, opts Options) Report {
	results := make([]Result, 0, 7)

	results = append(results, checkKubernetesVersion(ctx, runner))

	deployments, listErr := getDeploymentList(ctx, runner, opts.ControlPlaneNamespace, opts.Release)
	for _, component := range chartComponents {
		name := string(component.name)
		switch {
		case slices.Contains(opts.DisabledComponents, component.name):
			results = append(results, skip(name, fmt.Sprintf("components.%s.enabled is false in the Helm release", component.valuesKey)))
		case listErr != nil:
			results = append(results, fail(name, fmt.Sprintf("%v; inspect with kubectl -n %s get deploy", listErr, opts.ControlPlaneNamespace)))
		default:
			results = append(results, checkChartDeployment(name, deployments, component.chartPrefix))
		}
	}

	if slices.Contains(opts.DisabledComponents, ComponentTauCore) {
		detail := "components.tauCoreController.enabled is false in the Helm release"
		results = append(results,
			skip("Tau controller", detail),
			skip("TauCluster", detail),
		)
	} else {
		results = append(results,
			checkTauController(ctx, runner),
			checkTauCluster(ctx, runner),
		)
	}
	results = append(results, checkBaselineQueue(ctx, runner, opts.Release))
	if slices.Contains(opts.DisabledComponents, ComponentTauCore) {
		results = append(results, skip("Quota guard", "components.tauCoreController.enabled is false in the Helm release"))
	} else {
		results = append(results, checkQuotaGuard(ctx, runner))
	}
	return Report{Results: results}
}

type versionDoc struct {
	ServerVersion struct {
		Major      string `json:"major"`
		Minor      string `json:"minor"`
		GitVersion string `json:"gitVersion"`
	} `json:"serverVersion"`
}

func checkKubernetesVersion(ctx context.Context, runner Runner) Result {
	var doc versionDoc
	if err := getJSON(ctx, runner, []string{"version", "--output=json"}, &doc); err != nil {
		return fail("Kubernetes", fmt.Sprintf("%v; verify kubectl access and the selected context", err))
	}
	major, err := numericVersionPart(doc.ServerVersion.Major)
	if err != nil {
		return fail("Kubernetes", fmt.Sprintf("cannot parse server major version %q", doc.ServerVersion.Major))
	}
	minor, err := numericVersionPart(doc.ServerVersion.Minor)
	if err != nil {
		return fail("Kubernetes", fmt.Sprintf("cannot parse server minor version %q", doc.ServerVersion.Minor))
	}
	version := doc.ServerVersion.GitVersion
	if version == "" {
		version = fmt.Sprintf("v%d.%d", major, minor)
	}
	if major < minimumKubernetesMajor || major == minimumKubernetesMajor && minor < minimumKubernetesMinor {
		return fail("Kubernetes", fmt.Sprintf("%s is unsupported; upgrade the cluster to Kubernetes 1.30 or newer", version))
	}
	return pass("Kubernetes", fmt.Sprintf("%s (supported)", version))
}

func numericVersionPart(value string) (int, error) {
	end := 0
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, fmt.Errorf("no numeric prefix")
	}
	return strconv.Atoi(value[:end])
}

type deploymentDoc struct {
	Metadata struct {
		Name       string            `json:"name"`
		Generation int64             `json:"generation"`
		Labels     map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Replicas int32 `json:"replicas"`
	} `json:"spec"`
	Status struct {
		ObservedGeneration int64 `json:"observedGeneration"`
		ReadyReplicas      int32 `json:"readyReplicas"`
		AvailableReplicas  int32 `json:"availableReplicas"`
		UpdatedReplicas    int32 `json:"updatedReplicas"`
	} `json:"status"`
}

type deploymentListDoc struct {
	Items []deploymentDoc `json:"items"`
}

func getDeploymentList(ctx context.Context, runner Runner, namespace, release string) ([]deploymentDoc, error) {
	var doc deploymentListDoc
	args := []string{
		"get", "deployments",
		"--namespace", namespace,
		"--selector", "app.kubernetes.io/instance=" + release,
		"--output=json",
	}
	if err := getJSON(ctx, runner, args, &doc); err != nil {
		return nil, fmt.Errorf("list Helm release deployments: %w", err)
	}
	return doc.Items, nil
}

func checkChartDeployment(component string, deployments []deploymentDoc, chartPrefix string) Result {
	var matches []deploymentDoc
	for _, deployment := range deployments {
		if strings.HasPrefix(deployment.Metadata.Labels["helm.sh/chart"], chartPrefix) {
			matches = append(matches, deployment)
		}
	}
	if len(matches) == 0 {
		return fail(component, "required Deployment was not found in the Helm release; inspect Helm values and release status")
	}
	if len(matches) > 1 {
		return fail(component, fmt.Sprintf("found %d matching Deployments; expected exactly one", len(matches)))
	}
	return deploymentReadinessResult(component, matches[0])
}

func checkTauController(ctx context.Context, runner Runner) Result {
	var deployment deploymentDoc
	args := []string{"get", "deployment", tauControllerName, "--namespace", tauPlatformNamespace, "--output=json"}
	if err := getJSON(ctx, runner, args, &deployment); err != nil {
		return fail("Tau controller", fmt.Sprintf("%v; inspect with kubectl -n %s get deploy,pods", err, tauPlatformNamespace))
	}
	return deploymentReadinessResult("Tau controller", deployment)
}

func deploymentReadinessResult(component string, deployment deploymentDoc) Result {
	desired := deployment.Spec.Replicas
	if desired < 1 {
		return fail(component, fmt.Sprintf("Deployment %s has no desired replicas", deployment.Metadata.Name))
	}
	if deployment.Status.ObservedGeneration < deployment.Metadata.Generation ||
		deployment.Status.ReadyReplicas != desired ||
		deployment.Status.AvailableReplicas != desired ||
		deployment.Status.UpdatedReplicas != desired {
		return fail(component, fmt.Sprintf(
			"Deployment %s is not ready (desired=%d updated=%d ready=%d available=%d); inspect its pods and events",
			deployment.Metadata.Name,
			desired,
			deployment.Status.UpdatedReplicas,
			deployment.Status.ReadyReplicas,
			deployment.Status.AvailableReplicas,
		))
	}
	return pass(component, fmt.Sprintf("Deployment %s is available (%d/%d)", deployment.Metadata.Name, deployment.Status.AvailableReplicas, desired))
}

type conditionDoc struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
}

type tauClusterDoc struct {
	Metadata struct {
		Generation int64 `json:"generation"`
	} `json:"metadata"`
	Status struct {
		Phase              string         `json:"phase"`
		ObservedGeneration int64          `json:"observedGeneration"`
		Conditions         []conditionDoc `json:"conditions"`
	} `json:"status"`
}

func checkTauCluster(ctx context.Context, runner Runner) Result {
	var cluster tauClusterDoc
	args := []string{"get", "clusters.tau.azure.com", tauClusterName, "--output=json"}
	if err := getJSON(ctx, runner, args, &cluster); err != nil {
		return fail("TauCluster", fmt.Sprintf("%v; inspect with kubectl get clusters.tau.azure.com %s -o yaml", err, tauClusterName))
	}
	if cluster.Status.ObservedGeneration < cluster.Metadata.Generation {
		return fail("TauCluster", fmt.Sprintf(
			"controller has not observed generation %d (observed %d); inspect tau-core-controller logs",
			cluster.Metadata.Generation,
			cluster.Status.ObservedGeneration,
		))
	}
	if cluster.Status.Phase == "Degraded" {
		return fail("TauCluster", "phase is Degraded; inspect status conditions and tau-core-controller logs")
	}
	condition, ok := findCondition(cluster.Status.Conditions, "NodesReady")
	if !ok {
		return fail("TauCluster", "NodesReady condition is missing; inspect tau-core-controller logs")
	}
	if condition.ObservedGeneration < cluster.Metadata.Generation || condition.Status != "True" {
		detail := strings.TrimSpace(strings.Join([]string{condition.Reason, condition.Message}, ": "))
		if detail == "" {
			detail = "condition is not True"
		}
		return fail("TauCluster", fmt.Sprintf("NodesReady=%s (%s); review TauCluster node label rules and matching nodes", condition.Status, detail))
	}
	return pass("TauCluster", fmt.Sprintf("generation %d observed; NodesReady=True", cluster.Metadata.Generation))
}

type clusterQueueListDoc struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Status struct {
			Conditions []conditionDoc `json:"conditions"`
		} `json:"status"`
	} `json:"items"`
}

func checkBaselineQueue(ctx context.Context, runner Runner, release string) Result {
	var queues clusterQueueListDoc
	args := []string{
		"get", "clusterqueues",
		"--selector", "app.kubernetes.io/instance=" + release + ",app.kubernetes.io/part-of=taugrid",
		"--output=json",
	}
	if err := getJSON(ctx, runner, args, &queues); err != nil {
		return fail("Baseline queue", fmt.Sprintf("%v; inspect with kubectl get clusterqueues", err))
	}
	if len(queues.Items) != 1 {
		return fail("Baseline queue", fmt.Sprintf("found %d Helm-owned ClusterQueues; expected exactly one", len(queues.Items)))
	}
	queue := queues.Items[0]
	active, ok := findCondition(queue.Status.Conditions, "Active")
	if !ok || active.Status != "True" {
		detail := "Active condition is missing"
		if ok {
			detail = strings.TrimSpace(strings.Join([]string{active.Reason, active.Message}, ": "))
		}
		return fail("Baseline queue", fmt.Sprintf("ClusterQueue %s is not Active (%s); inspect its status", queue.Metadata.Name, detail))
	}
	return pass("Baseline queue", fmt.Sprintf("ClusterQueue %s is Active", queue.Metadata.Name))
}

type quotaPolicyDoc struct {
	Spec struct {
		FailurePolicy    string `json:"failurePolicy"`
		MatchConstraints struct {
			ResourceRules []struct {
				APIGroups   []string `json:"apiGroups"`
				APIVersions []string `json:"apiVersions"`
				Operations  []string `json:"operations"`
				Resources   []string `json:"resources"`
			} `json:"resourceRules"`
		} `json:"matchConstraints"`
	} `json:"spec"`
}

type quotaBindingDoc struct {
	Spec struct {
		PolicyName        string   `json:"policyName"`
		ValidationActions []string `json:"validationActions"`
	} `json:"spec"`
}

func checkQuotaGuard(ctx context.Context, runner Runner) Result {
	var policy quotaPolicyDoc
	policyArgs := []string{"get", "validatingadmissionpolicy", quotaGuardName, "--output=json"}
	if err := getJSON(ctx, runner, policyArgs, &policy); err != nil {
		return fail("Quota guard", fmt.Sprintf("%v; the quota decision guard must be installed", err))
	}
	if policy.Spec.FailurePolicy != "Fail" {
		return fail("Quota guard", fmt.Sprintf("policy failurePolicy=%q; expected Fail", policy.Spec.FailurePolicy))
	}
	rules := policy.Spec.MatchConstraints.ResourceRules
	if len(rules) != 1 ||
		!sameStringSet(rules[0].APIGroups, []string{"tau.azure.com"}) ||
		!sameStringSet(rules[0].APIVersions, []string{"v1alpha1"}) ||
		!sameStringSet(rules[0].Operations, []string{"CREATE", "UPDATE"}) ||
		!sameStringSet(rules[0].Resources, []string{"quotarequests"}) {
		return fail("Quota guard", "policy scope is broader than TauQuotaRequest CREATE/UPDATE; restore the chart's narrow resource rule")
	}

	var binding quotaBindingDoc
	bindingArgs := []string{"get", "validatingadmissionpolicybinding", quotaGuardName, "--output=json"}
	if err := getJSON(ctx, runner, bindingArgs, &binding); err != nil {
		return fail("Quota guard", fmt.Sprintf("%v; the quota decision guard binding must be installed", err))
	}
	if binding.Spec.PolicyName != quotaGuardName || !contains(binding.Spec.ValidationActions, "Deny") {
		return fail("Quota guard", fmt.Sprintf(
			"binding policyName=%q validationActions=%v; expected %s with Deny",
			binding.Spec.PolicyName,
			binding.Spec.ValidationActions,
			quotaGuardName,
		))
	}
	return pass("Quota guard", "ValidatingAdmissionPolicy fails closed and binding enforces Deny")
}

func findCondition(conditions []conditionDoc, conditionType string) (conditionDoc, bool) {
	for _, condition := range conditions {
		if condition.Type == conditionType {
			return condition, true
		}
	}
	return conditionDoc{}, false
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func sameStringSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for _, value := range want {
		if !contains(got, value) {
			return false
		}
	}
	return true
}

func getJSON(ctx context.Context, runner Runner, args []string, target any) error {
	output, err := runner.Raw(ctx, args, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(output), target); err != nil {
		return fmt.Errorf("decode kubectl output for %q: %w", strings.Join(args, " "), err)
	}
	return nil
}

func pass(name, detail string) Result {
	return Result{Name: name, Status: StatusPass, Detail: detail}
}

func fail(name, detail string) Result {
	return Result{Name: name, Status: StatusFail, Detail: detail}
}

func skip(name, detail string) Result {
	return Result{Name: name, Status: StatusSkip, Detail: detail}
}
