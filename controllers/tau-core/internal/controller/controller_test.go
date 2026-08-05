package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestTauClusterObserveModeUpdatesStatusWithoutMutatingResources(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:       tauv1alpha1.TauClusterSingletonName,
			Generation: 3,
		},
		Spec: tauv1alpha1.TauClusterSpec{
			ManagementMode: tauv1alpha1.ClusterManagementModeObserve,
			Nodes: tauv1alpha1.TauClusterNodesSpec{
				LabelRules: []tauv1alpha1.TauNodeLabelRule{{
					Match: tauv1alpha1.TauNodeMatch{VMSizes: []string{"Standard_ND96isr_H200_v5"}},
					Labels: map[string]string{
						"kueue.azure.com/gpu-series": "nd-h200-v5",
					},
				}},
			},
			Queues: tauv1alpha1.TauClusterQueuesSpec{
				Ownership:     tauv1alpha1.ClusterOwnershipExternal,
				ClusterQueues: []tauv1alpha1.TauClusterObjectReference{{Name: "tau-cq"}},
			},
		},
	}
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "observed-h200",
			Labels: map[string]string{
				azureVMSizeLabel: "Standard_ND96isr_H200_v5",
			},
		},
	}

	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, node).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recordingClient := &resourceMutationRecordingClient{Client: baseClient}
	reconciler := &TauClusterReconciler{Client: recordingClient}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: tauv1alpha1.TauClusterSingletonName}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(recordingClient.mutations) != 0 {
		t.Fatalf("Observe mode mutated cluster resources: %v", recordingClient.mutations)
	}

	var got tauv1alpha1.TauCluster
	if err := baseClient.Get(ctx, client.ObjectKey{Name: tauv1alpha1.TauClusterSingletonName}, &got); err != nil {
		t.Fatalf("Get TauCluster: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.ClusterPhasePending ||
		got.Status.ObservedGeneration != cluster.Generation ||
		got.Status.DesiredStateHash == "" {
		t.Fatalf("TauCluster status = %#v", got.Status)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionObserveOnly, metav1.ConditionTrue)
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionReconcilePaused, metav1.ConditionFalse)
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionReady, metav1.ConditionFalse)
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionNodesReady, metav1.ConditionFalse)
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue)
	if got.Status.Nodes != (tauv1alpha1.TauClusterSectionStatus{Observed: 1, Drifted: 1}) {
		t.Fatalf("node status = %#v", got.Status.Nodes)
	}
	if len(got.Status.ManagedResources) != 0 {
		t.Fatalf("managed resources = %#v, want empty", got.Status.ManagedResources)
	}
	var unchanged corev1.Node
	if err := baseClient.Get(ctx, client.ObjectKey{Name: node.Name}, &unchanged); err != nil {
		t.Fatalf("Get observed Node: %v", err)
	}
	if _, ok := unchanged.Labels["kueue.azure.com/gpu-series"]; ok {
		t.Fatal("Observe mode changed the node topology label")
	}

	before := got.Status
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: tauv1alpha1.TauClusterSingletonName}, &got); err != nil {
		t.Fatalf("Get TauCluster after no-op: %v", err)
	}
	if !reflect.DeepEqual(before, got.Status) {
		t.Fatalf("status changed on no-op reconcile:\nbefore=%#v\nafter=%#v", before, got.Status)
	}
	if len(recordingClient.mutations) != 0 {
		t.Fatalf("second Observe reconcile mutated cluster resources: %v", recordingClient.mutations)
	}
}

func TestTauClusterReconcileModeLabelsNativeAndFlexNodes(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			ManagementMode: tauv1alpha1.ClusterManagementModeReconcile,
			Nodes: tauv1alpha1.TauClusterNodesSpec{
				LabelRules: []tauv1alpha1.TauNodeLabelRule{
					{
						Match: tauv1alpha1.TauNodeMatch{VMSizes: []string{"Standard_ND96amsr_A100_v4"}},
						Labels: map[string]string{
							"kueue.azure.com/gpu-series": "ndm-a100-v4",
						},
					},
					{
						Match: tauv1alpha1.TauNodeMatch{VMSizes: []string{"Standard_ND96isr_H200_v5"}},
						Labels: map[string]string{
							"kueue.azure.com/gpu-series": "nd-h200-v5",
						},
					},
				},
			},
		},
	}
	nativeNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "native-a100",
		Labels: map[string]string{
			azureVMSizeLabel:                       "Standard_ND96amsr_A100_v4",
			"kubernetes.azure.com/agentpool":       "gpu",
			"kubernetes.azure.com/mode":            "user",
			"kubernetes.azure.com/managed-cluster": "test",
		},
	}}
	flexNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "flex-h200",
		Labels: map[string]string{
			azureVMSizeLabel:                   "Standard_ND96isr_H200_v5",
			"kueue.azure.com/gpu-series":       "stale-value",
			"example.com/external-flex-marker": "true",
		},
	}}
	cpuNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name: "system-cpu",
		Labels: map[string]string{
			azureVMSizeLabel: "Standard_D8ds_v6",
		},
	}}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, nativeNode, flexNode, cpuNode).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recordingClient := &resourceMutationRecordingClient{Client: baseClient}
	reconciler := &TauClusterReconciler{Client: recordingClient}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, cluster); err != nil {
		t.Fatalf("Get TauCluster: %v", err)
	}
	paused := findCondition(cluster.Status.Conditions, tauv1alpha1.ConditionReconcilePaused)
	if paused == nil || paused.Status != metav1.ConditionFalse || paused.Reason != "NodeReconciliationActive" {
		t.Fatalf("ReconcilePaused = %#v", paused)
	}
	assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionNodesReady, metav1.ConditionTrue)
	assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionDriftDetected, metav1.ConditionFalse)
	assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionReady, metav1.ConditionTrue)
	if cluster.Status.Phase != tauv1alpha1.ClusterPhaseReady {
		t.Fatalf("phase = %q, want %q", cluster.Status.Phase, tauv1alpha1.ClusterPhaseReady)
	}
	if cluster.Status.Nodes != (tauv1alpha1.TauClusterSectionStatus{Observed: 2, Ready: 2}) {
		t.Fatalf("node status = %#v", cluster.Status.Nodes)
	}
	if got, want := recordingClient.mutations, []string{"patch flex-h200", "patch native-a100"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("mutations = %v, want %v", got, want)
	}

	for name, want := range map[string]string{
		nativeNode.Name: "ndm-a100-v4",
		flexNode.Name:   "nd-h200-v5",
	} {
		var node corev1.Node
		if err := baseClient.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
			t.Fatalf("Get Node %q: %v", name, err)
		}
		if got := node.Labels["kueue.azure.com/gpu-series"]; got != want {
			t.Fatalf("Node %q gpu-series = %q, want %q", name, got, want)
		}
	}
	var unchangedCPU corev1.Node
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cpuNode.Name}, &unchangedCPU); err != nil {
		t.Fatalf("Get CPU Node: %v", err)
	}
	if _, ok := unchangedCPU.Labels["kueue.azure.com/gpu-series"]; ok {
		t.Fatal("unmatched CPU Node received a GPU-series label")
	}

	recordingClient.mutations = nil
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(recordingClient.mutations) != 0 {
		t.Fatalf("no-op Reconcile() mutations = %v", recordingClient.mutations)
	}
}

func TestTauClusterReconcileRejectsConflictingNodeLabelRules(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			ManagementMode: tauv1alpha1.ClusterManagementModeReconcile,
			Nodes: tauv1alpha1.TauClusterNodesSpec{LabelRules: []tauv1alpha1.TauNodeLabelRule{
				{
					Match:  tauv1alpha1.TauNodeMatch{VMSizes: []string{"Standard_ND96isr_H200_v5"}},
					Labels: map[string]string{"kueue.azure.com/gpu-series": "nd-h200-v5"},
				},
				{
					Match:  tauv1alpha1.TauNodeMatch{VMSizes: []string{"Standard_ND96isr_H200_v5"}},
					Labels: map[string]string{"kueue.azure.com/gpu-series": "wrong-series"},
				},
			}},
		},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "h200",
		Labels: map[string]string{azureVMSizeLabel: "Standard_ND96isr_H200_v5"},
	}}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, node).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recordingClient := &resourceMutationRecordingClient{Client: baseClient}
	reconciler := &TauClusterReconciler{Client: recordingClient}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(recordingClient.mutations) != 0 {
		t.Fatalf("conflicting rules caused mutations: %v", recordingClient.mutations)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, cluster); err != nil {
		t.Fatalf("Get TauCluster: %v", err)
	}
	if cluster.Status.Phase != tauv1alpha1.ClusterPhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", cluster.Status.Phase)
	}
	assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionTrue)
}

func TestValidateNodeLabelRulesRejectsReservedKubernetesPrefixes(t *testing.T) {
	for _, key := range []string{"kubernetes.io/arch", "node.kubernetes.io/instance-type"} {
		t.Run(key, func(t *testing.T) {
			err := validateNodeLabelRules([]tauv1alpha1.TauNodeLabelRule{{
				Labels: map[string]string{key: "managed"},
			}})
			if err == nil || !strings.Contains(err.Error(), "reserved Kubernetes label prefixes") {
				t.Fatalf("validateNodeLabelRules() error = %v, want reserved-prefix rejection", err)
			}
		})
	}
}

func TestNodeEventsEnqueueTauClusterSingleton(t *testing.T) {
	requests := enqueueTauClusterForNode(context.Background(), &corev1.Node{})
	if got, want := requests, []reconcile.Request{{NamespacedName: types.NamespacedName{Name: tauv1alpha1.TauClusterSingletonName}}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %#v, want %#v", got, want)
	}
}

func TestNodeWatchIgnoresStatusOnlyUpdates(t *testing.T) {
	watch := nodeLabelChangePredicate()
	original := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "gpu",
			Labels: map[string]string{azureVMSizeLabel: "Standard_ND96isr_H200_v5"},
		},
	}
	statusUpdate := original.DeepCopy()
	statusUpdate.Status.Conditions = []corev1.NodeCondition{{
		Type:   corev1.NodeReady,
		Status: corev1.ConditionTrue,
	}}
	if watch.Update(event.UpdateEvent{ObjectOld: original, ObjectNew: statusUpdate}) {
		t.Fatal("status-only Node update should not enqueue a full topology reconciliation")
	}

	labelUpdate := statusUpdate.DeepCopy()
	labelUpdate.Labels["kueue.azure.com/gpu-series"] = "drifted"
	if !watch.Update(event.UpdateEvent{ObjectOld: statusUpdate, ObjectNew: labelUpdate}) {
		t.Fatal("Node label update must enqueue topology reconciliation")
	}
	if !watch.Create(event.CreateEvent{Object: original}) {
		t.Fatal("Node creation must enqueue topology reconciliation")
	}
	if !watch.Delete(event.DeleteEvent{Object: original}) {
		t.Fatal("Node deletion must enqueue topology reconciliation")
	}
}

func TestTauClusterSpecHashCanonicalizesDefaults(t *testing.T) {
	implicit := tauv1alpha1.TauClusterSpec{}
	explicit := tauv1alpha1.TauClusterSpec{
		ManagementMode: tauv1alpha1.ClusterManagementModeObserve,
		DeletionPolicy: tauv1alpha1.ClusterDeletionPolicyRetain,
		Queues: tauv1alpha1.TauClusterQueuesSpec{
			Ownership: tauv1alpha1.ClusterOwnershipExternal,
		},
		WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{
			DefaultQueue: "jobqueue",
		},
	}
	if got, want := clusterSpecHash(implicit), clusterSpecHash(explicit); got != want {
		t.Fatalf("implicit defaults hash = %q, explicit defaults hash = %q", got, want)
	}
}

func TestWorkspaceReconcileCreatesNamespaceRBACAndReadyStatus(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	localQueue := testLocalQueue("aurora", "aurora", "aurora-cq")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, localQueue, testClusterQueue("aurora-cq")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}}
	// The first reconcile adds the finalizer, the second persists the primary
	// marker, and the third creates access resources.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora"}, &namespace); err != nil {
		t.Fatalf("target namespace not created: %v", err)
	}
	if namespace.Labels[labelPSAEnforce] != "baseline" || namespace.Labels[labelPSAAudit] != "restricted" || namespace.Labels[labelPSAWarn] != "restricted" {
		t.Fatalf("namespace PSA labels = %#v", namespace.Labels)
	}
	if namespace.Labels[labelWorkspace] != "aurora" {
		t.Fatalf("namespace workspace label = %#v, want aurora", namespace.Labels)
	}
	if namespace.Labels[labelWorkspaceLocalQueue] != "aurora" {
		t.Fatalf("namespace LocalQueue label = %q, want aurora", namespace.Labels[labelWorkspaceLocalQueue])
	}
	if namespace.Labels[labelKueueDefaultLocalQueue] != "aurora" {
		t.Fatalf("namespace Kueue default LocalQueue label = %q, want aurora", namespace.Labels[labelKueueDefaultLocalQueue])
	}
	if namespace.Annotations[annotationResultScope] != "/data/projects/aurora/runs" {
		t.Fatalf("namespace result scope = %q, want workspace output root", namespace.Annotations[annotationResultScope])
	}
	var binding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "aurora"}, &binding); err != nil {
		t.Fatalf("researcher rolebinding not reconciled: %v", err)
	}
	if binding.RoleRef.Kind != "ClusterRole" || binding.RoleRef.Name != defaultRoleName {
		t.Fatalf("RoleBinding roleRef = %#v, want ClusterRole %s", binding.RoleRef, defaultRoleName)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "aurora-researchers" {
		t.Fatalf("RoleBinding subjects = %#v, want aurora-researchers", binding.Subjects)
	}
	if binding.Subjects[0].APIGroup != rbacv1.GroupName {
		t.Fatalf("RoleBinding subject apiGroup = %q, want %q", binding.Subjects[0].APIGroup, rbacv1.GroupName)
	}
	var readerRole rbacv1.Role
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.PlatformNamespace}, &readerRole); err != nil {
		t.Fatalf("workspace reader role not reconciled: %v", err)
	}
	if len(readerRole.Rules) == 0 || len(readerRole.Rules[0].ResourceNames) != 1 || readerRole.Rules[0].ResourceNames[0] != "aurora" {
		t.Fatalf("reader role rules = %#v, want resourceNames scoped to aurora", readerRole.Rules)
	}
	var readerBinding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.PlatformNamespace}, &readerBinding); err != nil {
		t.Fatalf("workspace reader rolebinding not reconciled: %v", err)
	}
	if len(readerBinding.Subjects) != 1 || readerBinding.Subjects[0].APIGroup != rbacv1.GroupName {
		t.Fatalf("reader binding subjects = %#v, want group apiGroup", readerBinding.Subjects)
	}
	var serviceAccount corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workload", Namespace: "aurora"}, &serviceAccount); err != nil {
		t.Fatalf("workload identity service account not reconciled: %v", err)
	}
	if serviceAccount.Annotations[annotationAzureWIClientID] != "client-aurora" || serviceAccount.Labels[labelAzureWIUse] != "true" {
		t.Fatalf("service account WI metadata annotations=%#v labels=%#v", serviceAccount.Annotations, serviceAccount.Labels)
	}

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionRBACReady, metav1.ConditionTrue)
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionTrue)
	assertConditionAbsent(t, got.Status.Conditions, "StorageReady")
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionWorkloadIdentityReady, metav1.ConditionTrue)
	if got.Status.Target.ResolvedNamespace != "aurora" {
		t.Fatalf("resolved namespace = %q, want aurora", got.Status.Target.ResolvedNamespace)
	}

	before := got.Status
	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile no-op error = %v", err)
	}
	if result.RequeueAfter != readyRequeue {
		t.Fatalf("no-op result = %#v, want %v drift-repair requeue once Ready", result, readyRequeue)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace after no-op: %v", err)
	}
	if !reflect.DeepEqual(before, got.Status) {
		t.Fatalf("status changed on no-op reconcile:\nbefore=%#v\nafter=%#v", before, got.Status)
	}
}

func TestWorkspaceClusterWideAuthorizationCreatesNoResearcherRBAC(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.PrincipalRef = nil
	workspace.Spec.KubernetesSubject = nil
	workspace.Spec.Role = ""

	staleBinding := testRoleBinding("aurora", defaultRoleName, "aurora")
	staleReaderRole := testRole(tauv1alpha1.PlatformNamespace, "tau-workspace-reader-aurora", "aurora")
	staleReaderBinding := testRoleBinding(tauv1alpha1.PlatformNamespace, "tau-workspace-reader-aurora", "aurora")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			workspace,
			testLocalQueue("aurora", "aurora", "aurora-cq"),
			staleBinding,
			staleReaderRole,
			staleReaderBinding,
		).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("persist primary marker: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile cluster-wide workspace: %v", err)
	}

	for _, obj := range []client.Object{
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: "aurora"}},
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.PlatformNamespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.PlatformNamespace}},
	} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); !apierrors.IsNotFound(err) {
			t.Fatalf("cluster-wide authorization retained subject-specific %T: %v", obj, err)
		}
	}
	var workloadServiceAccount corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workload", Namespace: "aurora"}, &workloadServiceAccount); err != nil {
		t.Fatalf("cluster-wide authorization removed workload identity ServiceAccount: %v", err)
	}

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	rbacReady := findCondition(got.Status.Conditions, tauv1alpha1.ConditionRBACReady)
	if rbacReady == nil || rbacReady.Status != metav1.ConditionTrue || rbacReady.Reason != "ExistingClusterAuthorization" {
		t.Fatalf("RBACReady = %#v, want ExistingClusterAuthorization=True", rbacReady)
	}
}

func TestWorkspaceClusterWideAuthorizationDoesNotDeleteForeignRBAC(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.PrincipalRef = nil
	workspace.Spec.KubernetesSubject = nil
	workspace.Spec.Role = ""
	foreignBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: "aurora"},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			workspace,
			testLocalQueue("aurora", "aurora", "aurora-cq"),
			foreignBinding,
		).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("persist primary marker: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("reconcile cluster-wide workspace: %v", err)
	}
	var got rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "aurora"}, &got); err != nil {
		t.Fatalf("foreign RoleBinding was deleted: %v", err)
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &gotWorkspace); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	rbacReady := findCondition(gotWorkspace.Status.Conditions, tauv1alpha1.ConditionRBACReady)
	if rbacReady == nil || rbacReady.Status != metav1.ConditionFalse ||
		rbacReady.Reason != "AuthorizationNotReady" || !strings.Contains(rbacReady.Message, "refusing to delete") {
		t.Fatalf("RBACReady = %#v, want refusal to delete foreign RBAC", rbacReady)
	}
}

func TestCleanupPlatformReaderRBACDoesNotDeleteForeignObject(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	foreignRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.PlatformNamespace},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreignRole).Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	err := reconciler.cleanupPlatformReaderRBAC(ctx, "aurora")
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("cleanup error = %v, want refusal to delete foreign reader Role", err)
	}
	var got rbacv1.Role
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreignRole), &got); err != nil {
		t.Fatalf("foreign reader Role was deleted: %v", err)
	}
}

func TestWorkspaceReconcilePreservesExistingNamespaceMetadata(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Target.CreateNamespace = false
	namespace := testNamespace("aurora")
	namespace.Labels = map[string]string{
		labelPSAEnforce: "baseline",
		labelPSAAudit:   "baseline",
		labelPSAWarn:    "baseline",
	}
	localQueue := testLocalQueue("aurora", "aurora", "aurora-cq")
	existingBinding := testRoleBinding("aurora", defaultRoleName, "aurora")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, localQueue, existingBinding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora"}, &got); err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got.Labels[labelPSAEnforce] != "baseline" || got.Labels[labelPSAAudit] != "baseline" || got.Labels[labelPSAWarn] != "baseline" {
		t.Fatalf("existing namespace PSA labels changed: got %#v", got.Labels)
	}
	if got.Labels[labelWorkspace] != "aurora" {
		t.Fatalf("existing namespace workspace label = %#v, want aurora", got.Labels)
	}
	if got.Labels[labelWorkspaceLocalQueue] != "aurora" || got.Annotations[annotationResultScope] != "/data/projects/aurora/runs" {
		t.Fatalf("existing namespace workspace contract = labels %#v annotations %#v", got.Labels, got.Annotations)
	}
	var gotBinding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "aurora"}, &gotBinding); err != nil {
		t.Fatalf("Get researcher RoleBinding: %v", err)
	}
	if gotBinding.RoleRef.Kind != "ClusterRole" || gotBinding.RoleRef.Name != defaultRoleName {
		t.Fatalf("RoleBinding roleRef = %#v, want ClusterRole %s", gotBinding.RoleRef, defaultRoleName)
	}
}

func TestWorkspaceReconcileReportsNamespaceOwnedByAnotherWorkspace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	namespace := testNamespace("aurora")
	namespace.Labels = map[string]string{labelWorkspace: "other-workspace"}
	foreignBinding := testRoleBinding("aurora", defaultRoleName, "other-workspace")
	foreignBinding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: defaultRoleName}
	foreignBinding.Subjects = []rbacv1.Subject{{Kind: "Group", APIGroup: rbacv1.GroupName, Name: "other-researchers"}}
	foreignServiceAccount := testServiceAccount("aurora", "tau-workload", "other-workspace")
	foreignServiceAccount.Annotations = map[string]string{annotationAzureWIClientID: "other-client"}
	// The owning workspace must exist: a namespace is only off-limits while its
	// owner is live. A dangling owner is reclaimable by design.
	otherWorkspace := testWorkspace("other-workspace")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, foreignBinding, foreignServiceAccount, otherWorkspace, testClusterQueue("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      "aurora",
		Namespace: tauv1alpha1.PlatformNamespace,
	}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora"}, &got); err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got.Labels[labelWorkspace] != "other-workspace" {
		t.Fatalf("foreign namespace labels changed: %#v", got.Labels)
	}
	var gotBinding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "aurora"}, &gotBinding); err != nil {
		t.Fatalf("Get foreign RoleBinding: %v", err)
	}
	if gotBinding.Labels[labelWorkspace] != "other-workspace" || len(gotBinding.Subjects) != 1 || gotBinding.Subjects[0].Name != "other-researchers" {
		t.Fatalf("foreign RoleBinding changed: %#v", gotBinding)
	}
	var gotServiceAccount corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workload", Namespace: "aurora"}, &gotServiceAccount); err != nil {
		t.Fatalf("Get foreign ServiceAccount: %v", err)
	}
	if gotServiceAccount.Labels[labelWorkspace] != "other-workspace" || gotServiceAccount.Annotations[annotationAzureWIClientID] != "other-client" {
		t.Fatalf("foreign ServiceAccount changed: %#v", gotServiceAccount)
	}
	localQueue := &unstructured.Unstructured{}
	localQueue.SetGroupVersionKind(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: "aurora"}, localQueue); !apierrors.IsNotFound(err) {
		t.Fatalf("foreign target received a LocalQueue, err=%v object=%#v", err, localQueue.Object)
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &gotWorkspace); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if gotWorkspace.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
		t.Fatalf("workspace phase = %q, want Degraded", gotWorkspace.Status.Phase)
	}
	assertCondition(t, gotWorkspace.Status.Conditions, tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue)
}

func TestWorkspaceNamespaceMutationFailureBlocksNamespacedResources(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, testClusterQueue("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	c := &namespaceMutationFailingClient{
		Client:    baseClient,
		createErr: errors.New("injected namespace create failure"),
	}
	reconciler := &TauWorkspaceReconciler{Client: c}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      "aurora",
		Namespace: tauv1alpha1.PlatformNamespace,
	}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "aurora"}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("namespace failure created researcher RoleBinding: %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: "tau-workload", Namespace: "aurora"}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("namespace failure created workload identity ServiceAccount: %v", err)
	}
	localQueue := &unstructured.Unstructured{}
	localQueue.SetGroupVersionKind(localQueueGVK)
	if err := baseClient.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: "aurora"}, localQueue); !apierrors.IsNotFound(err) {
		t.Fatalf("namespace failure created LocalQueue: %v", err)
	}
}

func TestWorkspaceReconcileRefusesToAdoptForeignAccessObjects(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	foreignBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: "aurora"},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "foreign-role"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "foreign-group"}},
	}
	foreignServiceAccount := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "tau-workload",
			Namespace:   "aurora",
			Annotations: map[string]string{annotationAzureWIClientID: "foreign-client"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, foreignBinding, foreignServiceAccount, testClusterQueue("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var gotBinding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreignBinding), &gotBinding); err != nil {
		t.Fatalf("Get foreign RoleBinding: %v", err)
	}
	if gotBinding.RoleRef.Name != "foreign-role" || gotBinding.Subjects[0].Name != "foreign-group" || len(gotBinding.Labels) != 0 {
		t.Fatalf("foreign RoleBinding was adopted: %#v", gotBinding)
	}
	var gotServiceAccount corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreignServiceAccount), &gotServiceAccount); err != nil {
		t.Fatalf("Get foreign ServiceAccount: %v", err)
	}
	if gotServiceAccount.Annotations[annotationAzureWIClientID] != "foreign-client" || len(gotServiceAccount.Labels) != 0 {
		t.Fatalf("foreign ServiceAccount was adopted: %#v", gotServiceAccount)
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	assertCondition(t, gotWorkspace.Status.Conditions, tauv1alpha1.ConditionRBACReady, metav1.ConditionFalse)
	assertCondition(t, gotWorkspace.Status.Conditions, tauv1alpha1.ConditionWorkloadIdentityReady, metav1.ConditionFalse)
}

func TestWorkspaceReconcileCleansStaleTargetRBAC(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Generation = 2
	workspace.Status = tauv1alpha1.TauWorkspaceStatus{
		Phase:              tauv1alpha1.WorkspacePhaseReady,
		ObservedGeneration: 1,
		Target:             tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: "old-aurora"},
	}
	localQueue := testLocalQueue("aurora", "aurora", "aurora-cq")
	staleRole := testRole("old-aurora", defaultRoleName, "aurora")
	staleBinding := testRoleBinding("old-aurora", defaultRoleName, "aurora")
	staleServiceAccount := testServiceAccount("old-aurora", "tau-workload", "aurora")
	staleNamespace := testNamespace("old-aurora")
	staleNamespace.Labels = map[string]string{
		labelManagedBy:              labelManagedByValue,
		labelWorkspace:              "aurora",
		labelWorkspaceLocalQueue:    "aurora",
		labelKueueDefaultLocalQueue: "aurora",
		"preserved":                 "true",
	}
	staleNamespace.Annotations = map[string]string{
		annotationResultScope: "/data/projects/aurora/runs",
		"preserved":           "true",
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, localQueue, staleRole, staleBinding, staleServiceAccount, staleNamespace).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "old-aurora"}, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected stale role deleted, got err=%v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "old-aurora"}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected stale rolebinding deleted, got err=%v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workload", Namespace: "old-aurora"}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected stale serviceaccount deleted, got err=%v", err)
	}
	var gotNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "old-aurora"}, &gotNamespace); err != nil {
		t.Fatalf("Get stale namespace: %v", err)
	}
	for _, key := range []string{labelManagedBy, labelWorkspace, labelWorkspaceLocalQueue, labelKueueDefaultLocalQueue} {
		if _, ok := gotNamespace.Labels[key]; ok {
			t.Fatalf("stale namespace retained controller label %q: %#v", key, gotNamespace.Labels)
		}
	}
	if gotNamespace.Labels["preserved"] != "true" || gotNamespace.Annotations["preserved"] != "true" {
		t.Fatalf("stale namespace cleanup changed unrelated metadata: labels=%#v annotations=%#v", gotNamespace.Labels, gotNamespace.Annotations)
	}
	if _, ok := gotNamespace.Annotations[annotationResultScope]; ok {
		t.Fatalf("stale namespace retained result scope: %#v", gotNamespace.Annotations)
	}
}

func TestWorkspaceReconcileCleansRenamedAndRemovedWorkloadIdentity(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	localQueue := testLocalQueue("aurora", "aurora", "aurora-cq")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, localQueue, testClusterQueue("aurora-cq")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var current tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &current); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	current.Spec.WorkloadIdentity.ServiceAccountName = "tau-workload-v2"
	if err := c.Update(ctx, &current); err != nil {
		t.Fatalf("rename workload identity: %v", err)
	}
	reconcileWorkspace(t, reconciler, ctx, "aurora")

	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workload", Namespace: "aurora"}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("renamed workload identity retained old ServiceAccount: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workload-v2", Namespace: "aurora"}, &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("renamed workload identity did not create new ServiceAccount: %v", err)
	}

	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &current); err != nil {
		t.Fatalf("Get renamed workspace: %v", err)
	}
	current.Spec.WorkloadIdentity = nil
	if err := c.Update(ctx, &current); err != nil {
		t.Fatalf("remove workload identity: %v", err)
	}
	reconcileWorkspace(t, reconciler, ctx, "aurora")

	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workload-v2", Namespace: "aurora"}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("removed workload identity retained ServiceAccount: %v", err)
	}
}

func TestWorkspaceDeleteCleansWorkspaceAccess(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	now := metav1.Now()
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.DeletionTimestamp = &now
	workspace.Status.Target.ResolvedNamespace = "aurora"
	targetNamespace := testNamespace("aurora")
	targetNamespace.Labels = map[string]string{labelWorkspace: "aurora"}
	targetRole := testRole("aurora", defaultRoleName, "aurora")
	targetBinding := testRoleBinding("aurora", defaultRoleName, "aurora")
	targetServiceAccount := testServiceAccount("aurora", "tau-workload", "aurora")
	readerRole := testRole(tauv1alpha1.PlatformNamespace, "tau-workspace-reader-aurora", "aurora")
	readerBinding := testRoleBinding(tauv1alpha1.PlatformNamespace, "tau-workspace-reader-aurora", "aurora")
	localQueue := testLocalQueue("aurora", "aurora", "aurora")
	localQueue.SetLabels(workspaceLabels("aurora"))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, targetNamespace, targetRole, targetBinding, targetServiceAccount, readerRole, readerBinding, localQueue).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}}); err != nil {
		t.Fatalf("Reconcile delete error = %v", err)
	}
	for _, key := range []client.ObjectKey{
		{Name: defaultRoleName, Namespace: "aurora"},
		{Name: "tau-workload", Namespace: "aurora"},
		{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.PlatformNamespace},
	} {
		if key.Name == "tau-workload" {
			if err := c.Get(ctx, key, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
				t.Fatalf("expected serviceaccount %s deleted, got err=%v", key, err)
			}
		} else {
			if err := c.Get(ctx, key, &rbacv1.Role{}); !apierrors.IsNotFound(err) {
				t.Fatalf("expected role %s deleted, got err=%v", key, err)
			}
			if err := c.Get(ctx, key, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
				t.Fatalf("expected rolebinding %s deleted, got err=%v", key, err)
			}
		}
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora"}, targetNamespace); err != nil {
		t.Fatalf("target namespace was deleted: %v", err)
	}
	deletedLocalQueue := &unstructured.Unstructured{}
	deletedLocalQueue.SetGroupVersionKind(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: "aurora"}, deletedLocalQueue); !apierrors.IsNotFound(err) {
		t.Fatalf("expected controller-owned LocalQueue deleted, got err=%v", err)
	}
}

func TestWorkspaceRefusesReservedTargetNamespace(t *testing.T) {
	for _, reserved := range []string{
		tauv1alpha1.PlatformNamespace,
		"kube-system",
		"kube-public",
		"kube-node-lease",
		metav1.NamespaceDefault,
	} {
		t.Run(reserved, func(t *testing.T) {
			ctx := context.Background()
			scheme := testScheme(t)
			workspace := testWorkspace("aurora")
			workspace.Spec.Target.Namespace = reserved
			// Helm owns tau-platform; kube-system carries cluster-critical
			// metadata. Both must survive a workspace pointed at them.
			namespace := testNamespace(reserved)
			namespace.Labels = map[string]string{
				labelManagedBy:              "Helm",
				labelKueueDefaultLocalQueue: "platform-queue",
			}
			localQueue := testLocalQueue("aurora", reserved, "aurora-cq")
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(workspace, namespace, localQueue).
				WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
				Build()
			reconciler := &TauWorkspaceReconciler{Client: c}

			reconcileWorkspace(t, reconciler, ctx, "aurora")

			var got corev1.Namespace
			if err := c.Get(ctx, client.ObjectKey{Name: reserved}, &got); err != nil {
				t.Fatalf("Get namespace: %v", err)
			}
			if got.Labels[labelManagedBy] != "Helm" {
				t.Fatalf("reserved namespace managed-by = %q, want Helm untouched", got.Labels[labelManagedBy])
			}
			if got.Labels[labelKueueDefaultLocalQueue] != "platform-queue" {
				t.Fatalf("reserved namespace default-local-queue = %q, want platform-queue untouched", got.Labels[labelKueueDefaultLocalQueue])
			}
			if _, ok := got.Labels[labelWorkspace]; ok {
				t.Fatalf("reserved namespace was claimed by a workspace: %#v", got.Labels)
			}

			var updated tauv1alpha1.TauWorkspace
			if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &updated); err != nil {
				t.Fatalf("Get workspace: %v", err)
			}
			if updated.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
				t.Fatalf("workspace phase = %q, want Degraded", updated.Status.Phase)
			}
		})
	}
}

// A workspace retargeted away from a reserved namespace must not strip that
// namespace's queue metadata on the way out: reconcileNamespace refuses the
// reserved target, so cleanupStaleNamespaceMetadata never runs against it.
func TestWorkspaceRetargetDoesNotStripReservedNamespaceMetadata(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Target.Namespace = "aurora"
	workspace.Status.Target.ResolvedNamespace = tauv1alpha1.PlatformNamespace
	platform := testNamespace(tauv1alpha1.PlatformNamespace)
	platform.Labels = map[string]string{
		labelWorkspace:              "aurora",
		labelManagedBy:              labelManagedByValue,
		labelKueueDefaultLocalQueue: "platform-queue",
	}
	localQueue := testLocalQueue("aurora", "aurora", "aurora-cq")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, platform, localQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get platform namespace: %v", err)
	}
	if got.Labels[labelKueueDefaultLocalQueue] != "platform-queue" {
		t.Fatalf("platform namespace lost its Kueue default queue label: %#v", got.Labels)
	}
}

func TestWorkspaceReconcileReportsMissingQueue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, testNamespace("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionFalse)
}

func TestWorkspaceReconcileCreatesWorkspaceLocalQueue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Queue = "jobqueue"
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, testClusterQueue("jobqueue")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	localQueue := &unstructured.Unstructured{}
	localQueue.SetGroupVersionKind(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "jobqueue", Namespace: "aurora"}, localQueue); err != nil {
		t.Fatalf("Get reconciled LocalQueue: %v", err)
	}
	clusterQueue, _, _ := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	if clusterQueue != "jobqueue" {
		t.Fatalf("LocalQueue clusterQueue = %q, want jobqueue", clusterQueue)
	}
	if localQueue.GetLabels()[labelManagedBy] != labelManagedByValue ||
		localQueue.GetLabels()[labelWorkspace] != "aurora" {
		t.Fatalf("LocalQueue ownership labels = %#v", localQueue.GetLabels())
	}

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	if got.Status.Queue.LocalQueue != "jobqueue" || got.Status.Queue.ClusterQueue != "jobqueue" {
		t.Fatalf("queue status = %#v, want jobqueue -> jobqueue", got.Status.Queue)
	}
}

func TestWorkspaceReconcileBlocksAdditionalV0Workspace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	primary := testWorkspace("zeta")
	primary.CreationTimestamp = metav1.NewTime(time.Date(2026, 7, 20, 0, 0, 0, 0, time.UTC))
	primaryNamespace := testNamespace("zeta")
	primaryNamespace.Labels = workspaceNamespaceLabels("zeta", primary.Spec.Queue)
	additional := testWorkspace("alpha")
	additional.CreationTimestamp = primary.CreationTimestamp
	additional.Spec.Queue = "jobqueue"
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(primary, primaryNamespace, additional, testClusterQueue("jobqueue")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "alpha")

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get additional workspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionRBACReady, metav1.ConditionFalse)
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionFalse)
	for _, condition := range got.Status.Conditions {
		if (condition.Type == tauv1alpha1.ConditionRBACReady || condition.Type == tauv1alpha1.ConditionQueueReady) &&
			condition.Reason != "AdditionalWorkspaceBlocked" {
			t.Fatalf("%s reason = %q, want AdditionalWorkspaceBlocked", condition.Type, condition.Reason)
		}
	}

	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha"}, &namespace); !apierrors.IsNotFound(err) {
		t.Fatalf("additional workspace Namespace exists or lookup failed: %v", err)
	}
	localQueue := &unstructured.Unstructured{}
	localQueue.SetGroupVersionKind(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "jobqueue", Namespace: "alpha"}, localQueue); !apierrors.IsNotFound(err) {
		t.Fatalf("additional workspace LocalQueue exists or lookup failed: %v", err)
	}
	var binding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "alpha"}, &binding); !apierrors.IsNotFound(err) {
		t.Fatalf("additional workspace RoleBinding exists or lookup failed: %v", err)
	}
}

func TestWorkspacePersistsPrimaryMarkerBeforeCreatingAccessResources(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("research")
	workspace.Spec.Queue = "jobqueue"
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, testClusterQueue("jobqueue")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      "research",
		Namespace: tauv1alpha1.PlatformNamespace,
	}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("finalizer reconcile: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("primary marker reconcile: %v", err)
	}

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "research", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Annotations[annotationV0Primary] != "true" {
		t.Fatalf("primary marker = %q, want true", got.Annotations[annotationV0Primary])
	}
	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "research"}, &namespace); !apierrors.IsNotFound(err) {
		t.Fatalf("Namespace created before primary marker became observable: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("resource reconcile: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "research"}, &namespace); err != nil {
		t.Fatalf("Namespace not created after primary marker: %v", err)
	}
}

func TestWorkspacePromotionWaitsForTerminatingPrimaryCleanup(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	primary := testWorkspace("zeta")
	primary.Finalizers = []string{workspaceFinalizer}
	primary.DeletionTimestamp = &now
	primary.Annotations = map[string]string{annotationV0Primary: "true"}
	primaryBinding := testRoleBinding("zeta", defaultRoleName, "zeta")
	primaryQueue := testLocalQueue("zeta", "zeta", "zeta")
	additional := testWorkspace("alpha")
	additional.Spec.Queue = "jobqueue"
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(primary, primaryBinding, primaryQueue, additional, testClusterQueue("jobqueue")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "alpha")
	var alphaNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha"}, &alphaNamespace); !apierrors.IsNotFound(err) {
		t.Fatalf("additional workspace activated before primary cleanup: %v", err)
	}
	var gotAdditional tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha", Namespace: tauv1alpha1.PlatformNamespace}, &gotAdditional); err != nil {
		t.Fatalf("Get additional workspace: %v", err)
	}
	if gotAdditional.Annotations[annotationV0Primary] == "true" {
		t.Fatal("additional workspace claimed primary before terminating primary cleanup")
	}

	primaryReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "zeta", Namespace: tauv1alpha1.PlatformNamespace}}
	if _, err := reconciler.Reconcile(ctx, primaryReq); err != nil {
		t.Fatalf("cleanup terminating primary: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: "zeta"}, &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("terminating primary RoleBinding survived cleanup: %v", err)
	}

	additionalReq := ctrl.Request{NamespacedName: types.NamespacedName{Name: "alpha", Namespace: tauv1alpha1.PlatformNamespace}}
	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, additionalReq); err != nil {
			t.Fatalf("promote additional workspace iteration %d: %v", i, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKey{Name: "alpha"}, &alphaNamespace); err != nil {
		t.Fatalf("additional workspace did not activate after primary cleanup: %v", err)
	}
}

func TestWorkspaceReconcileRestoresOwnedLocalQueueClusterQueue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	localQueue := testLocalQueue("aurora", "aurora", "other-cq")
	localQueue.SetLabels(workspaceLabels("aurora"))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, localQueue, testClusterQueue("aurora"), testClusterQueue("other-cq")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: "aurora"}, got); err != nil {
		t.Fatalf("Get LocalQueue: %v", err)
	}
	clusterQueue, _, _ := unstructured.NestedString(got.Object, "spec", "clusterQueue")
	if clusterQueue != "aurora" {
		t.Fatalf("owned LocalQueue clusterQueue = %q, want restored to aurora", clusterQueue)
	}
}

func TestWorkspaceReconcileDegradesWhenBackingClusterQueueDisappears(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	localQueue := testLocalQueue("aurora", "aurora", "aurora-cq")
	clusterQueue := testClusterQueue("aurora-cq")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, localQueue, clusterQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")
	if err := c.Delete(ctx, clusterQueue); err != nil {
		t.Fatalf("delete backing ClusterQueue: %v", err)
	}
	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded after backing ClusterQueue deletion", got.Status.Phase)
	}
	queueReady := findCondition(got.Status.Conditions, tauv1alpha1.ConditionQueueReady)
	if queueReady == nil || queueReady.Status != metav1.ConditionFalse || !strings.Contains(queueReady.Message, "aurora-cq") {
		t.Fatalf("QueueReady = %#v, want missing backing ClusterQueue", queueReady)
	}
}

func TestWorkspaceReconcileDoesNotRequireStorage(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	localQueue := testLocalQueue("aurora", "aurora", "aurora-cq")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, testNamespace("aurora"), localQueue, testClusterQueue("aurora-cq")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready without a PVC", got.Status.Phase)
	}
	assertConditionAbsent(t, got.Status.Conditions, "StorageReady")
}

// A workspace blocked on a missing ClusterQueue must retry on its own because
// Kueue may be installed or configured after the Tau controller starts.
func TestWorkspaceReconcileRequeuesWhileNotReady(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(testWorkspace("aurora"), testNamespace("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}}

	var result ctrl.Result
	for i := 0; i < 3; i++ {
		var err error
		if result, err = reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Phase == tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = Ready, want not-Ready without a LocalQueue")
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionFalse)
	if result.RequeueAfter != notReadyRequeue {
		t.Fatalf("RequeueAfter = %v, want %v while not Ready", result.RequeueAfter, notReadyRequeue)
	}
}

func TestQuotaRequestReportOnlyApproval(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	request := testQuotaRequest("aurora-h200-burst")
	request.Annotations = map[string]string{
		annotationApproved:   "true",
		annotationReviewedBy: "platform",
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(request).
		WithStatusSubresource(&tauv1alpha1.TauQuotaRequest{}).
		Build()
	reconciler := &TauQuotaRequestReconciler{Client: c}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: request.Name, Namespace: request.Namespace}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var got tauv1alpha1.TauQuotaRequest
	if err := c.Get(ctx, client.ObjectKey{Name: request.Name, Namespace: request.Namespace}, &got); err != nil {
		t.Fatalf("Get quota request: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.QuotaRequestPhaseApproved {
		t.Fatalf("phase = %q, want Approved", got.Status.Phase)
	}
	if got.Status.Decision == "" || got.Status.ApprovedBy != "platform" {
		t.Fatalf("status = %#v, want decision and reviewer", got.Status)
	}
	assertCondition(t, got.Status.Conditions, "QuotaMutated", metav1.ConditionFalse)

	before := got.Status
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: request.Name, Namespace: request.Namespace}}); err != nil {
		t.Fatalf("Reconcile no-op error = %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: request.Name, Namespace: request.Namespace}, &got); err != nil {
		t.Fatalf("Get quota request after no-op: %v", err)
	}
	if !reflect.DeepEqual(before, got.Status) {
		t.Fatalf("quota request status changed on no-op reconcile:\nbefore=%#v\nafter=%#v", before, got.Status)
	}
}

func TestMergeConditionsDeduplicatesByType(t *testing.T) {
	oldTime := metav1.Now()
	existing := []metav1.Condition{{
		Type:               tauv1alpha1.ConditionDriftDetected,
		Status:             metav1.ConditionFalse,
		Reason:             "NoDrift",
		Message:            "target namespace is reconciled",
		LastTransitionTime: oldTime,
	}}
	desired := []metav1.Condition{
		condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionFalse, "NoDrift", "target namespace is reconciled", 1),
		condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "RBACCleanupFailed", "cleanup failed", 1),
	}
	got := mergeConditions(existing, desired)
	if len(got) != 1 {
		t.Fatalf("conditions len = %d, want 1: %#v", len(got), got)
	}
	if got[0].Status != metav1.ConditionTrue || got[0].Reason != "RBACCleanupFailed" {
		t.Fatalf("condition = %#v, want latest DriftDetected", got[0])
	}
}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("Add client-go scheme: %v", err)
	}

	if err := tauv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("Add Tau scheme: %v", err)
	}
	return scheme
}

type resourceMutationRecordingClient struct {
	client.Client
	mutations []string
}

type namespaceMutationFailingClient struct {
	client.Client
	createErr error
	updateErr error
}

func (c *namespaceMutationFailingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if _, ok := obj.(*corev1.Namespace); ok && c.createErr != nil {
		return c.createErr
	}
	return c.Client.Create(ctx, obj, opts...)
}

func (c *namespaceMutationFailingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.Namespace); ok && c.updateErr != nil {
		return c.updateErr
	}
	return c.Client.Update(ctx, obj, opts...)
}

func (c *resourceMutationRecordingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.mutations = append(c.mutations, "create "+obj.GetName())
	return c.Client.Create(ctx, obj, opts...)
}

func (c *resourceMutationRecordingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.mutations = append(c.mutations, "update "+obj.GetName())
	return c.Client.Update(ctx, obj, opts...)
}

func (c *resourceMutationRecordingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.mutations = append(c.mutations, "patch "+obj.GetName())
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *resourceMutationRecordingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.mutations = append(c.mutations, "delete "+obj.GetName())
	return c.Client.Delete(ctx, obj, opts...)
}

func reconcileWorkspace(t *testing.T, reconciler *TauWorkspaceReconciler, ctx context.Context, name string) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: tauv1alpha1.PlatformNamespace}}
	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() iteration %d error = %v", i, err)
		}
	}
}

func TestWorkspaceWithoutQueueResolvesTauClusterDefault(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Queue = ""
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, cluster, testClusterQueue("jobqueue")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	// The finalizer and the v0 primary-workspace marker each take a pass
	// before the workspace body reconciles.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("mark primary workspace: %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("Get TauWorkspace: %v", err)
	}
	if got.Spec.Queue != "" {
		t.Fatalf("spec.queue = %q, want the stored spec left untouched", got.Spec.Queue)
	}
	if got.Status.Queue.LocalQueue != "jobqueue" || got.Status.Queue.ClusterQueue != "jobqueue" {
		t.Fatalf("status.queue = %#v, want the TauCluster default", got.Status.Queue)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionTrue)

	localQueue := &unstructured.Unstructured{}
	localQueue.SetGroupVersionKind(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: "aurora", Name: "jobqueue"}, localQueue); err != nil {
		t.Fatalf("Get workspace LocalQueue: %v", err)
	}

	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora"}, &namespace); err != nil {
		t.Fatalf("Get workspace namespace: %v", err)
	}
	if namespace.Labels[labelKueueDefaultLocalQueue] != "jobqueue" {
		t.Fatalf("namespace labels = %#v", namespace.Labels)
	}
}

func TestWorkspaceWithoutQueueOrClusterDefaultIsDegraded(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Queue = ""
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	recordingClient := &resourceMutationRecordingClient{Client: c}
	reconciler := &TauWorkspaceReconciler{Client: recordingClient}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
	// The v0 primary-workspace marker is written on its own pass, so let it
	// settle before asserting that an unresolved queue touches nothing.
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("mark primary workspace: %v", err)
	}
	recordingClient.mutations = nil
	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(recordingClient.mutations) != 0 {
		t.Fatalf("unresolved queue mutated resources: %v", recordingClient.mutations)
	}

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("Get TauWorkspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionFalse)
	if queue := findCondition(got.Status.Conditions, tauv1alpha1.ConditionQueueReady); queue == nil || queue.Reason != "QueueUnresolved" {
		t.Fatalf("QueueReady = %#v", queue)
	}
}

func testWorkspace(name string) *tauv1alpha1.TauWorkspace {
	return &tauv1alpha1.TauWorkspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: tauv1alpha1.GroupVersion.String(), Kind: tauv1alpha1.KindTauWorkspace},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tauv1alpha1.PlatformNamespace},
		Spec: tauv1alpha1.TauWorkspaceSpec{
			PrincipalRef:      &tauv1alpha1.PrincipalRef{Provider: tauv1alpha1.PrincipalProviderEntra, Name: name + "-researchers"},
			KubernetesSubject: &tauv1alpha1.KubernetesSubject{Kind: "Group", Name: name + "-researchers"},
			Role:              "tau-researcher-v1",
			Target:            tauv1alpha1.WorkspaceTarget{Namespace: name, CreateNamespace: true},
			Queue:             name,
			WorkloadIdentity:  &tauv1alpha1.WorkspaceWorkloadIdentity{ServiceAccountName: "tau-workload", ClientID: "client-" + name},
			Defaults:          tauv1alpha1.WorkspaceDefaults{OutputRoot: "/data/projects/" + name + "/runs", Priority: "normal"},
		},
	}
}

func testQuotaRequest(name string) *tauv1alpha1.TauQuotaRequest {
	return &tauv1alpha1.TauQuotaRequest{
		TypeMeta:   metav1.TypeMeta{APIVersion: tauv1alpha1.GroupVersion.String(), Kind: tauv1alpha1.KindTauQuotaRequest},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tauv1alpha1.PlatformNamespace},
		Spec: tauv1alpha1.TauQuotaRequestSpec{
			Workspace:    "aurora",
			Resource:     "h200",
			Requested:    32,
			Reason:       "train-70b-checkpoint-sweep",
			MutationMode: tauv1alpha1.QuotaMutationModeReportOnly,
		},
	}
}

func testNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
}

func testRole(namespace, name, workspace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    workspaceLabels(workspace),
		},
	}
}

func testRoleBinding(namespace, name, workspace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    workspaceLabels(workspace),
		},
	}
}

func testServiceAccount(namespace, name, workspace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    workspaceLabels(workspace),
		},
	}
}

func testLocalQueue(namespace, name, clusterQueue string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": localQueueGVK.GroupVersion().String(),
		"kind":       "LocalQueue",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"clusterQueue": clusterQueue,
		},
	}}
	obj.SetGroupVersionKind(localQueueGVK)
	return obj
}

func testClusterQueue(name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": clusterQueueGVK.GroupVersion().String(),
		"kind":       clusterQueueGVK.Kind,
		"metadata": map[string]any{
			"name": name,
		},
	}}
	obj.SetGroupVersionKind(clusterQueueGVK)
	return obj
}

func assertCondition(t *testing.T, conditions []metav1.Condition, conditionType string, status metav1.ConditionStatus) {
	t.Helper()
	for _, c := range conditions {
		if c.Type == conditionType {
			if c.Status != status {
				t.Fatalf("condition %s status = %s, want %s (condition=%#v)", conditionType, c.Status, status, c)
			}
			return
		}
	}
	t.Fatalf("condition %s missing from %#v", conditionType, conditions)
}

func assertConditionAbsent(t *testing.T, conditions []metav1.Condition, conditionType string) {
	t.Helper()
	if got := findCondition(conditions, conditionType); got != nil {
		t.Fatalf("condition %s unexpectedly present: %#v", conditionType, got)
	}
}

func TestWorkspaceReclaimsNamespaceFromDeletedOwner(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	stranded := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shared",
		Labels: map[string]string{labelWorkspace: "ghost"},
	}}
	workspace := testWorkspace("aurora")
	workspace.Spec.Target.Namespace = "shared"
	localQueue := testLocalQueue("aurora", "shared", "aurora-cq")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, localQueue, stranded).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := &TauWorkspaceReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.PlatformNamespace}}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	var reclaimed corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "shared"}, &reclaimed); err != nil {
		t.Fatalf("namespace missing: %v", err)
	}
	if reclaimed.Labels[labelWorkspace] != "aurora" {
		t.Fatalf("namespace orphaned by a deleted owner must be reclaimable, labels = %#v", reclaimed.Labels)
	}
}
