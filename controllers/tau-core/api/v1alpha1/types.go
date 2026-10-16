// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

const (
	SystemNamespace       = "tau-system"
	LegacySystemNamespace = "tau-platform"

	KindTauCluster      = "TauCluster"
	KindTauTeam         = "TauTeam"
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

	TeamPhasePending  = "Pending"
	TeamPhaseReady    = "Ready"
	TeamPhaseDegraded = "Degraded"

	ConditionQuotaReady            = "QuotaReady"
	ConditionRBACReady             = "RBACReady"
	ConditionQueueReady            = "QueueReady"
	ConditionWorkloadIdentityReady = "WorkloadIdentityReady"
	ConditionDriftDetected         = "DriftDetected"

	MinWorkspaceTTLSecondsAfterFinished int64 = 600

	QuotaRequestPhasePendingApproval = "PendingApproval"
	QuotaRequestPhaseApproved        = "Approved"
	QuotaRequestPhaseRejected        = "Rejected"
	QuotaRequestPhaseExpired         = "Expired"

	QuotaMutationModeReportOnly = "ReportOnly"
)

type TauClusterObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type TauNamespacedObjectReference struct {
	// +kubebuilder:validation:MinLength=1
	Namespace string `json:"namespace"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type TauNodeMatch struct {
	VMSizes []string `json:"vmSizes,omitempty"`
}

type TauNodeLabelRule struct {
	Match TauNodeMatch `json:"match,omitempty"`
	// +kubebuilder:validation:MinProperties=1
	Labels map[string]string `json:"labels"`
}

type TauClusterNodesSpec struct {
	Selector   map[string]string  `json:"selector,omitempty"`
	LabelRules []TauNodeLabelRule `json:"labelRules,omitempty"`
}

type TauClusterQueuesSpec struct {
	// +kubebuilder:default=External
	// +kubebuilder:validation:Enum=External;Adopt;Manage
	Ownership         string                         `json:"ownership,omitempty"`
	Topology          *TauClusterObjectReference     `json:"topology,omitempty"`
	ResourceFlavors   []TauClusterObjectReference    `json:"resourceFlavors,omitempty"`
	ClusterQueues     []TauClusterObjectReference    `json:"clusterQueues,omitempty"`
	SharedLocalQueues []TauNamespacedObjectReference `json:"sharedLocalQueues,omitempty"`
}

type TauClusterWorkspaceDefaults struct {
	// +kubebuilder:default=jobqueue
	DefaultQueue string `json:"defaultQueue,omitempty"`
}

type TauResourceQuota struct {
	// +kubebuilder:validation:MinLength=1
	Flavor string `json:"flavor"`
	// +kubebuilder:validation:MinLength=1
	Resource string `json:"resource"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!string(self).startsWith('-')",message="nominalQuota must be non-negative"
	NominalQuota resource.Quantity `json:"nominalQuota"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!string(self).startsWith('-')",message="borrowingLimit must be non-negative"
	BorrowingLimit *resource.Quantity `json:"borrowingLimit,omitempty"`
	// +kubebuilder:validation:Type=string
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:XValidation:rule="!string(self).startsWith('-')",message="lendingLimit must be non-negative"
	LendingLimit *resource.Quantity `json:"lendingLimit,omitempty"`
}

type TauResourceCapacityStatus struct {
	Flavor   string            `json:"flavor"`
	Resource string            `json:"resource"`
	Capacity resource.Quantity `json:"capacity"`
}

type TauClusterSpec struct {
	// +kubebuilder:default=Observe
	// +kubebuilder:validation:Enum=Observe;Reconcile
	ManagementMode string `json:"managementMode,omitempty"`
	// +kubebuilder:default=Retain
	// +kubebuilder:validation:Enum=Retain;DeleteManaged
	DeletionPolicy string              `json:"deletionPolicy,omitempty"`
	Nodes          TauClusterNodesSpec `json:"nodes,omitempty"`
	// +kubebuilder:default={}
	Queues TauClusterQueuesSpec `json:"queues,omitempty"`
	// +kubebuilder:default={}
	WorkspaceDefaults TauClusterWorkspaceDefaults `json:"workspaceDefaults,omitempty"`
	WorkloadProfiles  []profile.WorkloadProfile   `json:"workloadProfiles,omitempty"`
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
	Phase              string                      `json:"phase,omitempty"`
	ObservedGeneration int64                       `json:"observedGeneration,omitempty"`
	DesiredStateHash   string                      `json:"desiredStateHash,omitempty"`
	Nodes              TauClusterSectionStatus     `json:"nodes,omitempty"`
	Queues             TauClusterSectionStatus     `json:"queues,omitempty"`
	WorkloadProfiles   profile.ProfileSetStatus    `json:"workloadProfiles,omitempty"`
	DiscoveredCapacity []TauResourceCapacityStatus `json:"discoveredCapacity,omitempty"`
	ManagedResources   []TauManagedResourceStatus  `json:"managedResources,omitempty"`
	Conditions         []metav1.Condition          `json:"conditions,omitempty"`
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
	// +kubebuilder:validation:Enum=entra;github
	Provider string `json:"provider"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type TauTeamSpec struct {
	// +kubebuilder:validation:MaxItems=256
	Quota []TauResourceQuota `json:"quota,omitempty"`
}

type TauTeamStatus struct {
	// +kubebuilder:validation:Enum=Pending;Ready;Degraded
	Phase              string             `json:"phase,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Cohort             string             `json:"cohort,omitempty"`
	CohortUID          string             `json:"cohortUID,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:path=teams,singular=team,scope=Namespaced,shortName=tt
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Cohort",type=string,JSONPath=`.status.cohort`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type TauTeam struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   TauTeamSpec   `json:"spec,omitempty"`
	Status TauTeamStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type TauTeamList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []TauTeam `json:"items"`
}

type WorkspaceAuthorization struct {
	// +kubebuilder:default=workspace-rbac
	// +kubebuilder:validation:Enum=workspace-rbac;cluster-wide
	Mode string `json:"mode,omitempty"`
}

type KubernetesSubject struct {
	// +kubebuilder:validation:Enum=Group;User;ServiceAccount
	Kind string `json:"kind"`
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

type WorkspaceTarget struct {
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace       string `json:"namespace,omitempty"`
	CreateNamespace bool   `json:"createNamespace,omitempty"`
}

type WorkspaceDefaults struct {
	// +kubebuilder:validation:MinLength=1
	OutputRoot string `json:"outputRoot,omitempty"`
	// +kubebuilder:validation:Enum=default;priority;normal
	Priority string `json:"priority,omitempty"`
	// +kubebuilder:validation:Minimum=600
	// +kubebuilder:validation:Maximum=2147483647
	TTLSecondsAfterFinished *int64 `json:"ttlSecondsAfterFinished,omitempty"`
}

type WorkspaceWorkloadIdentity struct {
	// +kubebuilder:validation:MinLength=1
	ServiceAccountName string `json:"serviceAccountName"`
	// +kubebuilder:validation:MinLength=1
	ClientID string `json:"clientId"`
}

// +kubebuilder:validation:XValidation:rule="(has(self.authorization) && self.authorization.mode == 'cluster-wide') ? (!has(self.principalRef) && !has(self.kubernetesSubject) && !has(self.role)) : (has(self.principalRef) && has(self.kubernetesSubject) && has(self.role))",message="cluster-wide authorization must omit principalRef, kubernetesSubject, and role; workspace-rbac requires them"
type TauWorkspaceSpec struct {
	TeamRef *TauClusterObjectReference `json:"teamRef,omitempty"`
	// +kubebuilder:validation:MaxItems=256
	Quota             []TauResourceQuota      `json:"quota,omitempty"`
	Authorization     *WorkspaceAuthorization `json:"authorization,omitempty"`
	PrincipalRef      *PrincipalRef           `json:"principalRef,omitempty"`
	KubernetesSubject *KubernetesSubject      `json:"kubernetesSubject,omitempty"`
	// +kubebuilder:validation:Enum=researcher;tau-researcher-v1
	Role   string          `json:"role,omitempty"`
	Target WorkspaceTarget `json:"target,omitempty"`
	// +kubebuilder:validation:MinLength=1
	Queue            string                     `json:"queue,omitempty"`
	Defaults         WorkspaceDefaults          `json:"defaults,omitempty"`
	WorkloadIdentity *WorkspaceWorkloadIdentity `json:"workloadIdentity,omitempty"`
}

type WorkspaceTargetStatus struct {
	ResolvedNamespace string `json:"resolvedNamespace,omitempty"`
}

type WorkspaceQueueStatus struct {
	LocalQueue      string `json:"localQueue,omitempty"`
	ClusterQueue    string `json:"clusterQueue,omitempty"`
	ClusterQueueUID string `json:"clusterQueueUID,omitempty"`
}

type TauWorkspaceStatus struct {
	// +kubebuilder:validation:Enum=Pending;Ready;Degraded
	Phase              string                `json:"phase,omitempty"`
	ObservedGeneration int64                 `json:"observedGeneration,omitempty"`
	Target             WorkspaceTargetStatus `json:"target,omitempty"`
	Queue              WorkspaceQueueStatus  `json:"queue,omitempty"`
	Conditions         []metav1.Condition    `json:"conditions,omitempty"`
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
	Phase              string             `json:"phase,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Decision           string             `json:"decision,omitempty"`
	ApprovedBy         string             `json:"approvedBy,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
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
		&TauTeam{},
		&TauTeamList{},
		&TauWorkspace{},
		&TauWorkspaceList{},
		&TauQuotaRequest{},
		&TauQuotaRequestList{},
	)
	metav1.AddToGroupVersion(scheme, GroupVersion)
	return nil
}
