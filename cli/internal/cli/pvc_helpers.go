// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/kube"
)

const (
	defaultTauPVCName            = "blob-training"
	managedWorkflowMetricsRoot   = storage.DurableCheckpointsDir + "/finetunes"
	pvcHelperImage               = "busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662"
	pvcHelperPodTTL              = 60 * time.Second
	pvcHelperNotFoundMarker      = "TAU_PVC_GET_NOT_FOUND"
	pvcHelperBase64MissingMarker = "TAU_PVC_GET_BASE64_MISSING"
	pvcHelperListCompleteMarker  = "TAU_PVC_LIST_COMPLETE"
	pvcHelperListFailedMarker    = "TAU_PVC_LIST_FAILED"
	pvcHelperListSettleDelay     = 12 * time.Second
)

type managedWorkflowArtifactIndex struct {
	SchemaVersion int                       `json:"schema_version"`
	Run           string                    `json:"run"`
	Namespace     string                    `json:"namespace,omitempty"`
	ResourceName  string                    `json:"resource_name,omitempty"`
	CreatedAt     string                    `json:"created_at,omitempty"`
	HotRoot       string                    `json:"hot_root,omitempty"`
	DurableRoot   string                    `json:"durable_root,omitempty"`
	StorageProbe  map[string]any            `json:"storage_probe,omitempty"`
	Artifacts     []managedWorkflowArtifact `json:"artifacts"`
}

type managedWorkflowArtifact struct {
	Name              string `json:"name"`
	ManifestPath      string `json:"manifest_path"`
	SourcePath        string `json:"source_path,omitempty"`
	DurablePath       string `json:"durable_path"`
	Status            string `json:"status"`
	Kind              string `json:"kind,omitempty"`
	SizeBytes         int64  `json:"size_bytes,omitempty"`
	FileCount         int64  `json:"file_count,omitempty"`
	MTime             string `json:"mtime,omitempty"`
	UploadStartedAt   string `json:"upload_started_at,omitempty"`
	UploadCompletedAt string `json:"upload_completed_at,omitempty"`
	UploadDurationMS  int64  `json:"upload_duration_ms,omitempty"`
}

func managedWorkflowResultCandidatePaths(name, override string) []string {
	if strings.TrimSpace(override) != "" {
		return []string{strings.TrimSpace(override)}
	}
	candidates := []string{
		fmt.Sprintf("%s/%s/metrics.json", managedWorkflowMetricsRoot, name),
		fmt.Sprintf("%s/%s/metrics.json", storage.DurableCheckpointsDir, name),
		fmt.Sprintf("%s/%s/train-metrics.json", storage.DurableCheckpointsDir, name),
		fmt.Sprintf("%s/%s/eval-metrics.json", storage.DurableCheckpointsDir, name),
	}
	out := make([]string, 0, len(candidates))
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if candidate == "" || seen[candidate] {
			continue
		}
		seen[candidate] = true
		out = append(out, candidate)
	}
	return out
}

func fetchManagedWorkflowArtifacts(ctx context.Context, kubeContext, namespace, runName, pvcName string) ([]byte, string, error) {
	if pvcName == "" {
		pvcName = defaultTauPVCName
	}
	path := storage.DurableFinetuneArtifactsFile(runName)
	raw, err := fetchPVCFile(ctx, kubeContext, namespace, runName, pvcName, path)
	if err != nil {
		if strings.Contains(err.Error(), "result artifact not found at ") {
			return nil, path, fmt.Errorf(
				"artifact index not found at %s on PVC %s — has the run finished durable checkpoint finalization?",
				path,
				pvcName,
			)
		}
		return nil, path, err
	}
	return raw, path, nil
}

func fetchFirstPVCFile(ctx context.Context, kubeContext, namespace, runName, pvcName string, paths []string) ([]byte, string, error) {
	var notFound []string
	for _, candidate := range paths {
		raw, err := fetchPVCFile(ctx, kubeContext, namespace, runName, pvcName, candidate)
		if err == nil {
			return raw, candidate, nil
		}
		if strings.Contains(err.Error(), "result artifact not found at ") {
			notFound = append(notFound, candidate)
			continue
		}
		return nil, "", err
	}
	if len(notFound) == 0 {
		return nil, "", fmt.Errorf("no result artifact paths provided")
	}
	return nil, "", fmt.Errorf(
		"result artifact not found on PVC %s; tried: %s",
		pvcName,
		strings.Join(notFound, ", "),
	)
}

func pvcHelperPodName(prefix, runName string) (string, error) {
	var entropy [6]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", fmt.Errorf("generate PVC helper pod name: %w", err)
	}
	suffix := fmt.Sprintf("-%d-%s", time.Now().UnixNano(), hex.EncodeToString(entropy[:]))
	base := sanitizePodName(runName)
	maxBase := 63 - len(prefix) - 1 - len(suffix)
	if maxBase < 1 {
		return "", fmt.Errorf("PVC helper pod name suffix exceeds Kubernetes name limit")
	}
	if len(base) > maxBase {
		base = strings.Trim(base[:maxBase], "-")
	}
	if base == "" {
		base = "run"
	}
	return prefix + "-" + base + suffix, nil
}

func fetchPVCFile(ctx context.Context, kubeContext, namespace, runName, pvcName, path string) ([]byte, error) {
	if pvcName == "" {
		pvcName = defaultTauPVCName
	}
	podName, err := pvcHelperPodName("tau-get", runName)
	if err != nil {
		return nil, err
	}
	script := fmt.Sprintf(`if [ ! -f %s ]; then
  echo "%s" >&2
  exit 2
fi
if ! command -v base64 >/dev/null 2>&1; then
  echo "%s" >&2
  exit 3
fi
base64 %s
`, shellSingleQuote(path), pvcHelperNotFoundMarker, pvcHelperBase64MissingMarker, shellSingleQuote(path))
	r := kube.New(kubeContext)
	podYAML, err := helperPodYAMLForPVC(ctx, r, helperPodSpec{
		Name:      podName,
		Namespace: namespace,
		LabelApp:  "tau-pvc-get",
		Image:     pvcHelperImage,
		PVCName:   pvcName,
		TTLSec:    int(pvcHelperPodTTL.Seconds()),
		Script:    script,
	})
	if err != nil {
		return nil, fmt.Errorf("render helper pod: %w", err)
	}

	if _, err := r.Raw(ctx, []string{"apply", "-n", namespace, "-f", "-"}, podYAML); err != nil {
		return nil, fmt.Errorf("create helper pod: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = r.Raw(delCtx, []string{"delete", "pod", "-n", namespace, podName, "--wait=false", "--ignore-not-found"}, nil)
	}()

	phase, err := waitForHelperPodTerminal(ctx, r, namespace, podName, 90*time.Second)
	if err != nil {
		logs, lerr := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		if lerr == nil && strings.Contains(logs, pvcHelperNotFoundMarker) {
			return nil, fmt.Errorf("result artifact not found at %s on PVC %s — has the run finished?", path, pvcName)
		}
		if lerr == nil && strings.Contains(logs, pvcHelperBase64MissingMarker) {
			return nil, fmt.Errorf("helper image cannot fetch binary artifact %s from PVC %s: base64 command missing", path, pvcName)
		}
		return nil, fmt.Errorf("helper pod did not finish: %w (logs: %s)", err, strings.TrimSpace(logs))
	}
	if phase != "Succeeded" {
		logs, lerr := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		if lerr == nil && strings.Contains(logs, pvcHelperNotFoundMarker) {
			return nil, fmt.Errorf("result artifact not found at %s on PVC %s — has the run finished?", path, pvcName)
		}
		if lerr == nil && strings.Contains(logs, pvcHelperBase64MissingMarker) {
			return nil, fmt.Errorf("helper image cannot fetch binary artifact %s from PVC %s: base64 command missing", path, pvcName)
		}
		return nil, fmt.Errorf("helper pod did not succeed: phase=%s (logs: %s)", phase, strings.TrimSpace(logs))
	}

	out, err := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
	if err != nil {
		return nil, fmt.Errorf("read helper logs: %w", err)
	}
	return decodePVCFileLogs(path, pvcName, out)
}

func decodePVCFileLogs(path, pvcName, logs string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(logs)
	if err != nil {
		return nil, fmt.Errorf("decode binary artifact %s from PVC %s: helper output was not valid base64: %w", path, pvcName, err)
	}
	return raw, nil
}

func fetchPVCList(ctx context.Context, kubeContext, namespace, runName, pvcName, dirPath string) ([]string, error) {
	return fetchPVCListWithMode(ctx, kubeContext, namespace, runName, pvcName, dirPath, false)
}

func fetchPVCListRecursive(ctx context.Context, kubeContext, namespace, runName, pvcName, dirPath string) ([]string, error) {
	return fetchPVCListWithMode(ctx, kubeContext, namespace, runName, pvcName, dirPath, true)
}

func fetchPVCListWithMode(ctx context.Context, kubeContext, namespace, runName, pvcName, dirPath string, recursive bool) ([]string, error) {
	if pvcName == "" {
		pvcName = defaultTauPVCName
	}
	podName, err := pvcHelperPodName("tau-ls", runName)
	if err != nil {
		return nil, err
	}
	completionMarker := pvcHelperListCompleteMarker + ":" + podName
	r := kube.New(kubeContext)
	podYAML, err := helperPodYAMLForPVC(ctx, r, helperPodSpec{
		Name:      podName,
		Namespace: namespace,
		LabelApp:  "tau-pvc-list",
		Image:     pvcHelperImage,
		PVCName:   pvcName,
		TTLSec:    int(pvcHelperPodTTL.Seconds()),
		Script:    pvcListHelperScript(recursive),
		ScriptArgs: []string{
			dirPath,
			completionMarker,
			strconv.Itoa(int(pvcHelperListSettleDelay.Seconds())),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("render helper pod: %w", err)
	}

	if _, err := r.Raw(ctx, []string{"apply", "-n", namespace, "-f", "-"}, podYAML); err != nil {
		return nil, fmt.Errorf("create helper pod: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = r.Raw(delCtx, []string{"delete", "pod", "-n", namespace, podName, "--wait=false", "--ignore-not-found"}, nil)
	}()
	phase, err := waitForHelperPodTerminal(ctx, r, namespace, podName, 90*time.Second)
	if err != nil {
		logs, lerr := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		if lerr == nil && strings.Contains(logs, pvcHelperNotFoundMarker) {
			return nil, fmt.Errorf("result directory not found at %s on PVC %s — has the run finished?", dirPath, pvcName)
		}
		if lerr == nil && strings.Contains(logs, pvcHelperListFailedMarker) {
			return nil, fmt.Errorf("enumerate result directory %s on PVC %s: storage listing failed (logs: %s)", dirPath, pvcName, strings.TrimSpace(logs))
		}
		return nil, fmt.Errorf("helper pod did not finish: %w (logs: %s)", err, strings.TrimSpace(logs))
	}
	if phase != "Succeeded" {
		logs, lerr := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		if lerr == nil && strings.Contains(logs, pvcHelperNotFoundMarker) {
			return nil, fmt.Errorf("result directory not found at %s on PVC %s — has the run finished?", dirPath, pvcName)
		}
		if lerr == nil && strings.Contains(logs, pvcHelperListFailedMarker) {
			return nil, fmt.Errorf("enumerate result directory %s on PVC %s: storage listing failed (logs: %s)", dirPath, pvcName, strings.TrimSpace(logs))
		}
		return nil, fmt.Errorf("helper pod did not succeed: phase=%s (logs: %s)", phase, strings.TrimSpace(logs))
	}
	out, err := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
	if err != nil {
		return nil, fmt.Errorf("read helper logs: %w", err)
	}
	return decodePVCListLogs(dirPath, pvcName, completionMarker, out)
}

func pvcListHelperScript(recursive bool) string {
	maxDepth := "-maxdepth 1"
	if recursive {
		maxDepth = ""
	}
	return fmt.Sprintf(`dir=$1
completion_marker=$2
settle_seconds=$3
root=$dir
while [ "$root" != "/" ] && [ "${root%%/}" != "$root" ]; do
  root=${root%%/}
done
if [ ! -d "$root" ]; then
  echo "%s" >&2
  exit 2
fi
if ! command -v base64 >/dev/null 2>&1; then
  echo "%s" >&2
  exit 3
fi
probe=/tmp/tau-pvc-list-probe
if ! find "$root" -mindepth 1 -maxdepth 1 -print -quit >"$probe"; then
  echo "%s: initial directory probe" >&2
  exit 3
fi
if [ ! -s "$probe" ] && [ "$settle_seconds" -gt 0 ]; then
  sleep "$settle_seconds"
fi
if ! find "$root" -mindepth 1 %s -exec sh -c '
  root=$1
  shift
  for entry do
    relative=${entry#"$root"/}
    encoded=$(printf "%%s" "$relative" | base64) || exit 10
    printf "%%s" "$encoded" | tr -d "\n"
    printf "\n"
  done
' sh "$root" {} +; then
  echo "%s: recursive directory listing" >&2
  exit 3
fi
printf '%%s\n' "$completion_marker"
`, pvcHelperNotFoundMarker, pvcHelperBase64MissingMarker, pvcHelperListFailedMarker, maxDepth, pvcHelperListFailedMarker)
}

func decodePVCListLogs(dirPath, pvcName, completionMarker, logs string) ([]string, error) {
	lines := strings.Split(strings.TrimRight(logs, "\r\n"), "\n")
	if len(lines) == 0 || strings.TrimSuffix(lines[len(lines)-1], "\r") != completionMarker {
		return nil, fmt.Errorf(
			"verify directory listing for %s on PVC %s: helper output is missing its completion marker",
			dirPath,
			pvcName,
		)
	}

	var entries []string
	for _, line := range lines[:len(lines)-1] {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			return nil, fmt.Errorf(
				"decode directory listing for %s on PVC %s: helper output contains an empty entry record",
				dirPath,
				pvcName,
			)
		}
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return nil, fmt.Errorf(
				"decode directory listing entry for %s on PVC %s: helper output was not valid base64: %w",
				dirPath,
				pvcName,
				err,
			)
		}
		entries = append(entries, string(raw))
	}
	sort.Strings(entries)
	return entries, nil
}

func validatePVCArtifactVisible(ctx context.Context, kubeContext, namespace, runName, pvcName, artifactPath string, expectedFileCount int64) error {
	if pvcName == "" {
		pvcName = defaultTauPVCName
	}
	podName := fmt.Sprintf("tau-artifact-probe-%s-%d", sanitizePodName(runName), time.Now().Unix())
	if len(podName) > 60 {
		podName = podName[:60]
	}
	script := fmt.Sprintf(`path=%s
expected=%d
if [ -f "$path" ]; then
  echo "file 1"
  exit 0
fi
if [ -d "$path" ]; then
  count="$(find "$path" -type f | wc -l | tr -d ' ')"
  if [ "$count" = "" ]; then count=0; fi
  if [ "$count" -eq 0 ]; then
    echo "TAU_ARTIFACT_EMPTY_DIR" >&2
    exit 3
  fi
  if [ "$expected" -gt 0 ] && [ "$count" -lt "$expected" ]; then
    echo "TAU_ARTIFACT_INCOMPLETE_DIR count=$count expected=$expected" >&2
    exit 4
  fi
  echo "dir $count"
  exit 0
fi
echo "TAU_ARTIFACT_NOT_FOUND" >&2
exit 2
`, shellSingleQuote(artifactPath), expectedFileCount)
	r := kube.New(kubeContext)
	podYAML, err := helperPodYAMLForPVC(ctx, r, helperPodSpec{
		Name:      podName,
		Namespace: namespace,
		LabelApp:  "tau-artifact-probe",
		Image:     pvcHelperImage,
		PVCName:   pvcName,
		TTLSec:    int(pvcHelperPodTTL.Seconds()),
		Script:    script,
	})
	if err != nil {
		return fmt.Errorf("render helper pod: %w", err)
	}

	if _, err := r.Raw(ctx, []string{"apply", "-n", namespace, "-f", "-"}, podYAML); err != nil {
		return fmt.Errorf("create helper pod: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = r.Raw(delCtx, []string{"delete", "pod", "-n", namespace, podName, "--wait=false", "--ignore-not-found"}, nil)
	}()
	phase, err := waitForHelperPodTerminal(ctx, r, namespace, podName, 90*time.Second)
	if err != nil {
		logs, _ := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		return artifactVisibilityError(artifactPath, pvcName, strings.TrimSpace(logs), err)
	}
	if phase != "Succeeded" {
		logs, _ := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		return artifactVisibilityError(artifactPath, pvcName, strings.TrimSpace(logs), fmt.Errorf("phase=%s", phase))
	}
	return nil
}

func artifactVisibilityError(path, pvcName, logs string, err error) error {
	if err == nil {
		err = fmt.Errorf("helper pod failed")
	}
	switch {
	case strings.Contains(logs, "TAU_ARTIFACT_NOT_FOUND"):
		return fmt.Errorf("artifact payload not found at %s on PVC %s", path, pvcName)
	case strings.Contains(logs, "TAU_ARTIFACT_EMPTY_DIR"):
		return fmt.Errorf("artifact payload at %s on PVC %s is an empty directory; BlobFuse visibility may be stale or incomplete", path, pvcName)
	case strings.Contains(logs, "TAU_ARTIFACT_INCOMPLETE_DIR"):
		return fmt.Errorf("artifact payload at %s on PVC %s is incomplete (%s)", path, pvcName, logs)
	default:
		return fmt.Errorf("artifact visibility probe failed for %s on PVC %s: %w (logs: %s)", path, pvcName, err, logs)
	}
}

type helperPodSpec struct {
	Name       string
	Namespace  string
	LabelApp   string
	Image      string
	PVCName    string
	TTLSec     int
	Script     string
	ScriptArgs []string
	Placement  helperPodPlacement
}

type helperPodPlacement struct {
	NodeSelector map[string]string  `json:"nodeSelector,omitempty"`
	Affinity     *helperPodAffinity `json:"affinity,omitempty"`
	Tolerations  []map[string]any   `json:"tolerations,omitempty"`
}

type helperPodAffinity struct {
	NodeAffinity map[string]any `json:"nodeAffinity,omitempty" yaml:"nodeAffinity,omitempty"`
}

type helperPodObjMeta struct {
	Name      string            `yaml:"name"`
	Namespace string            `yaml:"namespace"`
	Labels    map[string]string `yaml:"labels,omitempty"`
}

type helperPodVolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}

type helperPodContainer struct {
	Name         string                 `yaml:"name"`
	Image        string                 `yaml:"image"`
	Command      []string               `yaml:"command"`
	Args         []string               `yaml:"args"`
	VolumeMounts []helperPodVolumeMount `yaml:"volumeMounts,omitempty"`
}

type helperPodPVCSource struct {
	ClaimName string `yaml:"claimName"`
}

type helperPodVolume struct {
	Name                  string             `yaml:"name"`
	PersistentVolumeClaim helperPodPVCSource `yaml:"persistentVolumeClaim"`
}

type helperPodPodSpec struct {
	RestartPolicy         string               `yaml:"restartPolicy"`
	ActiveDeadlineSeconds int                  `yaml:"activeDeadlineSeconds"`
	NodeSelector          map[string]string    `yaml:"nodeSelector,omitempty"`
	Affinity              *helperPodAffinity   `yaml:"affinity,omitempty"`
	Tolerations           []map[string]any     `yaml:"tolerations,omitempty"`
	Containers            []helperPodContainer `yaml:"containers"`
	Volumes               []helperPodVolume    `yaml:"volumes"`
}

type helperPodObject struct {
	APIVersion string           `yaml:"apiVersion"`
	Kind       string           `yaml:"kind"`
	Metadata   helperPodObjMeta `yaml:"metadata"`
	Spec       helperPodPodSpec `yaml:"spec"`
}

func helperPodYAML(s helperPodSpec) ([]byte, error) {
	args := []string{s.Script}
	if len(s.ScriptArgs) > 0 {
		args = append(args, "helper")
		args = append(args, s.ScriptArgs...)
	}
	pod := helperPodObject{
		APIVersion: "v1",
		Kind:       "Pod",
		Metadata: helperPodObjMeta{
			Name:      s.Name,
			Namespace: s.Namespace,
			Labels:    map[string]string{"app": s.LabelApp},
		},
		Spec: helperPodPodSpec{
			RestartPolicy:         "Never",
			ActiveDeadlineSeconds: s.TTLSec,
			NodeSelector:          s.Placement.NodeSelector,
			Affinity:              s.Placement.Affinity,
			Tolerations:           s.Placement.Tolerations,
			Containers: []helperPodContainer{{
				Name:    "helper",
				Image:   s.Image,
				Command: []string{"sh", "-c"},
				Args:    args,
				VolumeMounts: []helperPodVolumeMount{{
					Name:      "data",
					MountPath: "/data",
				}},
			}},
			Volumes: []helperPodVolume{{
				Name:                  "data",
				PersistentVolumeClaim: helperPodPVCSource{ClaimName: s.PVCName},
			}},
		},
	}
	return yaml.Marshal(pod)
}

func helperPodYAMLForPVC(ctx context.Context, r kubeRawRunner, s helperPodSpec) ([]byte, error) {
	placement, err := resolveHelperPodPlacement(ctx, r, s.Namespace, s.PVCName)
	if err != nil {
		return nil, err
	}
	s.Placement = placement
	return helperPodYAML(s)
}

func resolveHelperPodPlacement(
	ctx context.Context,
	r kubeRawRunner,
	namespace, pvcName string,
) (helperPodPlacement, error) {
	pvcNamespace, err := requireWorkloadNamespace(namespace)
	if err != nil {
		return helperPodPlacement{}, err
	}
	rawPVC, err := r.Raw(ctx, []string{"-n", pvcNamespace, "get", "pvc", pvcName, "-o", "json"}, nil)
	if err != nil {
		return helperPodPlacement{}, fmt.Errorf("read PVC %s/%s for helper placement: %w", pvcNamespace, pvcName, err)
	}
	var pvc pvcDoc
	if err := json.Unmarshal([]byte(rawPVC), &pvc); err != nil {
		return helperPodPlacement{}, fmt.Errorf("parse PVC %s/%s for helper placement: %w", pvcNamespace, pvcName, err)
	}
	if pvc.Spec.VolumeName == "" {
		return helperPodPlacement{}, nil
	}
	rawPV, err := r.Raw(ctx, []string{"get", "pv", pvc.Spec.VolumeName, "-o", "json"}, nil)
	if err != nil {
		if helperPlacementDiscoveryDenied(err) {
			return helperPodPlacement{}, nil
		}
		return helperPodPlacement{}, fmt.Errorf("read bound PV %q for PVC %s/%s helper placement: %w", pvc.Spec.VolumeName, pvcNamespace, pvcName, err)
	}
	var pv pvDoc
	if err := json.Unmarshal([]byte(rawPV), &pv); err != nil {
		return helperPodPlacement{}, fmt.Errorf("parse bound PV %q for PVC %s/%s helper placement: %w", pvc.Spec.VolumeName, pvcNamespace, pvcName, err)
	}
	placement, err := helperPlacementFromPV(pvcName, pv)
	if err != nil {
		return helperPodPlacement{}, err
	}
	if pv.Spec.CSI == nil {
		return placement, nil
	}
	driver := strings.TrimSpace(pv.Spec.CSI.Driver)
	if driver == "" {
		return helperPodPlacement{}, fmt.Errorf("PVC helper %q is backed by a CSI volume with no driver name; cannot determine safe helper placement", pvcName)
	}
	driversByNode, err := listCSINodeDrivers(ctx, r)
	if err != nil {
		if helperPlacementDiscoveryDenied(err) {
			return placement, nil
		}
		return helperPodPlacement{}, err
	}
	compatible := make([]string, 0, len(driversByNode))
	for nodeName, drivers := range driversByNode {
		if drivers[driver] {
			compatible = append(compatible, nodeName)
		}
	}
	sort.Strings(compatible)
	if len(compatible) == 0 {
		return helperPodPlacement{}, fmt.Errorf(
			"PVC helper %q is backed by CSI driver %q, but that driver is not registered on any node",
			pvcName,
			driver,
		)
	}
	if len(compatible) == len(driversByNode) {
		return placement, nil
	}
	addRequiredNodeNames(&placement, compatible)
	return placement, nil
}

func helperPlacementFromPV(pvcName string, pv pvDoc) (helperPodPlacement, error) {
	placement := helperPodPlacement{}
	var terms []map[string]any
	if pv.Spec.NodeAffinity != nil && pv.Spec.NodeAffinity.Required != nil {
		terms = pv.Spec.NodeAffinity.Required.NodeSelectorTerms
	}
	if len(terms) > 0 {
		rawTerms := make([]any, 0, len(terms))
		for _, term := range terms {
			rawTerms = append(rawTerms, term)
		}
		placement.Affinity = &helperPodAffinity{
			NodeAffinity: map[string]any{
				"requiredDuringSchedulingIgnoredDuringExecution": map[string]any{
					"nodeSelectorTerms": rawTerms,
				},
			},
		}
	}
	if (len(pv.Spec.Local) > 0 || len(pv.Spec.HostPath) > 0) && len(terms) == 0 {
		return helperPodPlacement{}, fmt.Errorf(
			"PVC helper %q uses node-local storage without PV node affinity; cannot determine a safe node for the helper pod",
			pvcName,
		)
	}
	return placement, nil
}

func helperPlacementDiscoveryDenied(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "forbidden") ||
		strings.Contains(message, "unauthorized") ||
		strings.Contains(message, "cannot get resource") ||
		strings.Contains(message, "cannot list resource")
}

func addRequiredNodeNames(placement *helperPodPlacement, nodeNames []string) {
	if placement.Affinity == nil {
		placement.Affinity = &helperPodAffinity{}
	}
	nodeAffinity := placement.Affinity.NodeAffinity
	if nodeAffinity == nil {
		nodeAffinity = map[string]any{}
		placement.Affinity.NodeAffinity = nodeAffinity
	}
	required, _ := nodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	if required == nil {
		required = map[string]any{}
		nodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"] = required
	}
	terms, _ := required["nodeSelectorTerms"].([]any)
	if len(terms) == 0 {
		terms = []any{map[string]any{}}
	}
	requirement := map[string]any{
		"key":      "metadata.name",
		"operator": "In",
		"values":   nodeNames,
	}
	for i, rawTerm := range terms {
		term, _ := rawTerm.(map[string]any)
		if term == nil {
			term = map[string]any{}
			terms[i] = term
		}
		matchFields, _ := term["matchFields"].([]any)
		term["matchFields"] = append(matchFields, requirement)
	}
	required["nodeSelectorTerms"] = terms
}

func waitForHelperPodTerminal(ctx context.Context, r kubeRawRunner, namespace, podName string, timeout time.Duration) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	for {
		out, err := r.Raw(waitCtx, []string{
			"get", "-n", namespace, "pod/" + podName,
			"-o", "jsonpath={.status.phase}",
		}, nil)
		if err == nil {
			phase := strings.TrimSpace(out)
			if phase == "Succeeded" {
				return phase, nil
			}
			if phase == "Failed" {
				if diagnostic := helperPodEventDiagnostic(waitCtx, r, namespace, podName); diagnostic != "" {
					return phase, fmt.Errorf("helper pod failed: %s", diagnostic)
				}
				return phase, nil
			}
		}
		select {
		case <-waitCtx.Done():
			if diagnostic := helperPodEventDiagnostic(ctx, r, namespace, podName); diagnostic != "" {
				return "", fmt.Errorf("%w; pod events: %s", waitCtx.Err(), diagnostic)
			}
			return "", waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func helperPodEventDiagnostic(ctx context.Context, r kubeRawRunner, namespace, podName string) string {
	out, err := r.Raw(ctx, []string{
		"get", "events", "-n", namespace,
		"--field-selector", "involvedObject.kind=Pod,involvedObject.name=" + podName,
		"-o", "json",
	}, nil)
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	var events struct {
		Items []struct {
			Type    string `json:"type"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &events); err != nil {
		return ""
	}
	var diagnostics []string
	for _, event := range events.Items {
		if !strings.EqualFold(event.Type, "Warning") {
			continue
		}
		detail := strings.TrimSpace(event.Reason + ": " + event.Message)
		if detail == ":" {
			continue
		}
		diagnostics = append(diagnostics, detail)
	}
	return strings.Join(diagnostics, "; ")
}

func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func sanitizePodName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "x"
	}
	return out
}
