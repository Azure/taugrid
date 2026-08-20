// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package v1alpha1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

func TestTauWorkspaceRoundTrip(t *testing.T) {
	ws := TauWorkspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: KindTauWorkspace},
		ObjectMeta: metav1.ObjectMeta{Name: "aurora", Namespace: SystemNamespace},
		Spec: TauWorkspaceSpec{
			PrincipalRef:      &PrincipalRef{Provider: PrincipalProviderEntra, Name: "aurora-researchers"},
			KubernetesSubject: &KubernetesSubject{Kind: "Group", Name: "aurora-researchers"},
			Role:              "tau-researcher-v1",
			Target:            WorkspaceTarget{Namespace: "aurora", CreateNamespace: true},
			Queue:             "aurora",
			Defaults:          WorkspaceDefaults{OutputRoot: "/data/projects/aurora/runs", Priority: "normal"},
		},
		Status: TauWorkspaceStatus{
			Phase:              WorkspacePhaseReady,
			ObservedGeneration: 1,
			Target:             WorkspaceTargetStatus{ResolvedNamespace: "aurora"},
			Conditions: []metav1.Condition{{
				Type:               ConditionRBACReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "Allowed",
				Message:            "researcher role binding exists",
			}},
		},
	}

	data, err := json.Marshal(ws)
	if err != nil {
		t.Fatalf("Marshal TauWorkspace: %v", err)
	}
	var got TauWorkspace
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal TauWorkspace: %v", err)
	}
	if got.APIVersion != GroupVersion.String() || got.Kind != KindTauWorkspace {
		t.Fatalf("GVK = %s/%s, want %s/%s", got.APIVersion, got.Kind, GroupVersion.String(), KindTauWorkspace)
	}
	if got.Namespace != SystemNamespace {
		t.Fatalf("namespace = %q, want %q", got.Namespace, SystemNamespace)
	}
}

func TestTauClusterRoundTrip(t *testing.T) {
	cluster := TauCluster{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: KindTauCluster},
		ObjectMeta: metav1.ObjectMeta{Name: TauClusterSingletonName},
		Spec: TauClusterSpec{
			ManagementMode: ClusterManagementModeObserve,
			DeletionPolicy: ClusterDeletionPolicyRetain,
			Features:       TauClusterFeaturesSpec{MultiKueue: TauClusterFeatureBeta},
			Nodes: TauClusterNodesSpec{
				Selector: map[string]string{"kubernetes.azure.com/agentpool": "gpu"},
				LabelRules: []TauNodeLabelRule{{
					Match:  TauNodeMatch{VMSizes: []string{"Standard_ND96isr_H200_v5"}},
					Labels: map[string]string{"example.com/gpu-class": "h200"},
				}},
			},
			Queues: TauClusterQueuesSpec{
				Ownership:         ClusterOwnershipExternal,
				Topology:          &TauClusterObjectReference{Name: "default-node-topology"},
				ResourceFlavors:   []TauClusterObjectReference{{Name: "h200"}},
				ClusterQueues:     []TauClusterObjectReference{{Name: "tau-cq"}},
				SharedLocalQueues: []TauNamespacedObjectReference{{Namespace: "ray", Name: "jobqueue"}},
			},
			WorkloadProfiles: []profile.WorkloadProfile{{
				Name:              "research-1gpu",
				GPUsPerWorker:     1,
				WorkerCount:       1,
				Mode:              profile.ModeFixed,
				Placement:         profile.PlacementIndependent,
				DefaultLocalQueue: "jobqueue",
				ExecutionTarget:   profile.ExecutionTargetMultiKueueBeta,
				Applicability: profile.ProfileApplicability{
					Teams: []string{"research"}, Namespaces: []string{"ray"},
				},
				Priorities: profile.ProfilePriorities{
					WorkloadPriorityClassName: "tau-default",
					PodPriorityClassName:      "tau-default",
				},
			}},
		},
		Status: TauClusterStatus{
			Phase:              ClusterPhasePending,
			ObservedGeneration: 1,
			DesiredStateHash:   "abc123",
			WorkloadProfiles: profile.ProfileSetStatus{
				ObservedGeneration: 1,
				Observed:           1,
				Ready:              1,
				ProfileSetHash:     "profile123",
				Profiles: []profile.ResolvedWorkloadProfile{{
					WorkloadProfile: profile.WorkloadProfile{
						Name:              "research-1gpu",
						GPUsPerWorker:     1,
						WorkerCount:       1,
						Mode:              profile.ModeFixed,
						Placement:         profile.PlacementIndependent,
						DefaultLocalQueue: "jobqueue",
						Priorities:        profile.ProfilePriorities{DisableDefaultPriorities: true},
					},
					LocalQueues: []profile.ResolvedLocalQueue{{
						Namespace: "ray", Name: "jobqueue", ClusterQueue: "tau-cq",
					}},
					ResourceFlavors: []string{"h100"},
				}},
			},
			Conditions: []metav1.Condition{{
				Type:               ConditionObserveOnly,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
				Reason:             "ObserveMode",
				Message:            "no cluster resources are mutated",
			}},
		},
	}

	data, err := json.Marshal(cluster)
	if err != nil {
		t.Fatalf("Marshal TauCluster: %v", err)
	}
	var got TauCluster
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("Unmarshal TauCluster: %v", err)
	}
	if got.Name != TauClusterSingletonName || got.Spec.ManagementMode != ClusterManagementModeObserve {
		t.Fatalf("TauCluster identity/mode = %q/%q", got.Name, got.Spec.ManagementMode)
	}
	if got.Spec.Features.MultiKueue != TauClusterFeatureBeta ||
		got.Spec.WorkloadProfiles[0].ExecutionTarget != profile.ExecutionTargetMultiKueueBeta {
		t.Fatalf("TauCluster feature/profile execution contract = %#v", got.Spec)
	}
	if len(got.Spec.Queues.SharedLocalQueues) != 1 || got.Spec.Queues.SharedLocalQueues[0].Namespace != "ray" {
		t.Fatalf("TauCluster queue references = %#v", got.Spec.Queues.SharedLocalQueues)
	}
	if got.Status.DesiredStateHash != "abc123" {
		t.Fatalf("TauCluster desired state hash = %q", got.Status.DesiredStateHash)
	}
	if len(got.Spec.WorkloadProfiles) != 1 || got.Spec.WorkloadProfiles[0].Name != "research-1gpu" {
		t.Fatalf("TauCluster workload profile spec = %#v", got.Spec.WorkloadProfiles)
	}
	if got.Status.WorkloadProfiles.ProfileSetHash != "profile123" ||
		len(got.Status.WorkloadProfiles.Profiles) != 1 ||
		got.Status.WorkloadProfiles.Profiles[0].LocalQueues[0].ClusterQueue != "tau-cq" {
		t.Fatalf("TauCluster workload profile status = %#v", got.Status.WorkloadProfiles)
	}
}

func TestClusterWideWorkspaceJSONOmitsSubjectIdentity(t *testing.T) {
	ws := TauWorkspace{
		Spec: TauWorkspaceSpec{
			Authorization: &WorkspaceAuthorization{Mode: AuthorizationModeClusterWide},
			Queue:         "jobqueue",
		},
	}
	data, err := json.Marshal(ws)
	if err != nil {
		t.Fatalf("Marshal cluster-wide TauWorkspace: %v", err)
	}
	for _, field := range []string{"principalRef", "kubernetesSubject", `"role"`} {
		if strings.Contains(string(data), field) {
			t.Fatalf("cluster-wide JSON unexpectedly contains %q: %s", field, data)
		}
	}
}

func TestCRDManifestsPinWorkspaceContract(t *testing.T) {
	ws := readCRD(t, "tau.azure.com_workspaces.yaml")
	assertCRD(t, ws, "workspaces.tau.azure.com", "TauWorkspace", apiextensionsv1.NamespaceScoped)

	version := ws.Spec.Versions[0]
	props := version.Schema.OpenAPIV3Schema.Properties["spec"].Properties
	for _, field := range []string{"authorization", "principalRef", "kubernetesSubject", "target", "queue", "defaults"} {
		if _, ok := props[field]; !ok {
			t.Fatalf("TauWorkspace spec schema missing %q", field)
		}
	}
	if _, ok := props["storage"]; ok {
		t.Fatal("TauWorkspace spec schema still carries storage: workspace storage is deferred beyond v0")
	}
	authMode := props["authorization"].Properties["mode"]
	if len(authMode.Enum) != 2 || authMode.Default == nil {
		t.Fatalf("authorization.mode schema = %#v, want two modes and workspace-rbac default", authMode)
	}
	statusProps := version.Schema.OpenAPIV3Schema.Properties["status"].Properties
	if _, ok := statusProps["quota"]; ok {
		t.Fatalf("TauWorkspace status schema still carries quota: Kueue counters cannot be kept fresh without a LocalQueue watch")
	}
	if _, ok := statusProps["conditions"]; !ok {
		t.Fatalf("TauWorkspace status schema missing conditions")
	}

	qr := readCRD(t, "tau.azure.com_quotarequests.yaml")
	assertCRD(t, qr, "quotarequests.tau.azure.com", "TauQuotaRequest", apiextensionsv1.NamespaceScoped)
	qrProps := qr.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties
	mode := qrProps["mutationMode"]
	if len(mode.Enum) != 1 {
		t.Fatalf("mutationMode enum len = %d, want 1", len(mode.Enum))
	}

}

func TestTauClusterCRDContract(t *testing.T) {
	cluster := readCRD(t, "tau.azure.com_clusters.yaml")
	assertCRD(t, cluster, "clusters.tau.azure.com", KindTauCluster, apiextensionsv1.ClusterScoped)

	version := cluster.Spec.Versions[0]
	specProps := version.Schema.OpenAPIV3Schema.Properties["spec"].Properties
	for _, field := range []string{"managementMode", "deletionPolicy", "nodes", "queues", "workspaceDefaults", "features", "workloadProfiles"} {
		if _, ok := specProps[field]; !ok {
			t.Fatalf("TauCluster spec schema missing %q", field)
		}
	}
	if _, ok := specProps["storage"]; ok {
		t.Fatal("TauCluster spec must not expose storage ownership in v0.1")
	}
	if got := string(specProps["managementMode"].Default.Raw); got != `"Observe"` {
		t.Fatalf("managementMode default = %s, want Observe", got)
	}
	if got := string(specProps["deletionPolicy"].Default.Raw); got != `"Retain"` {
		t.Fatalf("deletionPolicy default = %s, want Retain", got)
	}
	ownership := specProps["queues"].Properties["ownership"]
	if got := string(ownership.Default.Raw); got != `"External"` {
		t.Fatalf("queues.ownership default = %s, want External", got)
	}
	workspaceDefaults := specProps["workspaceDefaults"].Properties
	for _, field := range []string{"defaultStorageClass", "defaultStoragePVC"} {
		if _, ok := workspaceDefaults[field]; ok {
			t.Fatalf("TauCluster workspaceDefaults must not expose %q in v0.1", field)
		}
	}
	if got := string(workspaceDefaults["defaultQueue"].Default.Raw); got != `"jobqueue"` {
		t.Fatalf("workspaceDefaults.defaultQueue default = %s, want jobqueue", got)
	}
	multiKueue := specProps["features"].Properties["multiKueue"]
	if got := string(multiKueue.Default.Raw); got != `"Disabled"` {
		t.Fatalf("features.multiKueue default = %s, want Disabled", got)
	}
	if len(multiKueue.Enum) != 2 {
		t.Fatalf("features.multiKueue enum = %v, want Disabled and Beta", multiKueue.Enum)
	}

	profiles := specProps["workloadProfiles"]
	if profiles.XListType == nil || *profiles.XListType != "map" ||
		len(profiles.XListMapKeys) != 1 || profiles.XListMapKeys[0] != "name" {
		t.Fatalf("spec.workloadProfiles list semantics = type %v keys %v, want map/name", profiles.XListType, profiles.XListMapKeys)
	}
	profileProps := profiles.Items.Schema.Properties
	for _, field := range []string{"name", "description", "applicability", "gpusPerWorker", "workerCount", "mode", "placement", "defaultLocalQueue", "executionTarget", "priorities"} {
		if _, ok := profileProps[field]; !ok {
			t.Fatalf("TauCluster workload profile schema missing %q", field)
		}
	}
	for _, field := range []string{"quota", "capacity", "resourceFlavor", "topologyName", "resourceSelector", "topologySelector"} {
		if _, ok := profileProps[field]; ok {
			t.Fatalf("TauCluster workload profile spec must not expose %q", field)
		}
	}
	if len(profileProps["mode"].Enum) != 2 || len(profileProps["placement"].Enum) != 3 {
		t.Fatalf("workload profile enums = mode %v placement %v", profileProps["mode"].Enum, profileProps["placement"].Enum)
	}
	if got := string(profileProps["executionTarget"].Default.Raw); got != `"singleCluster"` ||
		len(profileProps["executionTarget"].Enum) != 2 {
		t.Fatalf("workload profile executionTarget schema = %#v", profileProps["executionTarget"])
	}

	statusProps := version.Schema.OpenAPIV3Schema.Properties["status"].Properties
	statusProfiles := statusProps["workloadProfiles"].Properties
	for _, field := range []string{"observedGeneration", "observed", "ready", "drifted", "profileSetHash", "profiles"} {
		if _, ok := statusProfiles[field]; !ok {
			t.Fatalf("TauCluster workload profile status schema missing %q", field)
		}
	}
	resolved := statusProfiles["profiles"]
	if resolved.XListType == nil || *resolved.XListType != "map" ||
		len(resolved.XListMapKeys) != 1 || resolved.XListMapKeys[0] != "name" {
		t.Fatalf("status.workloadProfiles.profiles list semantics = type %v keys %v", resolved.XListType, resolved.XListMapKeys)
	}
	resolvedProps := resolved.Items.Schema.Properties
	for _, field := range []string{"name", "localQueues", "clusterQueues", "resourceFlavors", "topologies", "workloadPriorityClasses", "podPriorityClasses", "conditions"} {
		if _, ok := resolvedProps[field]; !ok {
			t.Fatalf("resolved workload profile schema missing %q", field)
		}
	}
}

func TestTauClusterDeepCopyCopiesSharedProfileSlices(t *testing.T) {
	cluster := TauCluster{
		Spec: TauClusterSpec{WorkloadProfiles: []profile.WorkloadProfile{{
			Name:          "research-1gpu",
			Applicability: profile.ProfileApplicability{Teams: []string{"research"}},
		}}},
		Status: TauClusterStatus{WorkloadProfiles: profile.ProfileSetStatus{
			Profiles: []profile.ResolvedWorkloadProfile{{
				WorkloadProfile: profile.WorkloadProfile{Name: "research-1gpu"},
				ResourceFlavors: []string{"h100"},
				Conditions:      []metav1.Condition{{Type: profile.ConditionReady, Message: "ready"}},
			}},
		}},
	}

	copy := cluster.DeepCopy()
	copy.Spec.WorkloadProfiles[0].Applicability.Teams[0] = "other"
	copy.Status.WorkloadProfiles.Profiles[0].ResourceFlavors[0] = "h200"
	copy.Status.WorkloadProfiles.Profiles[0].Conditions[0].Message = "changed"
	if cluster.Spec.WorkloadProfiles[0].Applicability.Teams[0] != "research" ||
		cluster.Status.WorkloadProfiles.Profiles[0].ResourceFlavors[0] != "h100" ||
		cluster.Status.WorkloadProfiles.Profiles[0].Conditions[0].Message != "ready" {
		t.Fatal("TauCluster.DeepCopy() shallow-copied imported workload profile data")
	}
}

func readCRD(t *testing.T, name string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	path := filepath.Join("..", "..", "config", "crd", "bases", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(data, &crd); err != nil {
		t.Fatalf("Unmarshal CRD %s: %v", name, err)
	}
	return crd
}

func assertCRD(t *testing.T, crd apiextensionsv1.CustomResourceDefinition, name, kind string, scope apiextensionsv1.ResourceScope) {
	t.Helper()
	if crd.Name != name {
		t.Fatalf("CRD name = %q, want %q", crd.Name, name)
	}
	if crd.Spec.Group != GroupName {
		t.Fatalf("CRD group = %q, want %q", crd.Spec.Group, GroupName)
	}
	if crd.Spec.Scope != scope {
		t.Fatalf("CRD scope = %q, want %q", crd.Spec.Scope, scope)
	}
	if crd.Spec.Names.Kind != kind {
		t.Fatalf("CRD kind = %q, want %q", crd.Spec.Names.Kind, kind)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != Version {
		t.Fatalf("CRD versions = %#v, want one %s version", crd.Spec.Versions, Version)
	}
	if crd.Spec.Versions[0].Subresources == nil || crd.Spec.Versions[0].Subresources.Status == nil {
		t.Fatalf("CRD %s missing status subresource", name)
	}
}
