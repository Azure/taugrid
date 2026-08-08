// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	DefaultAdoptQueue      = "jobqueue"
	DefaultAdoptDataPVC    = "blob-training"
	namespaceResource      = "namespace"
	localQueueResource     = "localqueue.kueue.x-k8s.io"
	clusterQueueResource   = "clusterqueue.kueue.x-k8s.io"
	pvcResource            = "persistentvolumeclaim"
	storageClassResource   = "storageclass.storage.k8s.io"
	tauWorkspaceResource   = "workspace.tau.azure.com"
	defaultLocalQueueLabel = "kueue.x-k8s.io/default-local-queue"
)

// AdoptRunner is the narrow kubectl surface used by workspace adoption.
type AdoptRunner interface {
	Raw(ctx context.Context, args []string, stdin []byte) (string, error)
}

// AdoptOptions describes an existing namespace and its platform-owned
// dependencies. Adoption never creates these resources.
type AdoptOptions struct {
	Name              string
	Namespace         string
	Queue             string
	PlatformNamespace string
	DataPVC           string
	NamespaceUID      string
	QueueUID          string
	PVCUID            string
	StorageClass      string
	ClusterQueue      string
	OutputRoot        string
	Priority          string
}

func (o AdoptOptions) withDefaults() AdoptOptions {
	if o.Namespace == "" {
		o.Namespace = o.Name
	}
	if o.Queue == "" {
		o.Queue = DefaultAdoptQueue
	}
	if o.PlatformNamespace == "" {
		o.PlatformNamespace = PlatformNamespace
	}
	if o.OutputRoot == "" && o.Name != "" {
		o.OutputRoot = "/data/projects/" + o.Name + "/runs"
	}
	return o
}

func (o AdoptOptions) validate() error {
	names := []struct {
		flag  string
		value string
	}{
		{"NAME", o.Name},
		{"--namespace", o.Namespace},
		{"--queue", o.Queue},
		{"--platform-namespace", o.PlatformNamespace},
		{"--cluster-queue", o.ClusterQueue},
		{"--storage-class", o.StorageClass},
	}
	for _, name := range names {
		if name.value == "" && (name.flag == "--cluster-queue" || name.flag == "--storage-class") {
			continue
		}
		if name.value == "" {
			return fmt.Errorf("%s must not be empty", name.flag)
		}
		if name.value != strings.TrimSpace(name.value) {
			return fmt.Errorf("%s must not contain leading or trailing whitespace", name.flag)
		}
		if errs := validation.IsDNS1123Subdomain(name.value); len(errs) > 0 {
			return fmt.Errorf("%s %q is invalid: %s", name.flag, name.value, strings.Join(errs, "; "))
		}
	}
	for _, label := range []struct {
		flag  string
		value string
	}{
		{"NAME", o.Name},
		{"--queue", o.Queue},
	} {
		if errs := validation.IsValidLabelValue(label.value); len(errs) > 0 {
			return fmt.Errorf("%s %q cannot be used as a controller label value: %s", label.flag, label.value, strings.Join(errs, "; "))
		}
	}
	for _, namespace := range []struct {
		flag  string
		value string
	}{
		{"--namespace", o.Namespace},
		{"--platform-namespace", o.PlatformNamespace},
	} {
		if errs := validation.IsDNS1123Label(namespace.value); len(errs) > 0 {
			return fmt.Errorf("%s %q is not a valid Namespace name: %s", namespace.flag, namespace.value, strings.Join(errs, "; "))
		}
	}
	if o.DataPVC != "" {
		if o.DataPVC != strings.TrimSpace(o.DataPVC) {
			return fmt.Errorf("--data-pvc must not contain leading or trailing whitespace")
		}
		if errs := validation.IsDNS1123Subdomain(o.DataPVC); len(errs) > 0 {
			return fmt.Errorf("--data-pvc %q is invalid: %s", o.DataPVC, strings.Join(errs, "; "))
		}
	}
	for _, guard := range []struct {
		flag  string
		value string
	}{
		{"--namespace-uid", o.NamespaceUID},
		{"--queue-uid", o.QueueUID},
		{"--pvc-uid", o.PVCUID},
	} {
		if guard.value != strings.TrimSpace(guard.value) {
			return fmt.Errorf("%s must not contain leading or trailing whitespace", guard.flag)
		}
	}
	if o.DataPVC == "" && (o.PVCUID != "" || o.StorageClass != "") {
		return fmt.Errorf("--pvc-uid and --storage-class require a non-empty --data-pvc")
	}
	if o.OutputRoot == "" || o.OutputRoot != strings.TrimSpace(o.OutputRoot) {
		return fmt.Errorf("--output-root must be non-empty and must not contain leading or trailing whitespace")
	}
	if !path.IsAbs(o.OutputRoot) || path.Clean(o.OutputRoot) != o.OutputRoot {
		return fmt.Errorf("--output-root %q must be an absolute, clean path", o.OutputRoot)
	}
	if o.Priority != "" && o.Priority != "default" && o.Priority != "priority" && o.Priority != "normal" {
		return fmt.Errorf("--priority must be one of: default, priority, normal")
	}
	return nil
}

type adoptManifest struct {
	APIVersion string          `yaml:"apiVersion"`
	Kind       string          `yaml:"kind"`
	Metadata   adoptObjectMeta `yaml:"metadata"`
	Spec       adoptSpec       `yaml:"spec"`
}

type adoptObjectMeta struct {
	Name            string `yaml:"name"`
	Namespace       string `yaml:"namespace"`
	UID             string `yaml:"uid,omitempty"`
	ResourceVersion string `yaml:"resourceVersion,omitempty"`
}

type adoptSpec struct {
	Authorization WorkspaceAuthorization `yaml:"authorization"`
	Target        adoptTarget            `yaml:"target"`
	Queue         string                 `yaml:"queue"`
	Defaults      WorkspaceDefaults      `yaml:"defaults"`
}

type adoptTarget struct {
	Namespace       string `yaml:"namespace"`
	CreateNamespace bool   `yaml:"createNamespace"`
}

func desiredAdoption(o AdoptOptions) Workspace {
	return Workspace{
		APIVersion: APIVersion,
		Kind:       KindWorkspace,
		Metadata: ObjectMeta{
			Name:      o.Name,
			Namespace: o.PlatformNamespace,
		},
		Spec: WorkspaceSpec{
			Authorization: &WorkspaceAuthorization{Mode: AuthorizationModeClusterWide},
			Target: WorkspaceTarget{
				Namespace:       o.Namespace,
				CreateNamespace: false,
			},
			Queue:    o.Queue,
			Defaults: WorkspaceDefaults{OutputRoot: o.OutputRoot, Priority: o.Priority},
		},
	}
}

func renderAdoption(o AdoptOptions, metadata ObjectMeta) ([]byte, error) {
	manifest := adoptManifest{
		APIVersion: APIVersion,
		Kind:       KindWorkspace,
		Metadata: adoptObjectMeta{
			Name:            o.Name,
			Namespace:       o.PlatformNamespace,
			UID:             metadata.UID,
			ResourceVersion: metadata.ResourceVersion,
		},
		Spec: adoptSpec{
			Authorization: WorkspaceAuthorization{Mode: AuthorizationModeClusterWide},
			Target: adoptTarget{
				Namespace:       o.Namespace,
				CreateNamespace: false,
			},
			Queue:    o.Queue,
			Defaults: WorkspaceDefaults{OutputRoot: o.OutputRoot, Priority: o.Priority},
		},
	}
	raw, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("render TauWorkspace adoption manifest: %w", err)
	}
	return raw, nil
}

// RenderAdoption renders the only object workspace adoption may apply.
func RenderAdoption(options AdoptOptions) ([]byte, error) {
	options = options.withDefaults()
	if err := options.validate(); err != nil {
		return nil, err
	}
	return renderAdoption(options, ObjectMeta{})
}

type objectMetadata struct {
	Name              string            `json:"name"`
	UID               string            `json:"uid"`
	ResourceVersion   string            `json:"resourceVersion"`
	DeletionTimestamp string            `json:"deletionTimestamp"`
	Labels            map[string]string `json:"labels"`
}

type namespaceDocument struct {
	Metadata objectMetadata `json:"metadata"`
}

type localQueueDocument struct {
	Metadata objectMetadata `json:"metadata"`
	Spec     struct {
		ClusterQueue string `json:"clusterQueue"`
	} `json:"spec"`
}

type clusterQueueDocument struct {
	Metadata objectMetadata `json:"metadata"`
}

type pvcDocument struct {
	Metadata objectMetadata `json:"metadata"`
	Spec     struct {
		StorageClassName string `json:"storageClassName"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type storageClassDocument struct {
	Metadata objectMetadata `json:"metadata"`
}

// AdoptionPreflight records the immutable identities and contracts observed by
// the read-only preflight.
type AdoptionPreflight struct {
	NamespaceUID            string
	QueueUID                string
	ClusterQueueUID         string
	PVCUID                  string
	StorageClassUID         string
	ResolvedClusterQueue    string
	ResolvedStorageClass    string
	ExistingWorkspace       bool
	ExistingWorkspaceUID    string
	ExistingWorkspaceRV     string
	ExistingWorkspaceIntent string
	DataPVC                 string
	Namespace               string
	Queue                   string
}

// Summary returns a concise deterministic result suitable for CLI output.
func (p AdoptionPreflight) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "preflight passed: Namespace %s; LocalQueue %s/%s -> ClusterQueue %s",
		p.Namespace, p.Namespace, p.Queue, p.ResolvedClusterQueue)
	if p.DataPVC != "" {
		fmt.Fprintf(&b, "; PVC %s/%s Bound (storageClass=%s)", p.Namespace, p.DataPVC, p.ResolvedStorageClass)
	}
	fmt.Fprintf(&b, "; TauWorkspace %s", p.ExistingWorkspaceIntent)
	return b.String()
}

func getJSON(ctx context.Context, runner AdoptRunner, args []string, target string, out any) error {
	raw, err := runner.Raw(ctx, args, nil)
	if err != nil {
		return fmt.Errorf("preflight: read %s: %w", target, err)
	}
	if err := json.Unmarshal([]byte(raw), out); err != nil {
		return fmt.Errorf("preflight: parse %s: %w", target, err)
	}
	return nil
}

func requireIdentity(kind, name, observed, expected string) error {
	if observed == "" {
		return fmt.Errorf("preflight: %s %q has no metadata.uid; refusing adoption without a stable identity", kind, name)
	}
	if expected != "" && observed != expected {
		return fmt.Errorf("preflight: %s %q UID is %q, want exactly %q", kind, name, observed, expected)
	}
	return nil
}

func requireNotTerminating(kind, name, deletionTimestamp string) error {
	if deletionTimestamp != "" {
		return fmt.Errorf("preflight: %s %q is terminating since %s", kind, name, deletionTimestamp)
	}
	return nil
}

// PreflightAdoption validates the existing namespace, queue, backing
// ClusterQueue, optional PVC, and any existing TauWorkspace without mutation.
func PreflightAdoption(ctx context.Context, runner AdoptRunner, options AdoptOptions) (AdoptionPreflight, error) {
	o := options.withDefaults()
	if err := o.validate(); err != nil {
		return AdoptionPreflight{}, err
	}
	desired := desiredAdoption(o)
	raw, err := runner.Raw(ctx,
		[]string{"-n", o.PlatformNamespace, "get", tauWorkspaceResource, "-o", "json"}, nil)
	if err != nil {
		return AdoptionPreflight{}, fmt.Errorf("preflight: list TauWorkspaces in %q: %w", o.PlatformNamespace, err)
	}
	list, err := ParseList([]byte(raw))
	if err != nil {
		return AdoptionPreflight{}, fmt.Errorf("preflight: %w", err)
	}
	if len(list.Items) > 1 {
		return AdoptionPreflight{}, fmt.Errorf(
			"preflight: v0 supports one TauWorkspace, but %d already exist in %q",
			len(list.Items),
			o.PlatformNamespace,
		)
	}
	if len(list.Items) == 1 {
		existing := list.Items[0]
		if err := requireNotTerminating("TauWorkspace", o.PlatformNamespace+"/"+existing.Metadata.Name, existing.Metadata.DeletionTimestamp); err != nil {
			return AdoptionPreflight{}, err
		}
		if existing.Metadata.Name != o.Name || !sameAdoptionIntent(existing, desired) {
			return AdoptionPreflight{}, fmt.Errorf(
				"preflight: v0 supports one active workspace and TauWorkspace %q already exists with different intent",
				existing.Metadata.Name,
			)
		}
	}

	var ns namespaceDocument
	if err := getJSON(ctx, runner, []string{"get", namespaceResource, o.Namespace, "-o", "json"}, fmt.Sprintf("Namespace %q", o.Namespace), &ns); err != nil {
		return AdoptionPreflight{}, err
	}
	if err := requireIdentity("Namespace", o.Namespace, ns.Metadata.UID, o.NamespaceUID); err != nil {
		return AdoptionPreflight{}, err
	}
	if err := requireNotTerminating("Namespace", o.Namespace, ns.Metadata.DeletionTimestamp); err != nil {
		return AdoptionPreflight{}, err
	}
	if owner := ns.Metadata.Labels[workloadmeta.LabelWorkspace]; owner != "" && owner != o.Name {
		return AdoptionPreflight{}, fmt.Errorf("preflight: Namespace %q is assigned to TauWorkspace %q, not %q", o.Namespace, owner, o.Name)
	}
	if queue := ns.Metadata.Labels[defaultLocalQueueLabel]; queue != "" && queue != o.Queue {
		return AdoptionPreflight{}, fmt.Errorf("preflight: Namespace %q default LocalQueue is %q, want exactly %q", o.Namespace, queue, o.Queue)
	}

	var localQueue localQueueDocument
	if err := getJSON(ctx, runner,
		[]string{"-n", o.Namespace, "get", localQueueResource, o.Queue, "-o", "json"},
		fmt.Sprintf("LocalQueue %s/%s", o.Namespace, o.Queue), &localQueue); err != nil {
		return AdoptionPreflight{}, err
	}
	if err := requireIdentity("LocalQueue", o.Namespace+"/"+o.Queue, localQueue.Metadata.UID, o.QueueUID); err != nil {
		return AdoptionPreflight{}, err
	}
	if err := requireNotTerminating("LocalQueue", o.Namespace+"/"+o.Queue, localQueue.Metadata.DeletionTimestamp); err != nil {
		return AdoptionPreflight{}, err
	}
	clusterQueue := strings.TrimSpace(localQueue.Spec.ClusterQueue)
	if clusterQueue == "" {
		return AdoptionPreflight{}, fmt.Errorf("preflight: LocalQueue %s/%s does not reference a ClusterQueue", o.Namespace, o.Queue)
	}
	if o.ClusterQueue != "" && clusterQueue != o.ClusterQueue {
		return AdoptionPreflight{}, fmt.Errorf("preflight: LocalQueue %s/%s references ClusterQueue %q, want exactly %q", o.Namespace, o.Queue, clusterQueue, o.ClusterQueue)
	}

	var cq clusterQueueDocument
	if err := getJSON(ctx, runner,
		[]string{"get", clusterQueueResource, clusterQueue, "-o", "json"},
		fmt.Sprintf("ClusterQueue %q", clusterQueue), &cq); err != nil {
		return AdoptionPreflight{}, err
	}
	if cq.Metadata.Name != clusterQueue {
		return AdoptionPreflight{}, fmt.Errorf("preflight: requested ClusterQueue %q but server returned %q", clusterQueue, cq.Metadata.Name)
	}
	if err := requireIdentity("ClusterQueue", clusterQueue, cq.Metadata.UID, ""); err != nil {
		return AdoptionPreflight{}, err
	}
	if err := requireNotTerminating("ClusterQueue", clusterQueue, cq.Metadata.DeletionTimestamp); err != nil {
		return AdoptionPreflight{}, err
	}

	report := AdoptionPreflight{
		NamespaceUID:         ns.Metadata.UID,
		QueueUID:             localQueue.Metadata.UID,
		ClusterQueueUID:      cq.Metadata.UID,
		ResolvedClusterQueue: clusterQueue,
		DataPVC:              o.DataPVC,
		Namespace:            o.Namespace,
		Queue:                o.Queue,
	}

	if o.DataPVC != "" {
		var pvc pvcDocument
		if err := getJSON(ctx, runner,
			[]string{"-n", o.Namespace, "get", pvcResource, o.DataPVC, "-o", "json"},
			fmt.Sprintf("PVC %s/%s", o.Namespace, o.DataPVC), &pvc); err != nil {
			return AdoptionPreflight{}, err
		}
		if err := requireIdentity("PVC", o.Namespace+"/"+o.DataPVC, pvc.Metadata.UID, o.PVCUID); err != nil {
			return AdoptionPreflight{}, err
		}
		if err := requireNotTerminating("PVC", o.Namespace+"/"+o.DataPVC, pvc.Metadata.DeletionTimestamp); err != nil {
			return AdoptionPreflight{}, err
		}
		if pvc.Status.Phase != "Bound" {
			return AdoptionPreflight{}, fmt.Errorf("preflight: PVC %s/%s phase is %q, want exactly %q", o.Namespace, o.DataPVC, pvc.Status.Phase, "Bound")
		}
		if o.StorageClass != "" && pvc.Spec.StorageClassName != o.StorageClass {
			return AdoptionPreflight{}, fmt.Errorf("preflight: PVC %s/%s storageClassName is %q, want exactly %q", o.Namespace, o.DataPVC, pvc.Spec.StorageClassName, o.StorageClass)
		}
		if pvc.Spec.StorageClassName == "" {
			return AdoptionPreflight{}, fmt.Errorf("preflight: PVC %s/%s has no storageClassName", o.Namespace, o.DataPVC)
		}
		var storageClass storageClassDocument
		if err := getJSON(ctx, runner,
			[]string{"get", storageClassResource, pvc.Spec.StorageClassName, "-o", "json"},
			fmt.Sprintf("StorageClass %q", pvc.Spec.StorageClassName), &storageClass); err != nil {
			return AdoptionPreflight{}, err
		}
		if storageClass.Metadata.Name != pvc.Spec.StorageClassName {
			return AdoptionPreflight{}, fmt.Errorf("preflight: requested StorageClass %q but server returned %q", pvc.Spec.StorageClassName, storageClass.Metadata.Name)
		}
		if err := requireIdentity("StorageClass", pvc.Spec.StorageClassName, storageClass.Metadata.UID, ""); err != nil {
			return AdoptionPreflight{}, err
		}
		if err := requireNotTerminating("StorageClass", pvc.Spec.StorageClassName, storageClass.Metadata.DeletionTimestamp); err != nil {
			return AdoptionPreflight{}, err
		}
		report.PVCUID = pvc.Metadata.UID
		report.StorageClassUID = storageClass.Metadata.UID
		report.ResolvedStorageClass = pvc.Spec.StorageClassName
	}

	raw, err = runner.Raw(ctx,
		[]string{"-n", o.PlatformNamespace, "get", tauWorkspaceResource, o.Name, "--ignore-not-found", "-o", "json"}, nil)
	if err != nil {
		return AdoptionPreflight{}, fmt.Errorf("preflight: read TauWorkspace %s/%s: %w", o.PlatformNamespace, o.Name, err)
	}
	if strings.TrimSpace(raw) == "" {
		report.ExistingWorkspaceIntent = "absent"
		return report, nil
	}
	existing, err := Parse([]byte(raw))
	if err != nil {
		return AdoptionPreflight{}, fmt.Errorf("preflight: %w", err)
	}
	if existing.Metadata.UID == "" || existing.Metadata.ResourceVersion == "" {
		return AdoptionPreflight{}, fmt.Errorf("preflight: existing TauWorkspace %s/%s lacks metadata.uid or metadata.resourceVersion", o.PlatformNamespace, o.Name)
	}
	if err := requireNotTerminating("TauWorkspace", o.PlatformNamespace+"/"+o.Name, existing.Metadata.DeletionTimestamp); err != nil {
		return AdoptionPreflight{}, err
	}
	if !sameAdoptionIntent(existing, desired) {
		return AdoptionPreflight{}, fmt.Errorf("preflight: existing TauWorkspace %s/%s has a conflicting spec; refusing to overwrite it", o.PlatformNamespace, o.Name)
	}
	report.ExistingWorkspace = true
	report.ExistingWorkspaceUID = existing.Metadata.UID
	report.ExistingWorkspaceRV = existing.Metadata.ResourceVersion
	report.ExistingWorkspaceIntent = "compatible"
	return report, nil
}

func sameAdoptionIntent(existing, desired Workspace) bool {
	existingSpec := existing.Spec
	desiredSpec := desired.Spec
	if existingSpec.Target.Namespace == "" {
		existingSpec.Target.Namespace = existing.Metadata.Name
	}
	if desiredSpec.Target.Namespace == "" {
		desiredSpec.Target.Namespace = desired.Metadata.Name
	}
	return reflect.DeepEqual(existingSpec, desiredSpec)
}

func verifyStablePreflight(before, after AdoptionPreflight) error {
	for resource, identities := range map[string][2]string{
		"Namespace":    {before.NamespaceUID, after.NamespaceUID},
		"LocalQueue":   {before.QueueUID, after.QueueUID},
		"ClusterQueue": {before.ClusterQueueUID, after.ClusterQueueUID},
		"PVC":          {before.PVCUID, after.PVCUID},
		"StorageClass": {before.StorageClassUID, after.StorageClassUID},
	} {
		if identities[0] != identities[1] {
			return fmt.Errorf("preflight: %s identity changed before apply (%q -> %q); refusing to continue", resource, identities[0], identities[1])
		}
	}
	if before.ExistingWorkspace != after.ExistingWorkspace ||
		before.ExistingWorkspaceUID != after.ExistingWorkspaceUID {
		return fmt.Errorf("preflight: TauWorkspace identity changed before apply; refusing to continue")
	}
	return nil
}

// ApplyAdoption repeats preflight, verifies observed identities are stable,
// then server-side dry-runs and conditionally creates only the TauWorkspace.
// A compatible existing workspace is an idempotent no-op.
func ApplyAdoption(ctx context.Context, runner AdoptRunner, options AdoptOptions, initial AdoptionPreflight) (string, error) {
	o := options.withDefaults()
	current, err := PreflightAdoption(ctx, runner, o)
	if err != nil {
		return "", err
	}
	if err := verifyStablePreflight(initial, current); err != nil {
		return "", err
	}
	if current.ExistingWorkspace {
		return fmt.Sprintf(
			"TauWorkspace %s/%s already exists with compatible intent; no changes applied\n",
			o.PlatformNamespace,
			o.Name,
		), nil
	}
	manifest, err := renderAdoption(o, ObjectMeta{})
	if err != nil {
		return "", err
	}
	args := []string{
		"-n", o.PlatformNamespace,
		"create",
		"-f", "-",
	}
	dryRunArgs := append(append([]string(nil), args...), "--dry-run=server")
	if _, err := runner.Raw(ctx, dryRunArgs, manifest); err != nil {
		return "", fmt.Errorf("TauWorkspace server-side dry-run failed: %w", err)
	}
	out, err := runner.Raw(ctx, args, manifest)
	if err != nil {
		return out, fmt.Errorf("TauWorkspace create failed; re-run adoption preflight before retrying: %w", err)
	}
	return out, nil
}
