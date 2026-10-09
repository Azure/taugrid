// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
)

const completionMarker = "TAUGRID_NIGHTLY_SMOKE_COMPLETE"

var (
	tauClusterGVR = schema.GroupVersionResource{
		Group: "tau.azure.com", Version: "v1alpha1", Resource: "clusters",
	}
	tauWorkspaceGVR = schema.GroupVersionResource{
		Group: "tau.azure.com", Version: "v1alpha1", Resource: "workspaces",
	}
	rayJobGVR = schema.GroupVersionResource{
		Group: "ray.io", Version: "v1", Resource: "rayjobs",
	}
	workloadGVR = schema.GroupVersionResource{
		Group: "kueue.x-k8s.io", Version: "v1beta2", Resource: "workloads",
	}
	podGVR = schema.GroupVersionResource{
		Group: "", Version: "v1", Resource: "pods",
	}
)

type options struct {
	tauBinary        string
	contextName      string
	systemNamespace  string
	workspace        string
	profile          string
	cpuFlavor        string
	queue            string
	principalName    string
	sourceDir        string
	artifactDir      string
	runName          string
	profileTimeout   time.Duration
	workspaceTimeout time.Duration
	topologyTimeout  time.Duration
	lifecycleTimeout time.Duration
	logsTimeout      time.Duration
	cleanupTimeout   time.Duration
	pollInterval     time.Duration
}

func (o options) validate() error {
	required := map[string]string{
		"tau":              o.tauBinary,
		"context":          o.contextName,
		"system-namespace": o.systemNamespace,
		"workspace":        o.workspace,
		"profile":          o.profile,
		"cpu-flavor":       o.cpuFlavor,
		"queue":            o.queue,
		"principal-name":   o.principalName,
		"source-dir":       o.sourceDir,
		"artifact-dir":     o.artifactDir,
		"run-name":         o.runName,
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("--%s is required", name)
		}
	}
	if problems := validation.IsDNS1123Label(o.runName); len(problems) > 0 {
		return fmt.Errorf("--run-name must be a DNS label: %s", strings.Join(problems, ", "))
	}
	for name, value := range map[string]time.Duration{
		"profile-timeout":   o.profileTimeout,
		"workspace-timeout": o.workspaceTimeout,
		"topology-timeout":  o.topologyTimeout,
		"lifecycle-timeout": o.lifecycleTimeout,
		"logs-timeout":      o.logsTimeout,
		"cleanup-timeout":   o.cleanupTimeout,
		"poll-interval":     o.pollInterval,
	} {
		if value <= 0 {
			return fmt.Errorf("--%s must be greater than zero", name)
		}
	}
	return nil
}

type result struct {
	Status         string            `json:"status"`
	Reason         string            `json:"reason"`
	RunName        string            `json:"run_name"`
	Workspace      string            `json:"workspace"`
	Profile        string            `json:"profile"`
	LifecycleState string            `json:"lifecycle_state"`
	ConfigSHA256   string            `json:"config_sha256"`
	Topology       *topologyEvidence `json:"topology,omitempty"`
}

type topologyEvidence struct {
	Level           string           `json:"level"`
	Flavor          string           `json:"flavor"`
	ClusterQueue    string           `json:"cluster_queue"`
	Workload        string           `json:"workload"`
	PodSet          string           `json:"pod_set"`
	AssignedWorkers int64            `json:"assigned_workers"`
	Domains         []topologyDomain `json:"domains"`
	PodNodes        []string         `json:"pod_nodes"`
}

type topologyDomain struct {
	Values []string `json:"values"`
	Count  int64    `json:"count"`
}

type commandExecutor interface {
	run(ctx context.Context, stdout, stderr io.Writer, executable string, args ...string) error
}

type osCommandExecutor struct{}

func (osCommandExecutor) run(ctx context.Context, stdout, stderr io.Writer, executable string, args ...string) error {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

type clusterAPI interface {
	ensureProfile(ctx context.Context, profile, workspace string) error
	waitProfileReady(ctx context.Context, profile string, timeout time.Duration) error
	waitWorkspaceReady(ctx context.Context, namespace, workspace string, timeout time.Duration) error
	waitTopologyVerified(
		ctx context.Context,
		namespace, runName, expectedFlavor, expectedQueue string,
		workers int64,
		timeout time.Duration,
	) (topologyEvidence, error)
	deleteRayJob(ctx context.Context, namespace, name string, timeout time.Duration) error
}

type kubernetesClient struct {
	dynamicClient dynamic.Interface
	pollInterval  time.Duration
}

func newKubernetesClient(dynamicClient dynamic.Interface, pollInterval time.Duration) *kubernetesClient {
	return &kubernetesClient{dynamicClient: dynamicClient, pollInterval: pollInterval}
}

func (k *kubernetesClient) ensureProfile(ctx context.Context, profile, workspace string) error {
	cluster, err := k.dynamicClient.Resource(tauClusterGVR).Get(ctx, "cluster", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting TauCluster cluster: %w", err)
	}
	profiles, found, err := unstructured.NestedSlice(cluster.Object, "spec", "workloadProfiles")
	if err != nil {
		return fmt.Errorf("reading TauCluster workload profiles: %w", err)
	}
	if found {
		for _, item := range profiles {
			entry, ok := item.(map[string]any)
			if ok && entry["name"] == profile {
				placement, _, _ := unstructured.NestedString(entry, "placement")
				workers, _, _ := unstructured.NestedInt64(entry, "workerCount")
				gpus, _, _ := unstructured.NestedInt64(entry, "gpusPerWorker")
				if placement != "same-host" || workers != 2 || gpus != 0 {
					return fmt.Errorf(
						"workload profile %s exists with placement=%q workerCount=%d gpusPerWorker=%d; "+
							"expected same-host, 2 workers, and 0 GPUs",
						profile, placement, workers, gpus,
					)
				}
				return nil
			}
		}
	}

	newProfile := map[string]any{
		"name":        profile,
		"description": "Unbounded stable CPU RayJob smoke profile",
		"applicability": map[string]any{
			"teams":      []any{"research"},
			"lanes":      []any{"training"},
			"namespaces": []any{workspace},
		},
		"gpusPerWorker":     int64(0),
		"workerCount":       int64(2),
		"mode":              "fixed",
		"placement":         "same-host",
		"defaultLocalQueue": "jobqueue",
		"executionTarget":   "singleCluster",
		"priorities": map[string]any{
			"workloadPriorityClassName": "taugrid-default",
			"podPriorityClassName":      "taugrid-default",
		},
	}
	path := "/spec/workloadProfiles"
	value := any([]any{newProfile})
	if found {
		path = "/spec/workloadProfiles/-"
		value = newProfile
	}
	patch, err := json.Marshal([]map[string]any{{
		"op":    "add",
		"path":  path,
		"value": value,
	}})
	if err != nil {
		return fmt.Errorf("encoding workload profile patch: %w", err)
	}
	if _, err := k.dynamicClient.Resource(tauClusterGVR).Patch(
		ctx, "cluster", types.JSONPatchType, patch, metav1.PatchOptions{},
	); err != nil {
		return fmt.Errorf("adding workload profile %s: %w", profile, err)
	}
	return nil
}

func (k *kubernetesClient) waitProfileReady(ctx context.Context, profile string, timeout time.Duration) error {
	var lastState string
	err := wait.PollUntilContextTimeout(ctx, k.pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		cluster, err := k.dynamicClient.Resource(tauClusterGVR).Get(ctx, "cluster", metav1.GetOptions{})
		if err != nil {
			lastState = err.Error()
			return false, clientReadinessError(err)
		}
		profiles, _, err := unstructured.NestedSlice(cluster.Object, "status", "workloadProfiles", "profiles")
		if err != nil {
			return false, fmt.Errorf("reading resolved workload profiles: %w", err)
		}
		for _, item := range profiles {
			entry, ok := item.(map[string]any)
			if !ok || entry["name"] != profile {
				continue
			}
			conditions, _, err := unstructured.NestedSlice(entry, "conditions")
			if err != nil {
				return false, fmt.Errorf("reading workload profile conditions: %w", err)
			}
			for _, condition := range conditions {
				fields, ok := condition.(map[string]any)
				if !ok || fields["type"] != "Ready" {
					continue
				}
				lastState = fmt.Sprintf("Ready=%v reason=%v message=%v", fields["status"], fields["reason"], fields["message"])
				return fields["status"] == "True", nil
			}
			lastState = "profile exists without a Ready condition"
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for workload profile %s to become Ready (last state: %s): %w", profile, lastState, err)
	}
	return nil
}

func (k *kubernetesClient) waitWorkspaceReady(
	ctx context.Context, namespace, workspace string, timeout time.Duration,
) error {
	var lastState string
	err := wait.PollUntilContextTimeout(ctx, k.pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		object, err := k.dynamicClient.Resource(tauWorkspaceGVR).Namespace(namespace).
			Get(ctx, workspace, metav1.GetOptions{})
		if err != nil {
			lastState = err.Error()
			return false, clientReadinessError(err)
		}
		phase, _, err := unstructured.NestedString(object.Object, "status", "phase")
		if err != nil {
			return false, fmt.Errorf("reading workspace phase: %w", err)
		}
		lastState = phase
		return phase == "Ready", nil
	})
	if err != nil {
		return fmt.Errorf("waiting for workspace %s/%s to become Ready (last phase: %s): %w",
			namespace, workspace, lastState, err)
	}
	return nil
}

func (k *kubernetesClient) waitTopologyVerified(
	ctx context.Context,
	namespace, runName, expectedFlavor, expectedQueue string,
	workers int64,
	timeout time.Duration,
) (topologyEvidence, error) {
	var evidence topologyEvidence
	var lastState string
	err := wait.PollUntilContextTimeout(ctx, k.pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		rayJob, err := k.dynamicClient.Resource(rayJobGVR).Namespace(namespace).
			Get(ctx, runName, metav1.GetOptions{})
		if err != nil {
			lastState = "waiting for RayJob: " + err.Error()
			return false, clientReadinessError(err)
		}
		workload, found, err := k.findRayJobWorkload(ctx, namespace, runName, string(rayJob.GetUID()))
		if err != nil {
			return false, err
		}
		if !found {
			lastState = "waiting for the Kueue Workload"
			return false, nil
		}
		workerPodSet, err := rayJobWorkerPodSet(rayJob, workers)
		if err != nil {
			return false, err
		}
		parsed, found, err := topologyEvidenceFromWorkload(
			workload, workerPodSet, expectedFlavor, expectedQueue, workers,
		)
		if err != nil {
			return false, err
		}
		if !found {
			lastState = "waiting for the worker topology assignment"
			return false, nil
		}

		rayClusterName, _, err := unstructured.NestedString(rayJob.Object, "status", "rayClusterName")
		if err != nil {
			return false, fmt.Errorf("reading RayJob RayCluster name: %w", err)
		}
		if rayClusterName == "" {
			lastState = "waiting for the RayCluster name"
			return false, nil
		}
		pods, err := k.dynamicClient.Resource(podGVR).Namespace(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("ray.io/cluster=%s,ray.io/node-type=worker", rayClusterName),
		})
		if err != nil {
			lastState = "waiting for Ray worker pods: " + err.Error()
			return false, clientReadinessError(err)
		}
		if int64(len(pods.Items)) != workers {
			lastState = fmt.Sprintf("waiting for %d Ray worker pods; observed %d", workers, len(pods.Items))
			return false, nil
		}

		allowedNodes := make(map[string]struct{})
		for _, domain := range parsed.Domains {
			if len(domain.Values) != 1 {
				return false, fmt.Errorf(
					"hostname topology domain has %d values, want exactly one: %v",
					len(domain.Values), domain.Values,
				)
			}
			allowedNodes[domain.Values[0]] = struct{}{}
		}
		parsed.PodNodes = parsed.PodNodes[:0]
		for i := range pods.Items {
			nodeName, _, err := unstructured.NestedString(pods.Items[i].Object, "spec", "nodeName")
			if err != nil {
				return false, fmt.Errorf("reading worker pod %s node: %w", pods.Items[i].GetName(), err)
			}
			if nodeName == "" {
				lastState = fmt.Sprintf("waiting for worker pod %s scheduling", pods.Items[i].GetName())
				return false, nil
			}
			if _, ok := allowedNodes[nodeName]; !ok {
				return false, fmt.Errorf(
					"worker pod %s ran on node %s outside Kueue topology domains %v",
					pods.Items[i].GetName(), nodeName, sortedKeys(allowedNodes),
				)
			}
			parsed.PodNodes = append(parsed.PodNodes, nodeName)
		}
		sort.Strings(parsed.PodNodes)
		evidence = parsed
		return true, nil
	})
	if err != nil {
		return topologyEvidence{}, fmt.Errorf(
			"verifying topology-aware admission for Tau run %s (last state: %s): %w",
			runName, lastState, err,
		)
	}
	return evidence, nil
}

func (k *kubernetesClient) findRayJobWorkload(
	ctx context.Context, namespace, runName, runUID string,
) (*unstructured.Unstructured, bool, error) {
	workloads, err := k.dynamicClient.Resource(workloadGVR).Namespace(namespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("listing Kueue Workloads: %w", err)
	}
	for i := range workloads.Items {
		for _, owner := range workloads.Items[i].GetOwnerReferences() {
			if owner.Kind == "RayJob" && owner.Name == runName &&
				(runUID == "" || string(owner.UID) == runUID) {
				return workloads.Items[i].DeepCopy(), true, nil
			}
		}
	}
	return nil, false, nil
}

func topologyEvidenceFromWorkload(
	workload *unstructured.Unstructured,
	expectedPodSet string,
	expectedFlavor, expectedQueue string,
	workers int64,
) (topologyEvidence, bool, error) {
	admitted, conditionFound, err := workloadCondition(workload, "Admitted")
	if err != nil {
		return topologyEvidence{}, false, err
	}
	if !conditionFound || admitted != "True" {
		return topologyEvidence{}, false, nil
	}
	clusterQueue, found, err := unstructured.NestedString(
		workload.Object, "status", "admission", "clusterQueue",
	)
	if err != nil {
		return topologyEvidence{}, false, fmt.Errorf("reading admitting ClusterQueue: %w", err)
	}
	if !found || clusterQueue != expectedQueue {
		return topologyEvidence{}, false, fmt.Errorf(
			"workload was admitted by ClusterQueue %q, want %q",
			clusterQueue, expectedQueue,
		)
	}
	assignments, found, err := unstructured.NestedSlice(
		workload.Object, "status", "admission", "podSetAssignments",
	)
	if err != nil {
		return topologyEvidence{}, false, fmt.Errorf("reading Kueue PodSetAssignments: %w", err)
	}
	if !found {
		return topologyEvidence{}, false, nil
	}
	for _, rawAssignment := range assignments {
		assignment, ok := rawAssignment.(map[string]any)
		if !ok {
			continue
		}
		name := fmt.Sprint(assignment["name"])
		if name != expectedPodSet {
			continue
		}
		count, ok := assignment["count"].(int64)
		if !ok || count != workers {
			return topologyEvidence{}, false, fmt.Errorf(
				"worker PodSetAssignment %s count is %v, want %d",
				name, assignment["count"], workers,
			)
		}
		flavors, found, err := unstructured.NestedStringMap(assignment, "flavors")
		if err != nil {
			return topologyEvidence{}, false, fmt.Errorf("reading worker ResourceFlavors: %w", err)
		}
		if !found {
			return topologyEvidence{}, false, fmt.Errorf("worker PodSetAssignment has no ResourceFlavors")
		}
		for _, resourceName := range []string{"cpu", "memory"} {
			if flavors[resourceName] != expectedFlavor {
				return topologyEvidence{}, false, fmt.Errorf(
					"worker %s flavor is %q, want %q",
					resourceName, flavors[resourceName], expectedFlavor,
				)
			}
		}
		topology, ok := assignment["topologyAssignment"].(map[string]any)
		if !ok {
			return topologyEvidence{}, false, nil
		}
		levelsRaw, ok := topology["levels"].([]any)
		if !ok || len(levelsRaw) != 1 || fmt.Sprint(levelsRaw[0]) != "kubernetes.io/hostname" {
			return topologyEvidence{}, false, fmt.Errorf(
				"worker topology levels are %v, want [kubernetes.io/hostname]",
				topology["levels"],
			)
		}
		slicesRaw, ok := topology["slices"].([]any)
		if !ok || len(slicesRaw) != 1 {
			return topologyEvidence{}, false, fmt.Errorf(
				"same-host worker assignment has %d topology slices, want exactly one",
				len(slicesRaw),
			)
		}
		evidence := topologyEvidence{
			Level:        "kubernetes.io/hostname",
			Flavor:       expectedFlavor,
			ClusterQueue: clusterQueue,
			Workload:     workload.GetName(),
			PodSet:       name,
		}
		slice, ok := slicesRaw[0].(map[string]any)
		if !ok {
			return topologyEvidence{}, false, fmt.Errorf("worker topology slice is not an object")
		}
		domainCount, ok := slice["domainCount"].(int64)
		if !ok || domainCount != 1 {
			return topologyEvidence{}, false, fmt.Errorf(
				"same-host topology slice domainCount is %v, want 1",
				slice["domainCount"],
			)
		}
		podCounts, ok := slice["podCounts"].(map[string]any)
		if !ok {
			return topologyEvidence{}, false, fmt.Errorf("worker topology slice has no podCounts")
		}
		for _, rawCount := range podCounts {
			podCount, ok := rawCount.(int64)
			if !ok {
				return topologyEvidence{}, false, fmt.Errorf(
					"worker topology pod count has type %T, want int64",
					rawCount,
				)
			}
			evidence.AssignedWorkers += podCount
		}
		valuesPerLevel, ok := slice["valuesPerLevel"].([]any)
		if !ok || len(valuesPerLevel) != 1 {
			return topologyEvidence{}, false, fmt.Errorf(
				"worker topology valuesPerLevel is %v, want one hostname level",
				slice["valuesPerLevel"],
			)
		}
		levelValues, ok := valuesPerLevel[0].(map[string]any)
		if !ok {
			return topologyEvidence{}, false, fmt.Errorf("worker hostname values are not an object")
		}
		hostnames := make(map[string]struct{})
		for _, rawValue := range levelValues {
			hostname := strings.TrimSpace(fmt.Sprint(rawValue))
			if hostname != "" {
				hostnames[hostname] = struct{}{}
			}
		}
		if len(hostnames) != 1 {
			return topologyEvidence{}, false, fmt.Errorf(
				"same-host topology slice assigned hostnames %v, want exactly one",
				sortedKeys(hostnames),
			)
		}
		evidence.Domains = append(evidence.Domains, topologyDomain{
			Values: sortedKeys(hostnames),
			Count:  evidence.AssignedWorkers,
		})
		if evidence.AssignedWorkers != workers {
			return topologyEvidence{}, false, fmt.Errorf(
				"worker topology assignment covers %d workers, want %d",
				evidence.AssignedWorkers, workers,
			)
		}
		return evidence, true, nil
	}
	return topologyEvidence{}, false, nil
}

func rayJobWorkerPodSet(rayJob *unstructured.Unstructured, workers int64) (string, error) {
	groups, found, err := unstructured.NestedSlice(
		rayJob.Object, "spec", "rayClusterSpec", "workerGroupSpecs",
	)
	if err != nil {
		return "", fmt.Errorf("reading RayJob worker groups: %w", err)
	}
	if !found {
		return "", fmt.Errorf("RayJob %s has no workerGroupSpecs", rayJob.GetName())
	}
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		replicas, _, err := unstructured.NestedInt64(group, "replicas")
		if err != nil {
			return "", fmt.Errorf("reading RayJob worker replicas: %w", err)
		}
		if replicas != workers {
			continue
		}
		groupName, _, err := unstructured.NestedString(group, "groupName")
		if err != nil {
			return "", fmt.Errorf("reading RayJob worker group name: %w", err)
		}
		if groupName == "" {
			return "", fmt.Errorf("RayJob worker group for %d workers has no groupName", workers)
		}
		return groupName, nil
	}
	return "", fmt.Errorf("RayJob %s has no worker group with %d replicas", rayJob.GetName(), workers)
}

func workloadCondition(workload *unstructured.Unstructured, conditionType string) (string, bool, error) {
	conditions, found, err := unstructured.NestedSlice(workload.Object, "status", "conditions")
	if err != nil {
		return "", false, fmt.Errorf("reading Workload conditions: %w", err)
	}
	if !found {
		return "", false, nil
	}
	for _, rawCondition := range conditions {
		condition, ok := rawCondition.(map[string]any)
		if ok && condition["type"] == conditionType {
			return fmt.Sprint(condition["status"]), true, nil
		}
	}
	return "", false, nil
}

func sortedKeys(values map[string]struct{}) []string {
	keys := make([]string, 0, len(values))
	for value := range values {
		keys = append(keys, value)
	}
	sort.Strings(keys)
	return keys
}

func clientReadinessError(err error) error {
	if apierrors.IsNotFound(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) {
		return nil
	}
	return err
}

func (k *kubernetesClient) deleteRayJob(
	ctx context.Context, namespace, name string, timeout time.Duration,
) error {
	resource := k.dynamicClient.Resource(rayJobGVR).Namespace(namespace)
	propagation := metav1.DeletePropagationForeground
	if err := resource.Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting RayJob %s/%s: %w", namespace, name, err)
	}
	err := wait.PollUntilContextTimeout(ctx, k.pollInterval, timeout, true, func(ctx context.Context) (bool, error) {
		_, err := resource.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("waiting for RayJob %s/%s deletion: %w", namespace, name, err)
	}
	return nil
}

type runner struct {
	options  options
	commands commandExecutor
	cluster  clusterAPI
	stderr   io.Writer
}

func newRunner(opts options, commands commandExecutor, cluster clusterAPI) *runner {
	return &runner{options: opts, commands: commands, cluster: cluster, stderr: os.Stderr}
}

func (r *runner) execute(ctx context.Context) error {
	if err := os.MkdirAll(r.options.artifactDir, 0o755); err != nil {
		return fmt.Errorf("creating artifact directory: %w", err)
	}
	outcome, runErr := r.run(ctx)
	if runErr != nil {
		outcome.Status = "failed"
		outcome.Reason = runErr.Error()
	}
	if err := writeJSONAtomic(r.resultPath(), outcome); err != nil {
		return errors.Join(runErr, fmt.Errorf("writing smoke result: %w", err))
	}
	return runErr
}

func (r *runner) run(ctx context.Context) (result, error) {
	outcome := result{
		Status:         "failed",
		RunName:        r.options.runName,
		Workspace:      r.options.workspace,
		Profile:        r.options.profile,
		LifecycleState: "unknown",
	}

	configPath, configHash, err := r.prepareWorkload()
	if err != nil {
		return outcome, fmt.Errorf("preparing workload: %w", err)
	}
	outcome.ConfigSHA256 = configHash

	if err := r.cluster.ensureProfile(ctx, r.options.profile, r.options.workspace); err != nil {
		return outcome, err
	}
	if err := r.cluster.waitProfileReady(ctx, r.options.profile, r.options.profileTimeout); err != nil {
		return outcome, err
	}
	if err := r.runTauToFile(ctx, r.artifactPath("workspace-create.txt"),
		"workspace", "create", r.options.workspace,
		"--principal-name", r.options.principalName,
		"--context", r.options.contextName,
		"--system-namespace", r.options.systemNamespace,
		"--apply",
	); err != nil {
		return outcome, fmt.Errorf("creating workspace through Tau: %w", err)
	}
	if err := r.cluster.waitWorkspaceReady(
		ctx, r.options.systemNamespace, r.options.workspace, r.options.workspaceTimeout,
	); err != nil {
		return outcome, err
	}

	if err := r.runTauToFile(ctx, r.artifactPath("validate.txt"),
		"run", "validate", "--config", configPath,
	); err != nil {
		return outcome, fmt.Errorf("validating Tau config: %w", err)
	}
	if err := r.runTauToFile(ctx, r.artifactPath("client-dry-run.yaml"),
		"run", "--config", configPath,
		"--context", r.options.contextName,
		"--workspace", r.options.workspace,
		"--dry-run=client",
	); err != nil {
		return outcome, fmt.Errorf("running Tau client dry-run: %w", err)
	}
	if err := r.runTauToFile(ctx, r.artifactPath("server-dry-run.txt"),
		"run", "--config", configPath,
		"--context", r.options.contextName,
		"--workspace", r.options.workspace,
		"--dry-run=server",
	); err != nil {
		return outcome, fmt.Errorf("running Tau server dry-run: %w", err)
	}
	if err := r.runTauToFile(ctx, r.artifactPath("submission.txt"),
		"run", "--config", configPath,
		"--context", r.options.contextName,
		"--workspace", r.options.workspace,
	); err != nil {
		return outcome, fmt.Errorf("submitting Tau run: %w", err)
	}

	logsCtx, cancelLogs := context.WithTimeout(ctx, r.options.logsTimeout)
	logsDone := make(chan error, 1)
	go func() {
		logsDone <- r.captureLogs(logsCtx)
	}()
	logsFinished := false
	defer func() {
		cancelLogs()
		if !logsFinished {
			<-logsDone
		}
	}()

	topology, err := r.cluster.waitTopologyVerified(
		ctx,
		r.options.workspace,
		r.options.runName,
		r.options.cpuFlavor,
		r.options.queue,
		2,
		r.options.topologyTimeout,
	)
	if err != nil {
		return outcome, err
	}
	outcome.Topology = &topology
	if err := writeJSONAtomic(r.artifactPath("topology.json"), topology); err != nil {
		return outcome, fmt.Errorf("writing topology evidence: %w", err)
	}

	state, err := r.waitRunState(ctx)
	outcome.LifecycleState = state
	if err != nil {
		return outcome, err
	}

	logsErr := <-logsDone
	logsFinished = true
	cancelLogs()
	logData, err := os.ReadFile(r.artifactPath("logs.txt"))
	if err != nil {
		return outcome, fmt.Errorf("reading Tau logs: %w", err)
	}
	if !bytes.Contains(logData, []byte(completionMarker)) {
		if logsErr != nil {
			return outcome, fmt.Errorf("Tau logs did not contain %s: %w", completionMarker, logsErr)
		}
		return outcome, fmt.Errorf("Tau logs did not contain %s", completionMarker)
	}

	cleanupCtx, cancelCleanup := context.WithTimeout(ctx, r.options.cleanupTimeout)
	defer cancelCleanup()
	if err := r.cluster.deleteRayJob(
		cleanupCtx, r.options.workspace, r.options.runName, r.options.cleanupTimeout,
	); err != nil {
		return outcome, err
	}
	if err := os.WriteFile(r.artifactPath("cleanup.txt"), []byte("RayJob deleted\n"), 0o644); err != nil {
		return outcome, fmt.Errorf("writing cleanup artifact: %w", err)
	}

	outcome.Status = "passed"
	outcome.Reason = ""
	return outcome, nil
}

func (r *runner) prepareWorkload() (string, string, error) {
	runDir := r.artifactPath("workload")
	if err := os.RemoveAll(runDir); err != nil {
		return "", "", fmt.Errorf("removing old workload directory: %w", err)
	}
	if err := copyDir(r.options.sourceDir, runDir); err != nil {
		return "", "", err
	}
	configPath := filepath.Join(runDir, "tau.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		return "", "", fmt.Errorf("reading Tau config: %w", err)
	}
	placeholder := []byte("name: __RUN_NAME__")
	if bytes.Count(config, placeholder) != 1 {
		return "", "", fmt.Errorf("Tau config must contain exactly one %q placeholder", placeholder)
	}
	config = bytes.Replace(config, placeholder, []byte("name: "+r.options.runName), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		return "", "", fmt.Errorf("writing rendered Tau config: %w", err)
	}
	sum := sha256.Sum256(config)
	return configPath, hex.EncodeToString(sum[:]), nil
}

func (r *runner) waitRunState(ctx context.Context) (string, error) {
	statusCtx, cancel := context.WithTimeout(ctx, r.options.lifecycleTimeout)
	defer cancel()
	state := "unknown"
	for {
		var buffer bytes.Buffer
		err := r.commands.run(statusCtx, &buffer, r.stderr, r.options.tauBinary,
			"run", "status", r.options.runName,
			"--context", r.options.contextName,
			"--workspace", r.options.workspace,
			"--output", "json",
		)
		if err == nil {
			if err := writeFileAtomic(r.artifactPath("status.json"), buffer.Bytes()); err != nil {
				return state, fmt.Errorf("writing Tau status: %w", err)
			}
			var status struct {
				Status struct {
					State string `json:"state"`
				} `json:"status"`
			}
			if err := json.Unmarshal(buffer.Bytes(), &status); err != nil {
				return state, fmt.Errorf("decoding Tau status: %w", err)
			}
			state = status.Status.State
			switch state {
			case "succeeded":
				return state, nil
			case "failed", "interrupted":
				return state, fmt.Errorf("Tau run reached terminal state %s", state)
			}
		} else if statusCtx.Err() == nil {
			fmt.Fprintf(r.stderr, "Tau status attempt failed: %v\n", err)
		}

		timer := time.NewTimer(r.options.pollInterval)
		select {
		case <-statusCtx.Done():
			timer.Stop()
			return state, fmt.Errorf("Tau run did not succeed before the timeout: %w", statusCtx.Err())
		case <-timer.C:
		}
	}
}

func (r *runner) captureLogs(ctx context.Context) error {
	logFile, err := os.OpenFile(r.artifactPath("logs.txt"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("creating Tau log artifact: %w", err)
	}
	defer logFile.Close()

	var lastErr error
	for {
		lastErr = r.commands.run(ctx, logFile, logFile, r.options.tauBinary,
			"logs", r.options.runName,
			"--context", r.options.contextName,
			"-n", r.options.workspace,
			"-f",
		)
		if lastErr == nil {
			return nil
		}
		timer := time.NewTimer(r.options.pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("capturing Tau logs: %w (last command error: %v)", ctx.Err(), lastErr)
		case <-timer.C:
		}
	}
}

func (r *runner) runTauToFile(ctx context.Context, path string, args ...string) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	defer file.Close()
	return r.commands.run(ctx, file, r.stderr, r.options.tauBinary, args...)
}

func (r *runner) artifactPath(name string) string {
	return filepath.Join(r.options.artifactDir, name)
}

func (r *runner) resultPath() string {
	return r.artifactPath("tau-native-smoke-result.json")
}

func copyDir(source, destination string) error {
	info, err := os.Stat(source)
	if err != nil {
		return fmt.Errorf("reading workload source directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workload source path %s is not a directory", source)
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		relative, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, relative)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeFileAtomic(path, data)
}

func writeFileAtomic(path string, data []byte) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(temp, path)
}
