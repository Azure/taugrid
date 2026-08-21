// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package workspace contains the client-side shape of TauWorkspace objects.
package workspace

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"
)

const (
	APIGroup         = "tau.azure.com"
	APIVersion       = APIGroup + "/v1alpha1"
	KindWorkspace    = "TauWorkspace"
	KindQuotaRequest = "TauQuotaRequest"
	SystemNamespace  = "tau-system"
	// LegacySystemNamespace is used only when reading connection artifacts
	// created before cluster.systemNamespace was part of their contract. New
	// installs and newly generated descriptors always write SystemNamespace.
	LegacySystemNamespace = "tau-platform"

	AuthorizationModeWorkspaceRBAC = "workspace-rbac"
	AuthorizationModeClusterWide   = "cluster-wide"
)

type ObjectMeta struct {
	Name              string            `json:"name" yaml:"name"`
	Namespace         string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty" yaml:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty" yaml:"resourceVersion,omitempty"`
	CreationTimestamp string            `json:"creationTimestamp,omitempty" yaml:"creationTimestamp,omitempty"`
	DeletionTimestamp string            `json:"deletionTimestamp,omitempty" yaml:"deletionTimestamp,omitempty"`
	Generation        int64             `json:"generation,omitempty" yaml:"generation,omitempty"`
	Labels            map[string]string `json:"labels,omitempty" yaml:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty" yaml:"annotations,omitempty"`
}

type PrincipalRef struct {
	Provider string `json:"provider" yaml:"provider"`
	Name     string `json:"name" yaml:"name"`
}

type KubernetesSubject struct {
	Kind string `json:"kind" yaml:"kind"`
	Name string `json:"name" yaml:"name"`
}

type WorkspaceAuthorization struct {
	Mode string `json:"mode,omitempty" yaml:"mode,omitempty"`
}

type WorkspaceTarget struct {
	Namespace       string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	CreateNamespace bool   `json:"createNamespace,omitempty" yaml:"createNamespace,omitempty"`
}

type WorkspaceDefaults struct {
	OutputRoot string `json:"outputRoot,omitempty" yaml:"outputRoot,omitempty"`
	// ScratchMount is not part of the TauWorkspace CRD schema, so the API
	// server prunes it on write. It is kept here only so reads of an object
	// that somehow carries it do not fail; do not add new consumers.
	ScratchMount string `json:"scratchMount,omitempty" yaml:"scratchMount,omitempty"`
	Priority     string `json:"priority,omitempty" yaml:"priority,omitempty"`
	// TTLSecondsAfterFinished mirrors WorkspaceDefaults.ttlSecondsAfterFinished
	// in the CRD: the workspace-level default retention for finished batch
	// Jobs. A pointer keeps "unset" distinct from an explicit value, because
	// zero is a legal Kubernetes TTL meaning "delete immediately".
	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty" yaml:"ttlSecondsAfterFinished,omitempty"`
}

// MinTTLSecondsAfterFinished mirrors the CRD's durability floor for
// WorkspaceDefaults.ttlSecondsAfterFinished. The CRD enforces it on write, but
// a workspace created against an older schema can still carry a shorter value,
// so readers re-check rather than trust the stored object.
//
// The floor exists because finished-Job retention races the lifecycle
// recorder: below roughly an order of magnitude above its observation
// interval, a Job that finishes just after a poll is collected with its pods
// before the next pass, and the durable record is lost rather than shortened.
const MinTTLSecondsAfterFinished int64 = 600

type WorkspaceWorkloadIdentity struct {
	ServiceAccountName string `json:"serviceAccountName,omitempty" yaml:"serviceAccountName,omitempty"`
	ClientID           string `json:"clientId,omitempty" yaml:"clientId,omitempty"`
	TenantID           string `json:"tenantId,omitempty" yaml:"tenantId,omitempty"`
}

type WorkspaceSpec struct {
	Authorization     *WorkspaceAuthorization    `json:"authorization,omitempty" yaml:"authorization,omitempty"`
	PrincipalRef      PrincipalRef               `json:"principalRef,omitempty" yaml:"principalRef,omitempty"`
	KubernetesSubject KubernetesSubject          `json:"kubernetesSubject,omitempty" yaml:"kubernetesSubject,omitempty"`
	Role              string                     `json:"role,omitempty" yaml:"role,omitempty"`
	Target            WorkspaceTarget            `json:"target,omitempty" yaml:"target,omitempty"`
	Queue             string                     `json:"queue" yaml:"queue"`
	Defaults          WorkspaceDefaults          `json:"defaults,omitempty" yaml:"defaults,omitempty"`
	WorkloadIdentity  *WorkspaceWorkloadIdentity `json:"workloadIdentity,omitempty" yaml:"workloadIdentity,omitempty"`
}

type WorkspaceTargetStatus struct {
	ResolvedNamespace string `json:"resolvedNamespace,omitempty"`
}

type WorkspaceQueueStatus struct {
	LocalQueue   string `json:"localQueue,omitempty"`
	ClusterQueue string `json:"clusterQueue,omitempty"`
}

type WorkspaceQuotaStatus struct {
	Resource string `json:"resource"`
	Flavor   string `json:"flavor,omitempty"`
	Nominal  int64  `json:"nominal,omitempty"`
	Admitted int64  `json:"admitted,omitempty"`
	Pending  int64  `json:"pending,omitempty"`
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type WorkspaceStatus struct {
	Phase              string                 `json:"phase,omitempty"`
	ObservedGeneration int64                  `json:"observedGeneration,omitempty"`
	Target             WorkspaceTargetStatus  `json:"target,omitempty"`
	Queue              WorkspaceQueueStatus   `json:"queue,omitempty"`
	Quota              []WorkspaceQuotaStatus `json:"quota,omitempty"`
	Conditions         []Condition            `json:"conditions,omitempty"`
}

type Workspace struct {
	APIVersion string          `json:"apiVersion" yaml:"apiVersion"`
	Kind       string          `json:"kind" yaml:"kind"`
	Metadata   ObjectMeta      `json:"metadata" yaml:"metadata"`
	Spec       WorkspaceSpec   `json:"spec" yaml:"spec"`
	Status     WorkspaceStatus `json:"status,omitempty" yaml:"status,omitempty"`
}

type WorkspaceList struct {
	Items []Workspace `json:"items"`
}

type ResolvedWorkspaceValues struct {
	Namespace      string `json:"namespace"`
	LocalQueue     string `json:"localQueue"`
	ServiceAccount string `json:"serviceAccount"`
}

type WorkspaceCLIView struct {
	Workspace
	Resolved ResolvedWorkspaceValues `json:"resolved"`
}

type QuotaRequestSpec struct {
	Workspace    string `json:"workspace" yaml:"workspace"`
	Resource     string `json:"resource" yaml:"resource"`
	Current      int64  `json:"current,omitempty" yaml:"current,omitempty"`
	Requested    int64  `json:"requested" yaml:"requested"`
	Duration     string `json:"duration,omitempty" yaml:"duration,omitempty"`
	Reason       string `json:"reason" yaml:"reason"`
	MutationMode string `json:"mutationMode,omitempty" yaml:"mutationMode,omitempty"`
}

type QuotaRequest struct {
	APIVersion string           `json:"apiVersion" yaml:"apiVersion"`
	Kind       string           `json:"kind" yaml:"kind"`
	Metadata   ObjectMeta       `json:"metadata" yaml:"metadata"`
	Spec       QuotaRequestSpec `json:"spec" yaml:"spec"`
}

func Parse(data []byte) (Workspace, error) {
	var out Workspace
	if err := json.Unmarshal(data, &out); err != nil {
		return Workspace{}, fmt.Errorf("parse TauWorkspace: %w", err)
	}
	if out.Kind != "" && out.Kind != KindWorkspace {
		return Workspace{}, fmt.Errorf("expected kind %s, got %s", KindWorkspace, out.Kind)
	}
	return out, nil
}

func ParseList(data []byte) (WorkspaceList, error) {
	var out WorkspaceList
	if err := json.Unmarshal(data, &out); err != nil {
		return WorkspaceList{}, fmt.Errorf("parse TauWorkspaceList: %w", err)
	}
	return out, nil
}

func RenderStatus(w Workspace) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace: %s\n", dash(w.Metadata.Name))
	fmt.Fprintf(&b, "  phase:      %s\n", dash(w.Status.Phase))
	fmt.Fprintf(&b, "  namespace:  %s\n", dash(ResolvedNamespace(w)))
	fmt.Fprintf(&b, "  queue:      %s\n", dash(ResolvedLocalQueue(w)))
	fmt.Fprintf(&b, "  serviceAccount: %s\n", dash(EffectiveServiceAccount(w)))
	if w.Status.Queue.ClusterQueue != "" {
		fmt.Fprintf(&b, "  clusterQ:   %s\n", w.Status.Queue.ClusterQueue)
	}
	if w.Spec.Defaults.OutputRoot != "" {
		fmt.Fprintf(&b, "  outputRoot: %s\n", w.Spec.Defaults.OutputRoot)
	}
	renderConditions(&b, w.Status.Conditions)
	renderQuota(&b, w.Status.Quota)
	return b.String()
}

func CLIView(w Workspace) WorkspaceCLIView {
	return WorkspaceCLIView{
		Workspace: w,
		Resolved: ResolvedWorkspaceValues{
			Namespace:      ResolvedNamespace(w),
			LocalQueue:     ResolvedLocalQueue(w),
			ServiceAccount: EffectiveServiceAccount(w),
		},
	}
}

func ResolvedNamespace(w Workspace) string {
	return firstNonEmpty(w.Status.Target.ResolvedNamespace, w.Spec.Target.Namespace)
}

func ResolvedLocalQueue(w Workspace) string {
	return firstNonEmpty(w.Status.Queue.LocalQueue, w.Spec.Queue)
}

func EffectiveServiceAccount(w Workspace) string {
	if w.Spec.WorkloadIdentity != nil {
		if name := strings.TrimSpace(w.Spec.WorkloadIdentity.ServiceAccountName); name != "" {
			return name
		}
	}
	return "default"
}

func RenderList(list WorkspaceList) string {
	if len(list.Items) == 0 {
		return "no workspaces found\n"
	}
	items := append([]Workspace(nil), list.Items...)
	sort.Slice(items, func(i, j int) bool { return items[i].Metadata.Name < items[j].Metadata.Name })
	var b strings.Builder
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tPHASE\tNAMESPACE\tQUEUE")
	for _, item := range items {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n",
			item.Metadata.Name,
			dash(item.Status.Phase),
			dash(item.Status.Target.ResolvedNamespace),
			dash(firstNonEmpty(item.Status.Queue.LocalQueue, item.Spec.Queue)),
		)
	}
	tw.Flush()
	return b.String()
}

func renderConditions(b *strings.Builder, conditions []Condition) {
	if len(conditions) == 0 {
		return
	}
	b.WriteString("\nConditions:\n")
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  TYPE\tSTATUS\tREASON\tMESSAGE")
	for _, c := range conditions {
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", c.Type, c.Status, dash(c.Reason), dash(c.Message))
	}
	tw.Flush()
}

func renderQuota(b *strings.Builder, quota []WorkspaceQuotaStatus) {
	if len(quota) == 0 {
		return
	}
	b.WriteString("\nQuota:\n")
	tw := tabwriter.NewWriter(b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  RESOURCE\tFLAVOR\tNOMINAL\tADMITTED\tPENDING")
	for _, q := range quota {
		fmt.Fprintf(tw, "  %s\t%s\t%d\t%d\t%d\n", q.Resource, dash(q.Flavor), q.Nominal, q.Admitted, q.Pending)
	}
	tw.Flush()
}

func Ready(w Workspace) bool {
	return w.Status.Phase == "Ready" &&
		w.Metadata.Generation > 0 &&
		w.Status.ObservedGeneration == w.Metadata.Generation
}

func NewQuotaRequest(name, namespace string, spec QuotaRequestSpec) QuotaRequest {
	if namespace == "" {
		namespace = SystemNamespace
	}
	if spec.MutationMode == "" {
		spec.MutationMode = "ReportOnly"
	}
	return QuotaRequest{
		APIVersion: APIVersion,
		Kind:       KindQuotaRequest,
		Metadata:   ObjectMeta{Name: name, Namespace: namespace},
		Spec:       spec,
	}
}

func dash(v string) string {
	if strings.TrimSpace(v) == "" {
		return "-"
	}
	return v
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
