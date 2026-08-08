// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// datasetKubeRunner is the minimal kubectl surface the workspace dataset Jobs
// need. It is an interface with a package-level factory so tests can inject a
// fake runner and assert the exact apply/get/logs/delete command sequence
// without a live cluster.
type datasetKubeRunner interface {
	Raw(ctx context.Context, extraArgs []string, stdin []byte) (string, error)
}

// newDatasetKubeRunner builds the real kubectl runner. Overridden in tests.
var newDatasetKubeRunner = func(kubeContext string) datasetKubeRunner {
	return kube.New(kubeContext)
}

// datasetFetchWorkspace reads a TauWorkspace by name. It is a package-level
// variable (not an inline call) so tests can supply a workspace object without
// a live cluster. The real implementation shells `kubectl get workspace`.
var datasetFetchWorkspace = func(ctx context.Context, kubeContext, namespace, name string) (tauworkspace.Workspace, error) {
	r := kube.New(kubeContext)
	raw, err := r.Raw(ctx, []string{"-n", namespace, "get", "workspace.tau.azure.com", name, "-o", "json"}, nil)
	if err != nil {
		return tauworkspace.Workspace{}, err
	}
	return tauworkspace.Parse([]byte(raw))
}

// Bounded Job safety limits. Registration and status are short control-plane
// operations; ingest gets a longer deadline because project datasets may be
// hundreds of GiB. Finished Jobs are garbage-collected.
const (
	datasetJobBackoffLimit    = 2
	datasetJobTTLSeconds      = 3600 // delete 1h after finish
	datasetJobDeadlineSeconds = 3600 // hard 1h wall-clock cap
	datasetJobWorkerContainer = "worker"
	datasetJobPollInterval    = 3 * time.Second
	datasetJobDefaultWait     = 30 * time.Minute
	datasetIngestDeadline     = 24 * 60 * 60
	datasetIngestWait         = 24*time.Hour + 5*time.Minute
)

// workspaceIdentity is the validated subset of a Ready TauWorkspace that a
// dataset Job needs: the target namespace and the workload-identity service
// account. clientID is validated present (federated identity must be wired)
// even though the pod consumes it via the ServiceAccount annotation.
type workspaceIdentity struct {
	Namespace          string
	ServiceAccountName string
	ClientID           string
}

// validateWorkspaceForJob enforces the workspace contract before any Job is
// rendered: the workspace must be Ready (phase Ready AND status observed the
// current generation), must resolve a target namespace, and must declare a
// workload-identity ServiceAccount with a client ID.
func validateWorkspaceForJob(ws tauworkspace.Workspace) (workspaceIdentity, error) {
	if !tauworkspace.Ready(ws) {
		return workspaceIdentity{}, fmt.Errorf(
			"workspace %q is not Ready (phase=%q observedGeneration=%d generation=%d); "+
				"wait for the workspace controller to reconcile it before ingesting",
			ws.Metadata.Name, ws.Status.Phase, ws.Status.ObservedGeneration, ws.Metadata.Generation,
		)
	}
	ns := strings.TrimSpace(ws.Status.Target.ResolvedNamespace)
	if ns == "" {
		ns = strings.TrimSpace(ws.Spec.Target.Namespace)
	}
	if ns == "" {
		return workspaceIdentity{}, fmt.Errorf("workspace %q has no resolved target namespace", ws.Metadata.Name)
	}
	if ws.Spec.WorkloadIdentity == nil {
		return workspaceIdentity{}, fmt.Errorf(
			"workspace %q has no workloadIdentity; a federated ServiceAccount is required "+
				"for the ingest Job to authenticate to Azure without secrets", ws.Metadata.Name)
	}
	sa := strings.TrimSpace(ws.Spec.WorkloadIdentity.ServiceAccountName)
	if sa == "" {
		return workspaceIdentity{}, fmt.Errorf("workspace %q workloadIdentity.serviceAccountName is empty", ws.Metadata.Name)
	}
	clientID := strings.TrimSpace(ws.Spec.WorkloadIdentity.ClientID)
	if clientID == "" {
		return workspaceIdentity{}, fmt.Errorf("workspace %q workloadIdentity.clientId is empty", ws.Metadata.Name)
	}
	return workspaceIdentity{Namespace: ns, ServiceAccountName: sa, ClientID: clientID}, nil
}

// --- Job manifest types (typed to guarantee the identity/security fields) ---

type k8sObjectMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace,omitempty"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

type k8sSeccompProfile struct {
	Type string `yaml:"type"`
}

type k8sCapabilities struct {
	Drop []string `yaml:"drop,omitempty"`
}

type k8sPodSecurityContext struct {
	RunAsNonRoot   *bool              `yaml:"runAsNonRoot,omitempty"`
	SeccompProfile *k8sSeccompProfile `yaml:"seccompProfile,omitempty"`
}

type k8sContainerSecurityContext struct {
	AllowPrivilegeEscalation *bool            `yaml:"allowPrivilegeEscalation"`
	ReadOnlyRootFilesystem   *bool            `yaml:"readOnlyRootFilesystem"`
	RunAsNonRoot             *bool            `yaml:"runAsNonRoot,omitempty"`
	Capabilities             *k8sCapabilities `yaml:"capabilities,omitempty"`
}

type k8sEnvVar struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type k8sVolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
}

type k8sContainer struct {
	Name            string                       `yaml:"name"`
	Image           string                       `yaml:"image"`
	Command         []string                     `yaml:"command,omitempty"`
	Args            []string                     `yaml:"args,omitempty"`
	Env             []k8sEnvVar                  `yaml:"env,omitempty"`
	SecurityContext *k8sContainerSecurityContext `yaml:"securityContext,omitempty"`
	VolumeMounts    []k8sVolumeMount             `yaml:"volumeMounts,omitempty"`
}

type k8sEmptyDir struct{}

type k8sConfigMapVolumeSource struct {
	Name string `yaml:"name"`
}

type k8sVolume struct {
	Name      string                    `yaml:"name"`
	EmptyDir  *k8sEmptyDir              `yaml:"emptyDir,omitempty"`
	ConfigMap *k8sConfigMapVolumeSource `yaml:"configMap,omitempty"`
}

type k8sPodSpec struct {
	ServiceAccountName string                 `yaml:"serviceAccountName"`
	RestartPolicy      string                 `yaml:"restartPolicy"`
	SecurityContext    *k8sPodSecurityContext `yaml:"securityContext,omitempty"`
	Containers         []k8sContainer         `yaml:"containers"`
	Volumes            []k8sVolume            `yaml:"volumes,omitempty"`
}

type k8sPodTemplate struct {
	Metadata k8sObjectMeta `yaml:"metadata"`
	Spec     k8sPodSpec    `yaml:"spec"`
}

type k8sJobSpec struct {
	BackoffLimit            int            `yaml:"backoffLimit"`
	TTLSecondsAfterFinished int            `yaml:"ttlSecondsAfterFinished"`
	ActiveDeadlineSeconds   int            `yaml:"activeDeadlineSeconds"`
	Template                k8sPodTemplate `yaml:"template"`
}

type k8sJob struct {
	APIVersion string        `yaml:"apiVersion"`
	Kind       string        `yaml:"kind"`
	Metadata   k8sObjectMeta `yaml:"metadata"`
	Spec       k8sJobSpec    `yaml:"spec"`
}

// datasetWorkerJobSpec is the render-time input for a dataset worker Job.
type datasetWorkerJobSpec struct {
	JobName     string
	Namespace   string
	ServiceAcct string
	Image       string
	DatasetName string
	Version     string
	// Command is the fixed argv prefix (e.g. tau data dataset ingest-worker NAME@VERSION).
	Command []string
	// FlagArgs are ordered flag args appended after Command.
	FlagArgs []string
	Labels   map[string]string
	// ActiveDeadlineSeconds overrides the short control-plane default.
	ActiveDeadlineSeconds int
	// ConfigMapMount, when non-nil, mounts a ConfigMap read-only into the worker
	// container (used by register to transport the immutable record).
	ConfigMapMount *datasetConfigMapMount
}

// datasetConfigMapMount describes a read-only ConfigMap volume mount.
type datasetConfigMapMount struct {
	ConfigMapName string
	MountPath     string
}

func boolPtr(b bool) *bool { return &b }

// renderDatasetWorkerJob builds a hardened batch/v1 Job manifest. The pod runs
// as the workspace ServiceAccount with the azure.workload.identity/use label so
// the webhook injects the federated token; it is non-root, read-only-root-fs,
// drops all capabilities, and mounts an emptyDir scratch at /tmp (with TMPDIR /
// HOME pointed there) so a read-only root filesystem stays compatible with the
// SDK credential/token cache.
func renderDatasetWorkerJob(s datasetWorkerJobSpec) ([]byte, error) {
	activeDeadlineSeconds := s.ActiveDeadlineSeconds
	if activeDeadlineSeconds == 0 {
		activeDeadlineSeconds = datasetJobDeadlineSeconds
	}
	podLabels := map[string]string{
		"app":                         "tau-dataset",
		"azure.workload.identity/use": "true",
		workloadmeta.LabelDataset:     sanitizeLabelValue(s.DatasetName),
	}
	container := k8sContainer{
		Name:    datasetJobWorkerContainer,
		Image:   s.Image,
		Command: s.Command,
		Args:    s.FlagArgs,
		Env: []k8sEnvVar{
			{Name: "TMPDIR", Value: "/tmp"},
			{Name: "HOME", Value: "/tmp"},
		},
		SecurityContext: &k8sContainerSecurityContext{
			AllowPrivilegeEscalation: boolPtr(false),
			ReadOnlyRootFilesystem:   boolPtr(true),
			RunAsNonRoot:             boolPtr(true),
			Capabilities:             &k8sCapabilities{Drop: []string{"ALL"}},
		},
		VolumeMounts: []k8sVolumeMount{
			{Name: "scratch", MountPath: "/tmp"},
		},
	}
	volumes := []k8sVolume{
		{Name: "scratch", EmptyDir: &k8sEmptyDir{}},
	}
	if s.ConfigMapMount != nil {
		container.VolumeMounts = append(container.VolumeMounts, k8sVolumeMount{
			Name:      "record",
			MountPath: s.ConfigMapMount.MountPath,
			ReadOnly:  true,
		})
		volumes = append(volumes, k8sVolume{
			Name:      "record",
			ConfigMap: &k8sConfigMapVolumeSource{Name: s.ConfigMapMount.ConfigMapName},
		})
	}
	job := k8sJob{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: k8sObjectMeta{
			Name:      s.JobName,
			Namespace: s.Namespace,
			Labels:    s.Labels,
		},
		Spec: k8sJobSpec{
			BackoffLimit:            datasetJobBackoffLimit,
			TTLSecondsAfterFinished: datasetJobTTLSeconds,
			ActiveDeadlineSeconds:   activeDeadlineSeconds,
			Template: k8sPodTemplate{
				Metadata: k8sObjectMeta{Labels: podLabels},
				Spec: k8sPodSpec{
					ServiceAccountName: s.ServiceAcct,
					RestartPolicy:      "Never",
					SecurityContext: &k8sPodSecurityContext{
						RunAsNonRoot:   boolPtr(true),
						SeccompProfile: &k8sSeccompProfile{Type: "RuntimeDefault"},
					},
					Containers: []k8sContainer{container},
					Volumes:    volumes,
				},
			},
		},
	}
	return yaml.Marshal(job)
}

// k8sConfigMap is a minimal typed ConfigMap for record transport.
type k8sConfigMap struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Metadata   k8sObjectMeta     `yaml:"metadata"`
	Data       map[string]string `yaml:"data"`
}

// renderRecordConfigMap builds a ConfigMap carrying a single record file.
func renderRecordConfigMap(name, namespace, fileName, payload string, labels map[string]string) ([]byte, error) {
	cm := k8sConfigMap{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Metadata: k8sObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Data: map[string]string{fileName: payload},
	}
	return yaml.Marshal(cm)
}

// sanitizeLabelValue produces a Kubernetes-label-safe value from an arbitrary
// string (max 63 chars, [a-z0-9-], trimmed of leading/trailing separators).
func sanitizeLabelValue(s string) string {
	san := sanitizePodName(s)
	if len(san) > 63 {
		san = strings.Trim(san[:63], "-")
	}
	return san
}

// datasetJobName builds a deterministic, DNS-1123-safe name from a prefix,
// dataset name, and version.
func datasetJobName(prefix, name, version string) string {
	base := prefix + "-" + sanitizePodName(name)
	if version != "" {
		base += "-" + sanitizePodName(version)
	}
	base = strings.Trim(base, "-")
	if len(base) > 63 {
		base = strings.Trim(base[:63], "-")
	}
	if base == "" {
		base = "tau-ds"
	}
	return base
}

// datasetRunJobName gives each worker attempt a distinct Job name. Kubernetes
// Jobs do not restart after reaching a terminal state, so reusing a deterministic
// name would make a failed ingest impossible to resume.
var datasetJobSequence atomic.Uint64

func datasetRunJobName(prefix, name, version string) string {
	attempt := strconv.FormatInt(time.Now().UnixNano(), 36) + "-" +
		strconv.FormatUint(datasetJobSequence.Add(1), 36)
	return datasetJobName(prefix+"-"+attempt, name, version)
}

// applyManifest applies YAML via `kubectl apply -f -`. dryRun "" applies for
// real; "client"/"server" validate without mutation.
func applyManifest(ctx context.Context, r datasetKubeRunner, manifest []byte, dryRun string) (string, error) {
	args := []string{"apply", "-f", "-"}
	if dryRun != "" {
		args = append(args, "--dry-run="+dryRun)
	}
	return r.Raw(ctx, args, manifest)
}

// waitForJob polls a Job until it succeeds or reports a terminal Failed
// condition. The cumulative failed-pod count is not terminal while Kubernetes
// still has backoffLimit retries available.
func waitForJob(ctx context.Context, r datasetKubeRunner, namespace, jobName string, timeout time.Duration) (string, error) {
	return waitForJobPolling(ctx, r, namespace, jobName, timeout, datasetJobPollInterval)
}

func waitForJobPolling(
	ctx context.Context,
	r datasetKubeRunner,
	namespace, jobName string,
	timeout, pollInterval time.Duration,
) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		succeeded, _ := r.Raw(waitCtx, []string{
			"-n", namespace, "get", "job/" + jobName,
			"-o", "jsonpath={.status.succeeded}",
		}, nil)
		if strings.TrimSpace(succeeded) != "" && strings.TrimSpace(succeeded) != "0" {
			return "Succeeded", nil
		}
		failed, _ := r.Raw(waitCtx, []string{
			"-n", namespace, "get", "job/" + jobName,
			"-o", `jsonpath={.status.conditions[?(@.type=="Failed")].status}`,
		}, nil)
		if strings.EqualFold(strings.TrimSpace(failed), "true") {
			return "Failed", nil
		}
		select {
		case <-waitCtx.Done():
			return "", fmt.Errorf("timed out waiting for job %s/%s after %s", namespace, jobName, timeout)
		case <-ticker.C:
		}
	}
}

// jobLogs fetches the worker container logs for a Job.
func jobLogs(ctx context.Context, r datasetKubeRunner, namespace, jobName string) (string, error) {
	return r.Raw(ctx, []string{
		"-n", namespace, "logs", "job/" + jobName,
		"-c", datasetJobWorkerContainer, "--tail=-1",
	}, nil)
}

// deleteConfigMap best-effort removes a transport ConfigMap.
func deleteConfigMap(ctx context.Context, r datasetKubeRunner, namespace, cmName string) error {
	_, err := r.Raw(ctx, []string{
		"-n", namespace, "delete", "configmap/" + cmName,
		"--ignore-not-found", "--wait=false",
	}, nil)
	return err
}

// extractJSONObject returns the last balanced-looking JSON object span from
// worker stdout logs. kubectl logs may prefix warnings, so we scan for the
// final '{' ... '}' span.
func extractJSONObject(logs string) ([]byte, error) {
	start := strings.IndexByte(logs, '{')
	end := strings.LastIndexByte(logs, '}')
	if start < 0 || end < 0 || end < start {
		return nil, fmt.Errorf("no JSON object found in worker logs")
	}
	return []byte(logs[start : end+1]), nil
}

// parseWorkerOutput extracts the complete stable ingest result from worker logs.
func parseWorkerOutput(logs string) (datasetIngestResult, error) {
	raw, err := extractJSONObject(logs)
	if err != nil {
		return datasetIngestResult{}, err
	}
	var out datasetIngestResult
	if err := json.Unmarshal(raw, &out); err != nil {
		return datasetIngestResult{}, fmt.Errorf("parse worker output JSON: %w", err)
	}
	return out, nil
}

// writeManifests writes rendered manifests to w for dry-run inspection.
func writeManifests(w io.Writer, manifests ...[]byte) {
	for i, m := range manifests {
		if i > 0 {
			fmt.Fprintln(w, "---")
		}
		w.Write(m)
		if len(m) > 0 && m[len(m)-1] != '\n' {
			fmt.Fprintln(w)
		}
	}
}
