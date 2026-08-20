// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	profile "github.com/Azure/taugrid/core/resourceprofile"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	SystemNamespace       = "tau-system"
	LegacySystemNamespace = "tau-platform"

	KindTauCluster      = "TauCluster"
	KindTauWorkspace    = "TauWorkspace"
	KindTauQuotaRequest = "TauQuotaRequest"

	TauClusterSingletonName = "cluster"

	ClusterManagementModeObserve   = "Observe"
	ClusterManagementModeReconcile = "Reconcile"

	ClusterDeletionPolicyRetain        = "Retain"
	ClusterDeletionPolicyDeleteManaged = "DeleteManaged"

	ClusterOwnershipExternal = "External"
	ClusterOwnershipAdopt    = "Adopt"
	ClusterOwnershipManage   = "Manage"

	TauClusterFeatureDisabled TauClusterFeatureStage = "Disabled"
	TauClusterFeatureBeta     TauClusterFeatureStage = "Beta"

	ClusterPhasePending  = "Pending"
	ClusterPhaseReady    = "Ready"
	ClusterPhaseDegraded = "Degraded"

	ConditionReady                 = "Ready"
	ConditionNodesReady            = "NodesReady"
	ConditionQueuesReady           = "QueuesReady"
	ConditionWorkspacesReady       = "WorkspacesReady"
	ConditionWorkloadProfilesReady = "WorkloadProfilesReady"
	ConditionMultiKueueReady       = "MultiKueueReady"
	ConditionObserveOnly           = "ObserveOnly"
	ConditionOwnershipConflict     = "OwnershipConflict"
	ConditionReconcilePaused       = "ReconcilePaused"
	ConditionDeletionBlocked       = "DeletionBlocked"

	PrincipalProviderEntra  = "entra"
	PrincipalProviderGitHub = "github"

	AuthorizationModeWorkspaceRBAC = "workspace-rbac"
	AuthorizationModeClusterWide   = "cluster-wide"

	WorkspacePhasePending  = "Pending"
	WorkspacePhaseReady    = "Ready"
	WorkspacePhaseDegraded = "Degraded"

	ConditionRBACReady             = "RBACReady"
	ConditionQueueReady            = "QueueReady"
	ConditionWorkloadIdentityReady = "WorkloadIdentityReady"
	ConditionDriftDetected         = "DriftDetected"

	// MinWorkspaceTTLSecondsAfterFinished is the smallest retention a
	// workspace may default to. It is a durability floor, not a tuning knob:
	// finished-Job retention races the lifecycle recorder, which observes on
	// an interval (30s by default) and then ingests. Below roughly an order of
	// magnitude above that window, a Job that finishes just after a poll is
	// collected with its pods before the next pass, and the terminal lifecycle
	// row plus the failure evidence are lost rather than merely shortened.
	//
	// Keep this in sync with the kubebuilder Minimum marker on
	// WorkspaceDefaults.TTLSecondsAfterFinished.
	MinWorkspaceTTLSecondsAfterFinished int64 = 600

	QuotaRequestPhasePendingApproval = "PendingApproval"
	QuotaRequestPhaseApproved        = "Approved"
	QuotaRequestPhaseRejected        = "Rejected"
	QuotaRequestPhaseExpired         = "Expired"

	QuotaMutationModeReportOnly = "ReportOnly"
)

type TauClusterFeatureStage string

type TauClusterFeaturesSpec struct {
	// +kubebuilder:default=Disabled
	// +kubebuilder:validation:Enum=Disabled;Beta
	MultiKueue TauClusterFeatureStage `json:"multiKueue,omitempty"`
}

type TauClusterObjectReference struct {
	// Name is the name of a cluster-scoped object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type TauNamespacedObjectReference struct {
	// Namespace is the namespace containing the referenced object.
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// Name is the name of the referenced object.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type TauNodeMatch struct {
	// VMSizes limits a rule to nodes whose AKS VM-size label matches one of
	// these values. An empty list matches every node selected by Nodes.Selector.
	// +listType=set
	VMSizes []string `json:"vmSizes,omitempty"`
}

type TauNodeLabelRule struct {
	Match TauNodeMatch `json:"match,omitempty"`
	// Labels is the exact set of node label keys and values this rule desires.
	// The controller never owns or removes keys not declared here.
	// +kubebuilder:validation:MinProperties=1
	Labels map[string]string `json:"labels"`
}

type TauClusterNodesSpec struct {
	// Selector limits node discovery to labels that match every key/value pair.
	Selector map[string]string `json:"selector,omitempty"`
	// +listType=atomic
	LabelRules []TauNodeLabelRule `json:"labelRules,omitempty"`
}

type TauClusterQueuesSpec struct {
	// Ownership controls whether queue objects are observed, explicitly
	// adopted, or managed. External is the safe default for GitOps objects.
	// +kubebuilder:default=External
	// +kubebuilder:validation:Enum=External;Adopt;Manage
	Ownership string                     `json:"ownership,omitempty"`
	Topology  *TauClusterObjectReference `json:"topology,omitempty"`
	// +listType=map
	// +listMapKey=name
	ResourceFlavors []TauClusterObjectReference `json:"resourceFlavors,omitempty"`
	// +listType=map
	// +listMapKey=name
	ClusterQueues []TauClusterObjectReference `json:"clusterQueues,omitempty"`
	// +listType=map
	// +listMapKey=namespace
	// +listMapKey=name
	SharedLocalQueues []TauNamespacedObjectReference `json:"sharedLocalQueues,omitempty"`
}

type TauClusterWorkspaceDefaults struct {
	// +kubebuilder:default=jobqueue
	DefaultQueue string `json:"defaultQueue,omitempty"`
}

type TauClusterSpec struct {
	// ManagementMode controls whether TauCluster only reports desired-state
	// differences or is allowed to reconcile explicitly owned resources.
	// +kubebuilder:default=Observe
	// +kubebuilder:validation:Enum=Observe;Reconcile
	ManagementMode string `json:"managementMode,omitempty"`
	// DeletionPolicy defaults to retaining every managed cluster resource.
	// +kubebuilder:default=Retain
	// +kubebuilder:validation:Enum=Retain;DeleteManaged
	DeletionPolicy string              `json:"deletionPolicy,omitempty"`
	Nodes          TauClusterNodesSpec `json:"nodes,omitempty"`
	// +kubebuilder:default={}
	Queues TauClusterQueuesSpec `json:"queues,omitempty"`
	// +kubebuilder:default={}
	WorkspaceDefaults TauClusterWorkspaceDefaults `json:"workspaceDefaults,omitempty"`
	// +kubebuilder:default={}
	Features TauClusterFeaturesSpec `json:"features,omitempty"`
	// WorkloadProfiles declares stable workload intent. Live quota, capacity,
	// flavor selectors, and topology selectors are resolved into status instead.
	// +listType=map
	// +listMapKey=name
	WorkloadProfiles []profile.WorkloadProfile `json:"workloadProfiles,omitempty"`
}

type TauClusterSectionStatus struct {
	Observed int32 `json:"observed,omitempty"`
	Ready    int32 `json:"ready,omitempty"`
	Drifted  int32 `json:"drifted,omitempty"`
}

type TauManagedResourceStatus struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
	// +kubebuilder:validation:Enum=External;Adopt;Manage
	Ownership string `json:"ownership"`
}

type TauClusterStatus struct {
	// +kubebuilder:validation:Enum=Pending;Ready;Degraded
	Phase              string                   `json:"phase,omitempty"`
	ObservedGeneration int64                    `json:"observedGeneration,omitempty"`
	DesiredStateHash   string                   `json:"desiredStateHash,omitempty"`
	Nodes              TauClusterSectionStatus  `json:"nodes,omitempty"`
	Queues             TauClusterSectionStatus  `json:"queues,omitempty"`
	WorkloadProfiles   profile.ProfileSetStatus `json:"workloadProfiles,omitempty"`
	// +listType=atomic
	ManagedResources []TauManagedResourceStatus `json:"managedResources,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=clusters,singular=cluster,scope=Cluster,shortName=tc
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.managementMode`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TauCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	// +kubebuilder:default={}
	Spec   TauClusterSpec   `json:"spec,omitempty"`
	Status TauClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TauClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TauCluster `json:"items"`
}

type PrincipalRef struct {
	// Provider is the external identity source. Initial supported values are
	// "entra" and "github".
	// +kubebuilder:validation:Enum=entra;github
	Provider string `json:"provider"`
	// Name is the external group or team reference in the selected provider.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type WorkspaceAuthorization struct {
	// Mode controls whether the controller grants namespace-scoped access or
	// relies on authorization the caller already has on the cluster.
	// +kubebuilder:default=workspace-rbac
	// +kubebuilder:validation:Enum=workspace-rbac;cluster-wide
	Mode string `json:"mode,omitempty"`
}

type KubernetesSubject struct {
	// Kind is the Kubernetes RBAC subject kind, usually Group.
	// +kubebuilder:validation:Enum=Group;User;ServiceAccount
	Kind string `json:"kind"`
	// Name is the subject value seen by Kubernetes authn/authz.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type WorkspaceTarget struct {
	// Namespace is the workload namespace. If empty, controllers default it to
	// the workspace object name.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace,omitempty"`
	// CreateNamespace allows the controller to create the workload namespace.
	CreateNamespace bool `json:"createNamespace,omitempty"`
}

type WorkspaceDefaults struct {
	// +kubebuilder:validation:MinLength=1
	OutputRoot string `json:"outputRoot,omitempty"`
	// Priority is the workspace-level default priority tier. The controller
	// does not read it, but the Tau CLI does: applyWorkspaceDefaults reads
	// spec.defaults.priority off the fetched TauWorkspace and uses it as the
	// fallback priority tier for every run submitted against the workspace.
	// Removing it from this schema makes the API server prune the field, which
	// silently downgrades "priority" workspaces to normal scheduling. Keep it.
	// +kubebuilder:validation:Enum=default;priority;normal
	Priority string `json:"priority,omitempty"`
	// TTLSecondsAfterFinished is the workspace-level default retention for
	// finished batch Jobs, in seconds. Like Priority, the controller does not
	// read it: the Tau CLI does, via applyWorkspaceDefaults, and applies it to
	// runs submitted against the workspace.
	//
	// It is an override, not a floor. When unset, tau keeps its built-in
	// retention; when set, it wins over the built-in but still loses to an
	// explicit run.ttl_seconds_after_finished in a run config.
	//
	// Scope is batch Jobs only, matching run.ttl_seconds_after_finished, which
	// core/runconfig rejects for the ray engine. RayJob cleanup is governed by
	// the Ray cluster's own shutdown behaviour and is not affected by this
	// field.
	//
	// The minimum is deliberately well above a single observation interval.
	// Retention races the lifecycle recorder, which polls (30s by default) and
	// then ingests: a Job finishing just after a poll with a very short TTL is
	// deleted along with its pods before the next pass, so the terminal row and
	// the failure evidence are lost permanently rather than merely early. Values
	// below MinWorkspaceTTLSecondsAfterFinished buy nothing — Kubernetes garbage
	// collection is not the constraint on cluster capacity — and silently defeat
	// the durable record they would otherwise be paired with.
	//
	// A pointer distinguishes "unset" from a zero the API server would
	// otherwise elide, which matters because zero is a legal Kubernetes TTL
	// meaning "delete immediately" and must never be inferred from absence.
	// +kubebuilder:validation:Minimum=600
	// +kubebuilder:validation:Maximum=2147483647
	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`
}

type WorkspaceWorkloadIdentity struct {
	// ServiceAccountName is the workload service account the controller creates
	// or validates in the target namespace.
	// +kubebuilder:validation:MinLength=1
	ServiceAccountName string `json:"serviceAccountName"`
	// ClientID is the Azure managed identity client ID used by Azure Workload
	// Identity. The controller does not create or grant the identity.
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientId"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.authorization) && self.authorization.mode == 'cluster-wide') ? (!has(self.principalRef) && !has(self.kubernetesSubject) && !has(self.role)) : (has(self.principalRef) && has(self.kubernetesSubject) && has(self.role))",message="cluster-wide authorization must omit principalRef, kubernetesSubject, and role; workspace-rbac requires them"
type TauWorkspaceSpec struct {
	Authorization     *WorkspaceAuthorization `json:"authorization,omitempty"`
	PrincipalRef      *PrincipalRef           `json:"principalRef,omitempty"`
	KubernetesSubject *KubernetesSubject      `json:"kubernetesSubject,omitempty"`
	// Role is the researcher authorization role bound in the target namespace.
	// The controller implements exactly one role, so the API accepts only that
	// value rather than silently degrading unknown ones at reconcile time.
	// +kubebuilder:validation:Enum=tau-researcher-v1
	Role   string          `json:"role,omitempty"`
	Target WorkspaceTarget `json:"target,omitempty"`
	// Queue is the workspace LocalQueue name. It is optional: when omitted the
	// controller resolves TauCluster.spec.workspaceDefaults.defaultQueue, which
	// is the name the TauGrid distribution gives its baseline ClusterQueue.
	// The effective value is always reported in status.queue.localQueue.
	// +kubebuilder:validation:MinLength=1
	Queue            string                     `json:"queue,omitempty"`
	Defaults         WorkspaceDefaults          `json:"defaults,omitempty"`
	WorkloadIdentity *WorkspaceWorkloadIdentity `json:"workloadIdentity,omitempty"`
}

type WorkspaceTargetStatus struct {
	ResolvedNamespace string `json:"resolvedNamespace,omitempty"`
}

type WorkspaceQueueStatus struct {
	LocalQueue   string `json:"localQueue,omitempty"`
	ClusterQueue string `json:"clusterQueue,omitempty"`
}

type TauWorkspaceStatus struct {
	// +kubebuilder:validation:Enum=Pending;Ready;Degraded
	Phase              string                `json:"phase,omitempty"`
	ObservedGeneration int64                 `json:"observedGeneration,omitempty"`
	Target             WorkspaceTargetStatus `json:"target,omitempty"`
	Queue              WorkspaceQueueStatus  `json:"queue,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=workspaces,singular=workspace,scope=Namespaced,shortName=tw
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.status.target.resolvedNamespace`
// +kubebuilder:printcolumn:name="Queue",type=string,JSONPath=`.spec.queue`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TauWorkspace struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TauWorkspaceSpec   `json:"spec,omitempty"`
	Status TauWorkspaceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TauWorkspaceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TauWorkspace `json:"items"`
}

type TauQuotaRequestSpec struct {
	// +kubebuilder:validation:MinLength=1
	Workspace string `json:"workspace"`
	// +kubebuilder:validation:MinLength=1
	Resource string `json:"resource"`
	// +kubebuilder:validation:Minimum=1
	Requested int64 `json:"requested"`
	// +kubebuilder:validation:MinLength=1
	Reason string `json:"reason"`
	// +kubebuilder:validation:Enum=ReportOnly
	MutationMode string `json:"mutationMode,omitempty"`
}

type TauQuotaRequestStatus struct {
	// +kubebuilder:validation:Enum=PendingApproval;Approved;Rejected;Expired
	Phase              string `json:"phase,omitempty"`
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Decision           string `json:"decision,omitempty"`
	ApprovedBy         string `json:"approvedBy,omitempty"`
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=quotarequests,singular=quotarequest,scope=Namespaced,shortName=tqr
// +kubebuilder:printcolumn:name="Workspace",type=string,JSONPath=`.spec.workspace`
// +kubebuilder:printcolumn:name="Resource",type=string,JSONPath=`.spec.resource`
// +kubebuilder:printcolumn:name="Requested",type=integer,JSONPath=`.spec.requested`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TauQuotaRequest struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TauQuotaRequestSpec   `json:"spec,omitempty"`
	Status TauQuotaRequestStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TauQuotaRequestList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TauQuotaRequest `json:"items"`
}

func addKnownTypes(scheme *runtime.Scheme) error {
	scheme.AddKnownTypes(GroupVersion,
		&TauCluster{},
		&TauClusterList{},
		&TauWorkspace{},
		&TauWorkspaceList{},
		&TauQuotaRequest{},
		&TauQuotaRequestList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
