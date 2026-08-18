// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/manifest"
	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/core/exptelemetry"
	"github.com/Azure/taugrid/core/kube"
	runtopology "github.com/Azure/taugrid/core/topology"
)

func applyDataPVCOverride(raw []byte, m *manifest.Manifest, dataPVC string) ([]byte, error) {
	if dataPVC == "" {
		return raw, nil
	}
	pvc := strings.TrimSpace(dataPVC)
	if pvc == "" || pvc != dataPVC {
		return nil, fmt.Errorf("--data-pvc must not be empty or have surrounding whitespace")
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("--data-pvc: manifest yaml: %w", err)
	}
	storageBlock, ok := doc["storage"]
	storageMap := map[string]any{}
	if ok && storageBlock != nil {
		var mapOK bool
		storageMap, mapOK = storageBlock.(map[string]any)
		if !mapOK {
			return nil, fmt.Errorf("--data-pvc: manifest storage must be a mapping")
		}
	}
	storageMap["data_pvc"] = pvc
	doc["storage"] = storageMap
	updated, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("--data-pvc: manifest yaml: %w", err)
	}
	m.Storage.DataPVC = pvc
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("--data-pvc: %w", err)
	}
	return updated, nil
}

func applyRuntimeImageOverride(raw []byte, m *manifest.Manifest, image string) ([]byte, error) {
	if image == "" {
		return raw, nil
	}
	img := strings.TrimSpace(image)
	if img == "" || img != image {
		return nil, fmt.Errorf("--image must not be empty or have surrounding whitespace")
	}

	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("--image: manifest yaml: %w", err)
	}
	runtimeBlock, ok := doc["runtime"]
	runtimeMap := map[string]any{}
	if ok && runtimeBlock != nil {
		var mapOK bool
		runtimeMap, mapOK = runtimeBlock.(map[string]any)
		if !mapOK {
			return nil, fmt.Errorf("--image: manifest runtime must be a mapping")
		}
	}
	runtimeMap["image"] = img
	doc["runtime"] = runtimeMap
	updated, err := yaml.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("--image: manifest yaml: %w", err)
	}
	m.Runtime.Image = img
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("--image: %w", err)
	}
	return updated, nil
}

func workloadKindK8sResource(kind string) string {
	switch kind {
	case manifest.WorkloadKindRayJob, manifest.WorkloadKindRayJobEval:
		return "rayjobs.ray.io"
	default:
		return "jobs.batch"
	}
}

func applyRunExperimentMetricsOffload(ctx context.Context, opts manifest.MetricsOffloadOptions) manifest.MetricsOffloadOptions {
	meta := runExperimentMetadataFromContext(ctx)
	if meta.Project != "" {
		opts.Project = meta.Project
	}
	if meta.ExperimentID != "" {
		opts.Experiment = meta.ExperimentID
	}
	if meta.RunGroupID != "" {
		opts.Group = meta.RunGroupID
	}
	protected := map[string]string{}
	if meta.Workspace != "" {
		protected[exptelemetry.TauWorkspaceTag] = meta.Workspace
	}
	opts.Tags = metricsoffload.MergeTags(opts.Tags, meta.Tags, protected)
	return opts
}

func loadJobSecretPayload(path string) (*manifest.JobSecret, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--secret-payload: %w", err)
	}
	var secret manifest.JobSecret
	if err := json.Unmarshal(raw, &secret); err != nil {
		return nil, fmt.Errorf("--secret-payload: %w", err)
	}
	if secret.Name == "" || len(secret.StringData) == 0 {
		return nil, fmt.Errorf("--secret-payload: requires name and stringData")
	}
	return &secret, nil
}

func defaultGPUResourceMode() string {
	if env := os.Getenv("TAU_GPU_RESOURCE_MODE"); strings.TrimSpace(env) != "" {
		return env
	}
	return manifest.GPUResourceModeDevicePlugin
}

func topologyOptionsFromSubmit(o jobrender.Options) runtopology.Options {
	return runtopology.Options{
		Team:                            o.Team,
		Lane:                            o.Lane,
		Mode:                            o.Mode,
		Placement:                       o.Topology,
		Shape:                           o.Shape,
		GPUClass:                        o.GPUClass,
		CheckpointEvery:                 o.CheckpointEvery,
		QueueName:                       o.QueueName,
		PriorityTier:                    o.PriorityTier,
		RequiredTopology:                o.RequiredTopology,
		WorkloadPriorityClassName:       o.WorkloadPriorityClassName,
		PodPriorityClassName:            o.PodPriorityClassName,
		DisableKueueTopologyAnnotations: o.DisableKueueTopologyAnnotations,
		DisableDefaultPriorities:        o.DisableDefaultPriorities,
	}
}

func validateManagedWorkflowTopologyIntent(m *manifest.Manifest, o jobrender.Options, preset *runtopology.ResolvedPreset, workloadKind string) error {
	// Workload-kind ↔ lane consistency. Lane is the Kueue-queue selector,
	// so the eval lane (lane=eval) and the training lanes (training,
	// elastic, large-memory) route to physically
	// different ClusterQueues with different priority classes and pod
	// shapes. Letting them cross-pollinate would silently misdispatch
	// the workload onto somebody else's quota slice.
	switch o.Lane {
	case "", "training", "elastic", "large-memory":
		// Train lanes (or no explicit lane). REJECT on rayjob-eval —
		// the eval shape (system head + GPU actor + CPU workers) belongs on lane=workload.
		// "" passes here because a missing lane is normal for the train
		// path (suggestManagedWorkflowPreset stamps lane=training by default).
		// On rayjob-eval, however, "" means the caller forgot to pick an
		// eval-lane preset, which would land the workload on the train
		// queue with the wrong priority class.
		if workloadKind == manifest.WorkloadKindRayJobEval && o.Lane != "" {
			return fmt.Errorf("--workload-kind=rayjob-eval requires lane=eval, got lane=%q; pick an eval-lane preset (e.g. azure.research.eval.gpu) or pass --lane=eval", o.Lane)
		}
	case "eval":
		// Eval lane is only valid for the rayjob-eval shape (system head +
		// GPU actor worker + CPU workers). A normal finetune (Job or RayJob multi-node) on
		// the eval lane would be misdispatched.
		if workloadKind != manifest.WorkloadKindRayJobEval {
			return fmt.Errorf("preset uses lane=eval, but workload kind is %q; eval lane is only valid for --workload-kind=rayjob-eval", workloadKind)
		}
	default:
		return fmt.Errorf("finetune training cannot use lane %q; use a training, elastic, large-memory, or eval preset", o.Lane)
	}

	// Cross-check: when a multi-node preset is in play, the manifest's
	// workers must match the preset's workers (analogous to the gpus/shape
	// cross-check below). A 2-node preset can't host a 4-pod manifest.
	if preset != nil && preset.Preset.Workers > 0 && m.Compute.Workers != preset.Preset.Workers {
		return fmt.Errorf("preset %s expects compute.workers=%d, but manifest has compute.workers=%d; pick a different preset or update the manifest", preset.Preset.Name, preset.Preset.Workers, m.Compute.Workers)
	}

	shape := o.Shape
	if shape == "" {
		return nil
	}
	want, ok, err := gpuCountFromShape(shape)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if m.Compute.GPUs != want {
		source := "--shape"
		if preset != nil {
			source = fmt.Sprintf("preset %s shape", preset.Preset.Name)
		}
		return fmt.Errorf("%s %q requests %d GPU(s), but manifest compute.gpus=%d; use a matching preset or update the manifest", source, shape, want, m.Compute.GPUs)
	}
	return nil
}

func gpuCountFromShape(shape string) (int, bool, error) {
	return runtopology.GPUCountFromShape(shape)
}

// suggestManagedWorkflowPreset is the one-line gap-closer that turns
// `tau run --config tau.yaml` into a managed-lane submit without the
// researcher having to remember preset names. Lane is "training" by default
// but switches to "eval" when the manifest declares an eval shape (so the
// eval RayJob lands on the right Kueue queue and priority class). Team comes
// from --team, then $TAU_TEAM. There is no default team — every team owns
// its own Kueue quota slice and we refuse to silently land workloads on
// somebody else's reservation. Returns the resolved preset and a
// human-readable description of where the team came from for the stderr
// message.
func suggestManagedWorkflowPreset(topo topologyFlags, gpus, workers int, lane string) (runtopology.ResolvedPreset, string, error) {
	team := topo.team
	source := "--team"
	if team == "" {
		if env := os.Getenv("TAU_TEAM"); env != "" {
			team = env
			source = "TAU_TEAM env"
		}
	}
	if team == "" {
		return runtopology.ResolvedPreset{}, "", fmt.Errorf("no team supplied: pass --team, set TAU_TEAM, or pick a preset with --preset")
	}
	if lane == "" {
		lane = "training"
	}
	resolved, err := runtopology.SuggestPreset(topo.policyPath, team, lane, gpus, workers)
	if err != nil {
		return runtopology.ResolvedPreset{}, "", err
	}
	return resolved, source, nil
}

func extraScriptPaths(extras []manifest.ExtraScript) string {
	paths := make([]string, 0, len(extras))
	for _, extra := range extras {
		paths = append(paths, "/script/"+extra.Name)
	}
	return strings.Join(paths, ", ")
}

func loadExtraScripts(specs []string) ([]manifest.ExtraScript, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]manifest.ExtraScript, 0, len(specs))
	seen := map[string]bool{}
	for _, spec := range specs {
		src, dest := splitExtraScriptSpec(spec)
		if src == "" {
			return nil, fmt.Errorf("--extra-script: source path is required")
		}
		if dest == "" {
			dest = filepath.Base(src)
		}
		if dest == "." || dest == string(filepath.Separator) || strings.Contains(dest, "/") || strings.Contains(dest, `\`) {
			return nil, fmt.Errorf("--extra-script %q: DEST must be a single filename mounted under /script", spec)
		}
		if seen[dest] {
			return nil, fmt.Errorf("--extra-script %q: duplicate destination %q", spec, dest)
		}
		seen[dest] = true
		info, err := os.Stat(src)
		if err != nil {
			return nil, fmt.Errorf("--extra-script %s: %w", src, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("--extra-script %s: directories are not supported; pass files explicitly", src)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return nil, fmt.Errorf("--extra-script %s: %w", src, err)
		}
		out = append(out, manifest.ExtraScript{Name: dest, Data: data})
	}
	return out, nil
}

func parseNodeSelectors(specs []string) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := map[string]string{}
	for _, spec := range specs {
		key, value, ok := strings.Cut(spec, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || key == "" || value == "" {
			return nil, fmt.Errorf("--node-selector %q: expected key=value with non-empty key and value", spec)
		}
		out[key] = value
	}
	return out, nil
}

func splitExtraScriptSpec(spec string) (src, dest string) {
	// SRC[:DEST] split has to handle three shapes:
	//   posix-no-dest:        /tmp/foo.py
	//   posix-with-dest:      /tmp/foo.py:bar.py
	//   windows-no-dest:      C:\Users\...\foo.py        (drive-letter colon!)
	//   windows-with-dest:    C:\Users\...\foo.py:foo.py
	//
	// A naive strings.Cut on first ':' eats the drive-letter colon on Windows;
	// a naive strings.LastIndex eats it when DEST is absent. Strip the volume
	// (filepath.VolumeName returns "C:" on Windows, "" on POSIX), then split
	// the remainder on the last colon. The drive-letter colon never reaches
	// the separator search.
	vol := filepath.VolumeName(spec)
	rest := spec[len(vol):]
	idx := strings.LastIndex(rest, ":")
	if idx < 0 {
		return spec, ""
	}
	return vol + rest[:idx], rest[idx+1:]
}

func workloadKindToK8sKind(wk string) string {
	switch wk {
	case manifest.WorkloadKindRayJob, manifest.WorkloadKindRayJobEval:
		return "RayJob"
	default:
		return "Job"
	}
}

// ownerRefTarget identifies the workload that should own a generated auxiliary
// object (the run Secret, the per-run SecretProviderClass) so that Kubernetes
// garbage-collects the two together instead of leaving the auxiliary object
// behind forever.
type ownerRefTarget struct {
	// APIVersion and Kind go into the ownerReference itself.
	APIVersion string
	Kind       string
	// Name is the workload name, used both for the UID lookup and the ref.
	Name string
	// Resource is the kubectl resource used to read the owner's UID.
	Resource string
}

func ownerRefTargetFor(workloadKind, ownerName string) ownerRefTarget {
	if workloadKindToK8sKind(workloadKind) == "RayJob" {
		return ownerRefTarget{APIVersion: "ray.io/v1", Kind: "RayJob", Name: ownerName, Resource: "rayjobs.ray.io"}
	}
	return ownerRefTarget{APIVersion: "batch/v1", Kind: "Job", Name: ownerName, Resource: "jobs.batch"}
}

// patchOwnerRef adopts an already-applied auxiliary object into its workload's
// ownerReferences.
//
// Ownership cannot be rendered inline: an ownerReference requires the owner's
// UID, which only exists once the API server has created the workload, and tau
// submits a multi-document manifest with `kubectl apply`. So the auxiliary
// object is applied first and adopted immediately afterwards. The window
// between the two is why every caller also registers the object for cleanup on
// submission failure — an interrupted run must not leave an unowned object
// that nothing will ever reclaim.
func patchOwnerRef(ctx context.Context, r *kube.Runner, namespace, resource, name string, owner ownerRefTarget) error {
	uidOut, err := r.Raw(ctx, []string{
		"get", owner.Resource, owner.Name, "-n", namespace,
		"-o", "jsonpath={.metadata.uid}",
	}, nil)
	if err != nil {
		return fmt.Errorf("get %s uid: %w", owner.Kind, err)
	}
	uid := strings.TrimSpace(uidOut)
	if uid == "" {
		return fmt.Errorf("empty uid for %s/%s", owner.Kind, owner.Name)
	}

	patch := fmt.Sprintf(`{"metadata":{"ownerReferences":[{"apiVersion":%q,"kind":%q,"name":%q,"uid":%q,"controller":true,"blockOwnerDeletion":true}]}}`,
		owner.APIVersion, owner.Kind, owner.Name, uid)

	if _, err := r.Raw(ctx, []string{
		"patch", resource, name, "-n", namespace,
		"--type=merge", "-p", patch,
	}, nil); err != nil {
		return fmt.Errorf("patch %s ownerRef: %w", resource, err)
	}
	return nil
}

func patchSecretOwnerRef(ctx context.Context, r *kube.Runner, namespace string, secret *manifest.JobSecret, workloadKind string) error {
	return patchOwnerRef(ctx, r, namespace, "secret", secret.Name, ownerRefTargetFor(workloadKind, secret.OwnerName))
}

// patchSecretProviderClassOwnerRef adopts the per-run SecretProviderClass that
// manifest.Render emits for Key Vault workloads. It is named after the run
// (kvspec.SPCName) and is therefore as disposable as the workload itself;
// without an ownerReference it would accumulate one orphan per Key Vault run.
func patchSecretProviderClassOwnerRef(ctx context.Context, r *kube.Runner, namespace, spcName, ownerName, workloadKind string) error {
	return patchOwnerRef(ctx, r, namespace, secretProviderClassResource, spcName, ownerRefTargetFor(workloadKind, ownerName))
}
