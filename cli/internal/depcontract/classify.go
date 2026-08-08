// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package depcontract classifies the cluster-local dependencies a rendered
// Tau Job/RayJob manifest declares: Secrets, PersistentVolumeClaims,
// ServiceAccounts, image pull secrets, CSI SecretProviderClasses, and
// container images. It never reads Secret/CSI *values* — only the names
// used to reference them — so it is safe to run against redacted or
// unredacted manifests alike, and it never contacts a cluster.
//
// This package is the mechanical half of the MultiKueue dependency
// contract; see the public multicluster guide for what
// each Kind means and which ones require pre-provisioned name parity across
// MultiKueue worker clusters. internal/platform consumes this package's
// output to run read-only existence/parity checks against explicitly
// supplied worker kube contexts.
package depcontract

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"

	"gopkg.in/yaml.v3"
)

// Kind identifies the category of a classified dependency.
type Kind string

const (
	// KindSecret is a Kubernetes Secret referenced by name (env
	// valueFrom.secretKeyRef, envFrom.secretRef, or a secret volume).
	// Pre-provisioned, name-parity required on every worker.
	KindSecret Kind = "Secret"
	// KindSecretProviderClass is a CSI Secrets Store SecretProviderClass
	// referenced by a csi volume with driver
	// "secrets-store.csi.k8s.io". Pre-provisioned, name-parity required.
	KindSecretProviderClass Kind = "SecretProviderClass"
	// KindPersistentVolumeClaim is a PVC referenced by
	// volumes[].persistentVolumeClaim.claimName. Pre-provisioned; its
	// *contents* are synchronized out-of-band, never by Tau.
	KindPersistentVolumeClaim Kind = "PersistentVolumeClaim"
	// KindServiceAccount is spec.serviceAccountName (or the deprecated
	// serviceAccount alias). Pre-provisioned, name-parity required.
	KindServiceAccount Kind = "ServiceAccount"
	// KindImagePullSecret is spec.imagePullSecrets[].name. Pre-provisioned,
	// name-parity required.
	KindImagePullSecret Kind = "ImagePullSecret"
	// KindImage is a container/initContainer image reference. Portable in
	// the spec, but pull availability cannot be proven by a read-only
	// kubectl get; see the public multicluster guide
	// and issue #871.
	KindImage Kind = "Image"
	// KindUnsupported is any dependency shape outside the MultiKueue
	// dependency contract: arbitrary ConfigMap references, or a volume
	// source this package does not recognize as portable
	// (hostPath, projected, nfs, etc.).
	KindUnsupported Kind = "Unsupported"
)

// Role identifies which pod template within a workload referenced a
// dependency. A RayJob's head and worker groups have independent pod
// templates, so the same dependency name can be referenced by one, the
// other, or both.
type Role string

const (
	RoleJob       Role = "job"
	RoleHead      Role = "head"
	RoleWorker    Role = "worker"
	RoleSubmitter Role = "submitter"
)

const secretsStoreCSIDriver = "secrets-store.csi.k8s.io"

// RedactedPlaceholder is the name/key substituted by
// internal/envspec.RedactSecretRefs for client dry-run output. A Dependency
// whose Name equals this placeholder does not identify a real object and
// must never be treated as one — callers (e.g. internal/platform) should
// refuse to preflight-check it rather than silently reporting a false
// pass or fail against a placeholder name.
const RedactedPlaceholder = "<redacted>"

const unsupportedConfigMapDetail = "ConfigMap references are not part of the MultiKueue dependency contract; see https://github.com/Azure/taugrid/blob/main/site/content/en/docs/operations/multicluster.md"

// Dependency is one classified reference to a cluster-local object (or, for
// Kind == KindImage, a container image reference). Name is the identifying
// value used to look the object up on a worker cluster — never a secret
// value. Namespace is empty for cluster-scoped or non-namespaced kinds
// (KindImage).
type Dependency struct {
	Kind      Kind
	Namespace string
	Name      string
	// Field is the manifest field path the reference was first found at,
	// e.g. "spec.template.spec.volumes[0].persistentVolumeClaim.claimName".
	// Diagnostic only; not part of identity/dedup.
	Field string
	// Roles lists every pod role that references this (Kind, Namespace,
	// Name) tuple, deduped and sorted.
	Roles []Role
	// Detail carries extra, non-sensitive context, e.g. the CSI driver
	// name for a SecretProviderClass, or why something is Unsupported.
	// Never a secret value.
	Detail string
}

// HasRole reports whether role is one of the pod roles that reference d.
func (d Dependency) HasRole(role Role) bool {
	for _, r := range d.Roles {
		if r == role {
			return true
		}
	}
	return false
}

// IsRedactedPlaceholder reports whether d.Name is the client dry-run
// redaction placeholder rather than a real object name (see
// RedactedPlaceholder).
func (d Dependency) IsRedactedPlaceholder() bool {
	return d.Name == RedactedPlaceholder
}

// WorkloadDependencies is the classification result for one workload object
// (one Job, or one RayJob) found in a rendered manifest.
type WorkloadDependencies struct {
	Kind         string // "Job" or "RayJob"
	Name         string
	Namespace    string
	Dependencies []Dependency
}

// Classify parses a rendered manifest (one or more YAML documents) and
// returns the classified dependencies for every Job or RayJob document it
// contains. Documents of other kinds are ignored: this package classifies
// what a *workload* depends on, not every object a submit path may render
// alongside it. Classify never reads Secret/CSI values and never contacts a
// cluster.
func Classify(manifest []byte) ([]WorkloadDependencies, error) {
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	var out []WorkloadDependencies
	for {
		var obj map[string]any
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("depcontract: parse manifest: %w", err)
		}
		if len(obj) == 0 {
			continue
		}
		switch kind, _ := obj["kind"].(string); kind {
		case "Job":
			out = append(out, classifyJob(obj))
		case "RayJob":
			out = append(out, classifyRayJob(obj))
		}
	}
	return out, nil
}

// Flatten merges dependencies across every workload (e.g. when a rendered
// manifest contains more than one Job/RayJob document), deduping by
// (Kind, Namespace, Name) and merging Roles. The result is sorted
// deterministically by Kind, then Namespace, then Name.
func Flatten(workloads []WorkloadDependencies) []Dependency {
	type mkey struct {
		kind Kind
		ns   string
		name string
	}
	merged := map[mkey]*Dependency{}
	var order []mkey
	for _, wd := range workloads {
		for _, d := range wd.Dependencies {
			k := mkey{d.Kind, d.Namespace, d.Name}
			existing, ok := merged[k]
			if !ok {
				cp := d
				cp.Roles = append([]Role(nil), d.Roles...)
				merged[k] = &cp
				order = append(order, k)
				continue
			}
			existing.Roles = mergeRoles(existing.Roles, d.Roles)
			if existing.Detail == "" {
				existing.Detail = d.Detail
			}
			if existing.Field == "" {
				existing.Field = d.Field
			}
		}
	}
	out := make([]Dependency, 0, len(order))
	for _, k := range order {
		out = append(out, *merged[k])
	}
	sortDependencies(out)
	return out
}

func workloadIdentity(obj map[string]any) (name, namespace string) {
	meta, _ := obj["metadata"].(map[string]any)
	return stringAt(meta, "name"), stringAt(meta, "namespace")
}

func classifyJob(obj map[string]any) WorkloadDependencies {
	name, namespace := workloadIdentity(obj)
	c := newCollector(namespace)
	spec := mapAt(obj, "spec", "template", "spec")
	c.walkPodSpec(spec, RoleJob, "spec.template.spec")
	return WorkloadDependencies{
		Kind:         "Job",
		Name:         name,
		Namespace:    namespace,
		Dependencies: c.finish(),
	}
}

func classifyRayJob(obj map[string]any) WorkloadDependencies {
	name, namespace := workloadIdentity(obj)
	c := newCollector(namespace)

	headSpec := mapAt(obj, "spec", "rayClusterSpec", "headGroupSpec", "template", "spec")
	c.walkPodSpec(headSpec, RoleHead, "spec.rayClusterSpec.headGroupSpec.template.spec")

	workerGroups := sliceAt(obj, "spec", "rayClusterSpec", "workerGroupSpecs")
	for i, wg := range workerGroups {
		wgMap, ok := wg.(map[string]any)
		if !ok {
			continue
		}
		spec := mapAt(wgMap, "template", "spec")
		field := fmt.Sprintf("spec.rayClusterSpec.workerGroupSpecs[%d].template.spec", i)
		c.walkPodSpec(spec, RoleWorker, field)
	}

	// KubeRay's RayJob CRD supports an optional submitterPodTemplate for
	// the job-submitter pod. Tau's rayjobrender does not render one
	// today, but classify it defensively for forward compatibility and
	// for operator-authored manifests.
	if submitterSpec := mapAt(obj, "spec", "submitterPodTemplate", "spec"); submitterSpec != nil {
		c.walkPodSpec(submitterSpec, RoleSubmitter, "spec.submitterPodTemplate.spec")
	}

	return WorkloadDependencies{
		Kind:         "RayJob",
		Name:         name,
		Namespace:    namespace,
		Dependencies: c.finish(),
	}
}

// collector accumulates dependency references for a single workload,
// deduping by (Kind, effective namespace, Name) while tracking every pod
// role that referenced each one.
type collector struct {
	namespace string
	order     []collectorKey
	byKey     map[collectorKey]*Dependency
	roles     map[collectorKey]map[Role]bool
}

type collectorKey struct {
	kind Kind
	ns   string
	name string
}

func newCollector(namespace string) *collector {
	return &collector{
		namespace: namespace,
		byKey:     map[collectorKey]*Dependency{},
		roles:     map[collectorKey]map[Role]bool{},
	}
}

func (c *collector) add(kind Kind, name, field, detail string, role Role) {
	if name == "" {
		return
	}
	ns := c.namespace
	if kind == KindImage {
		ns = ""
	}
	key := collectorKey{kind, ns, name}
	dep, ok := c.byKey[key]
	if !ok {
		dep = &Dependency{Kind: kind, Namespace: ns, Name: name, Field: field, Detail: detail}
		c.byKey[key] = dep
		c.roles[key] = map[Role]bool{}
		c.order = append(c.order, key)
	}
	if dep.Detail == "" && detail != "" {
		dep.Detail = detail
	}
	c.roles[key][role] = true
}

func (c *collector) finish() []Dependency {
	out := make([]Dependency, 0, len(c.order))
	for _, key := range c.order {
		dep := *c.byKey[key]
		roleSet := c.roles[key]
		roles := make([]Role, 0, len(roleSet))
		for r := range roleSet {
			roles = append(roles, r)
		}
		sort.Slice(roles, func(i, j int) bool { return roles[i] < roles[j] })
		dep.Roles = roles
		out = append(out, dep)
	}
	sortDependencies(out)
	return out
}

func sortDependencies(deps []Dependency) {
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].Kind != deps[j].Kind {
			return deps[i].Kind < deps[j].Kind
		}
		if deps[i].Namespace != deps[j].Namespace {
			return deps[i].Namespace < deps[j].Namespace
		}
		return deps[i].Name < deps[j].Name
	})
}

func mergeRoles(a, b []Role) []Role {
	set := map[Role]bool{}
	for _, r := range a {
		set[r] = true
	}
	for _, r := range b {
		set[r] = true
	}
	out := make([]Role, 0, len(set))
	for r := range set {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (c *collector) walkPodSpec(spec map[string]any, role Role, field string) {
	if spec == nil {
		return
	}

	if sa := nonEmpty(stringAt(spec, "serviceAccountName"), stringAt(spec, "serviceAccount")); sa != "" {
		c.add(KindServiceAccount, sa, field+".serviceAccountName", "", role)
	}

	if ips, ok := spec["imagePullSecrets"].([]any); ok {
		for i, e := range ips {
			m, _ := e.(map[string]any)
			if name := stringAt(m, "name"); name != "" {
				c.add(KindImagePullSecret, name, fmt.Sprintf("%s.imagePullSecrets[%d].name", field, i), "", role)
			}
		}
	}

	if vols, ok := spec["volumes"].([]any); ok {
		for i, v := range vols {
			vm, _ := v.(map[string]any)
			c.walkVolume(vm, role, fmt.Sprintf("%s.volumes[%d]", field, i))
		}
	}

	c.walkContainers(spec["initContainers"], role, field+".initContainers")
	c.walkContainers(spec["containers"], role, field+".containers")
	c.walkContainers(spec["ephemeralContainers"], role, field+".ephemeralContainers")
}

func (c *collector) walkVolume(vm map[string]any, role Role, field string) {
	if vm == nil {
		return
	}
	volName := stringAt(vm, "name")

	if pvc, ok := vm["persistentVolumeClaim"].(map[string]any); ok {
		if claim := stringAt(pvc, "claimName"); claim != "" {
			c.add(KindPersistentVolumeClaim, claim, field+".persistentVolumeClaim.claimName", "", role)
		}
		return
	}
	if sm, ok := vm["secret"].(map[string]any); ok {
		if name := stringAt(sm, "secretName"); name != "" {
			c.add(KindSecret, name, field+".secret.secretName", "", role)
		}
		return
	}
	if csi, ok := vm["csi"].(map[string]any); ok {
		driver := stringAt(csi, "driver")
		if driver == secretsStoreCSIDriver {
			attrs, _ := csi["volumeAttributes"].(map[string]any)
			if spc := stringAt(attrs, "secretProviderClass"); spc != "" {
				c.add(KindSecretProviderClass, spc, field+".csi.volumeAttributes.secretProviderClass", "driver="+driver, role)
			}
			return
		}
		name := nonEmpty(volName, field)
		c.add(KindUnsupported, name, field+".csi", "unsupported CSI driver "+describeValue(driver), role)
		return
	}
	if cm, ok := vm["configMap"].(map[string]any); ok {
		name := nonEmpty(stringAt(cm, "name"), volName, field)
		c.add(KindUnsupported, name, field+".configMap", unsupportedConfigMapDetail, role)
		return
	}
	if _, ok := vm["emptyDir"].(map[string]any); ok {
		return // portable: contains no external reference
	}
	if _, ok := vm["downwardAPI"].(map[string]any); ok {
		return // portable: derived entirely from the pod's own spec
	}

	// Any other volume source (hostPath, projected, nfs, azureFile, ...)
	// is outside the portable/pre-provisioned contract until a concrete
	// need arises; flag it rather than silently ignore it.
	name := nonEmpty(volName, field)
	c.add(KindUnsupported, name, field, "unsupported volume source: "+volumeSourceKeys(vm), role)
}

func (c *collector) walkContainers(v any, role Role, field string) {
	list, ok := v.([]any)
	if !ok {
		return
	}
	for i, item := range list {
		cm, ok := item.(map[string]any)
		if !ok {
			continue
		}
		containerField := fmt.Sprintf("%s[%d]", field, i)
		if img := stringAt(cm, "image"); img != "" {
			c.add(KindImage, img, containerField+".image", "", role)
		}
		c.walkEnv(cm["env"], role, containerField+".env")
		c.walkEnvFrom(cm["envFrom"], role, containerField+".envFrom")
	}
}

func (c *collector) walkEnv(v any, role Role, field string) {
	list, ok := v.([]any)
	if !ok {
		return
	}
	for i, item := range list {
		em, ok := item.(map[string]any)
		if !ok {
			continue
		}
		vf, ok := em["valueFrom"].(map[string]any)
		if !ok {
			continue
		}
		envField := fmt.Sprintf("%s[%d].valueFrom", field, i)
		if ref, ok := vf["secretKeyRef"].(map[string]any); ok {
			if name := stringAt(ref, "name"); name != "" {
				c.add(KindSecret, name, envField+".secretKeyRef.name", "", role)
			}
		}
		if ref, ok := vf["configMapKeyRef"].(map[string]any); ok {
			name := nonEmpty(stringAt(ref, "name"), envField)
			c.add(KindUnsupported, name, envField+".configMapKeyRef.name", unsupportedConfigMapDetail, role)
		}
	}
}

func (c *collector) walkEnvFrom(v any, role Role, field string) {
	list, ok := v.([]any)
	if !ok {
		return
	}
	for i, item := range list {
		em, ok := item.(map[string]any)
		if !ok {
			continue
		}
		itemField := fmt.Sprintf("%s[%d]", field, i)
		if ref, ok := em["secretRef"].(map[string]any); ok {
			if name := stringAt(ref, "name"); name != "" {
				c.add(KindSecret, name, itemField+".secretRef.name", "", role)
			}
		}
		if ref, ok := em["configMapRef"].(map[string]any); ok {
			name := nonEmpty(stringAt(ref, "name"), itemField)
			c.add(KindUnsupported, name, itemField+".configMapRef.name", unsupportedConfigMapDetail, role)
		}
	}
}

// --- small, dependency-free YAML tree helpers ---

func mapAt(obj map[string]any, keys ...string) map[string]any {
	cur := obj
	for _, k := range keys {
		if cur == nil {
			return nil
		}
		next, ok := cur[k].(map[string]any)
		if !ok {
			return nil
		}
		cur = next
	}
	return cur
}

func sliceAt(obj map[string]any, keys ...string) []any {
	if len(keys) == 0 {
		return nil
	}
	parent := mapAt(obj, keys[:len(keys)-1]...)
	if parent == nil {
		return nil
	}
	s, _ := parent[keys[len(keys)-1]].([]any)
	return s
}

func stringAt(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func nonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func describeValue(v string) string {
	if v == "" {
		return "(unspecified)"
	}
	return v
}

func volumeSourceKeys(vm map[string]any) string {
	keys := make([]string, 0, len(vm))
	for k := range vm {
		if k == "name" {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return "(unrecognized)"
	}
	out := keys[0]
	for _, k := range keys[1:] {
		out += "," + k
	}
	return out
}
