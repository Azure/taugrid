// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
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
						labelkeys.LabelGPUClass:      "h200-141gb",
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
	if _, ok := unchanged.Labels[labelkeys.LabelGPUClass]; ok {
		t.Fatal("Observe mode changed the node GPU class label")
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

func TestValidateNodeLabelRulesRejectsDerivedTopologyLabels(t *testing.T) {
	for _, key := range []string{
		labelkeys.LabelSite,
		labelkeys.LabelRegion,
		labelkeys.LabelNetworkDomain,
		labelkeys.LabelAcceleratorDomain,
		labelkeys.LabelInfiniband,
	} {
		t.Run(key, func(t *testing.T) {
			err := validateNodeLabelRules([]tauv1alpha1.TauNodeLabelRule{{
				Labels: map[string]string{key: "custom"},
			}})
			if err == nil ||
				!strings.Contains(err.Error(), "topology label is derived") ||
				!strings.Contains(err.Error(), key) {
				t.Fatalf("validateNodeLabelRules() error = %v, want derived-label rejection", err)
			}
		})
	}
}

func TestTauClusterRejectsDerivedTopologyRuleWithoutNodeOscillation(t *testing.T) {
	ctx := context.Background()
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			ManagementMode: tauv1alpha1.ClusterManagementModeReconcile,
			Nodes: tauv1alpha1.TauClusterNodesSpec{LabelRules: []tauv1alpha1.TauNodeLabelRule{{
				Labels: map[string]string{labelkeys.LabelSite: "custom"},
			}}},
		},
	}
	node := topologyTestNode("managed-h200", map[string]string{
		labelRegion:       "eastus2",
		azureVMSizeLabel:  "Standard_ND96isr_H200_v5",
		labelAKSAgentPool: "research",
	}, "azure:///managed-h200")
	baseClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, node).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recording := &resourceMutationRecordingClient{Client: baseClient}
	reconciler := &TauClusterReconciler{Client: recording}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}

	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() iteration %d error = %v", i, err)
		}
	}
	if len(recording.mutations) != 0 {
		t.Fatalf("invalid derived-label rule mutated resources across reconciles: %v", recording.mutations)
	}

	var gotNode corev1.Node
	if err := baseClient.Get(ctx, client.ObjectKey{Name: node.Name}, &gotNode); err != nil {
		t.Fatal(err)
	}
	if hasManagedTopologyLabels(&gotNode) {
		t.Fatalf("invalid rule allowed topology reconciliation: %#v", gotNode.Labels)
	}
	var gotCluster tauv1alpha1.TauCluster
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, &gotCluster); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, gotCluster.Status.Conditions, tauv1alpha1.ConditionNodesReady, metav1.ConditionFalse)
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
							labelkeys.LabelGPUClass:      "a100-80gb",
						},
					},
					{
						Match: tauv1alpha1.TauNodeMatch{VMSizes: []string{"Standard_ND96isr_H200_v5"}},
						Labels: map[string]string{
							"kueue.azure.com/gpu-series": "nd-h200-v5",
							labelkeys.LabelGPUClass:      "h200-141gb",
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
			labelRegion:                            "eastus2",
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
	if paused == nil || paused.Status != metav1.ConditionFalse || paused.Reason != "TopologyReconciliationActive" {
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
	if got, want := recordingClient.mutations, []string{
		"patch flex-h200",
		"patch native-a100",
		"patch flex-h200",
		"patch native-a100",
		"patch system-cpu",
		"create " + tauGPUNodeTopologyName,
	}; !reflect.DeepEqual(got, want) {
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
	for name, want := range map[string]string{
		nativeNode.Name: "a100-80gb",
		flexNode.Name:   "h200-141gb",
	} {
		var node corev1.Node
		if err := baseClient.Get(ctx, client.ObjectKey{Name: name}, &node); err != nil {
			t.Fatalf("Get Node %q: %v", name, err)
		}
		if got := node.Labels[labelkeys.LabelGPUClass]; got != want {
			t.Fatalf("Node %q gpu-class = %q, want %q", name, got, want)
		}
	}
	var unchangedCPU corev1.Node
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cpuNode.Name}, &unchangedCPU); err != nil {
		t.Fatalf("Get CPU Node: %v", err)
	}
	if _, ok := unchangedCPU.Labels["kueue.azure.com/gpu-series"]; ok {
		t.Fatal("unmatched CPU Node received a GPU-series label")
	}
	if _, ok := unchangedCPU.Labels[labelkeys.LabelGPUClass]; ok {
		t.Fatal("unmatched CPU Node received a GPU-class label")
	}
	if unchangedCPU.Labels[labelkeys.LabelSite] == "" ||
		unchangedCPU.Labels[labelkeys.LabelNetworkDomain] == "" ||
		unchangedCPU.Labels[labelkeys.LabelAcceleratorDomain] == "" {
		t.Fatalf("unmatched CPU Node has incomplete topology labels: %#v", unchangedCPU.Labels)
	}

	recordingClient.mutations = nil
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(recordingClient.mutations) != 0 {
		t.Fatalf("no-op Reconcile() mutations = %v", recordingClient.mutations)
	}
}

func TestTauClusterNoMatchingNodesIsReady(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			ManagementMode: tauv1alpha1.ClusterManagementModeReconcile,
			Nodes: tauv1alpha1.TauClusterNodesSpec{
				LabelRules: []tauv1alpha1.TauNodeLabelRule{{
					Match:  tauv1alpha1.TauNodeMatch{VMSizes: []string{"Standard_ND96isr_H200_v5"}},
					Labels: map[string]string{labelkeys.LabelGPUClass: "h200-141gb"},
				}},
			},
		},
	}
	cpuNode := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:   "system-cpu",
		Labels: map[string]string{azureVMSizeLabel: "Standard_D8ds_v6"},
	}}
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, cpuNode).
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
	nodesReady := findCondition(cluster.Status.Conditions, tauv1alpha1.ConditionNodesReady)
	if nodesReady == nil || nodesReady.Status != metav1.ConditionTrue || nodesReady.Reason != "NoMatchingNodes" {
		t.Fatalf("NodesReady = %#v", nodesReady)
	}
	assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionReady, metav1.ConditionTrue)
	if cluster.Status.Phase != tauv1alpha1.ClusterPhaseReady {
		t.Fatalf("phase = %q, want %q", cluster.Status.Phase, tauv1alpha1.ClusterPhaseReady)
	}
	if cluster.Status.Nodes != (tauv1alpha1.TauClusterSectionStatus{}) {
		t.Fatalf("node status = %#v", cluster.Status.Nodes)
	}
	if got, want := recordingClient.mutations, []string{"patch system-cpu", "create " + tauGPUNodeTopologyName}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CPU-only reconcile mutations = %v, want %v", got, want)
	}
	var gotCPU corev1.Node
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cpuNode.Name}, &gotCPU); err != nil {
		t.Fatalf("Get CPU Node: %v", err)
	}
	if gotCPU.Labels[labelkeys.LabelSite] == "" ||
		gotCPU.Labels[labelkeys.LabelNetworkDomain] == "" ||
		gotCPU.Labels[labelkeys.LabelAcceleratorDomain] == "" ||
		gotCPU.Labels[labelkeys.LabelInfiniband] != "false" {
		t.Fatalf("CPU Node topology labels = %#v", gotCPU.Labels)
	}
}

func TestTauClusterReconcileRejectsConflictingRulesWithoutMatchingNodes(t *testing.T) {
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
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
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

func TestNodeWatchFiltersStatusUpdates(t *testing.T) {
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
	capacityUpdate := statusUpdate.DeepCopy()
	capacityUpdate.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceName(nvidiaGPUResourceName): resource.MustParse("8"),
	}
	if !watch.Update(event.UpdateEvent{ObjectOld: statusUpdate, ObjectNew: capacityUpdate}) {
		t.Fatal("GPU allocatable capacity update must enqueue flavor reconciliation")
	}
	slotCapacityUpdate := statusUpdate.DeepCopy()
	slotCapacityUpdate.Status.Capacity = corev1.ResourceList{
		corev1.ResourceName(torchrunHostSlotResource): resource.MustParse("1"),
	}
	if !watch.Update(event.UpdateEvent{ObjectOld: statusUpdate, ObjectNew: slotCapacityUpdate}) {
		t.Fatal("host-slot capacity update must enqueue topology reconciliation")
	}
	slotAllocatableUpdate := slotCapacityUpdate.DeepCopy()
	slotAllocatableUpdate.Status.Allocatable = corev1.ResourceList{
		corev1.ResourceName(torchrunHostSlotResource): resource.MustParse("1"),
	}
	if !watch.Update(event.UpdateEvent{ObjectOld: slotCapacityUpdate, ObjectNew: slotAllocatableUpdate}) {
		t.Fatal("host-slot allocatable update must enqueue flavor reconciliation")
	}

	labelUpdate := statusUpdate.DeepCopy()
	labelUpdate.Labels["kueue.azure.com/gpu-series"] = "drifted"
	if !watch.Update(event.UpdateEvent{ObjectOld: statusUpdate, ObjectNew: labelUpdate}) {
		t.Fatal("Node label update must enqueue topology reconciliation")
	}
	providerUpdate := statusUpdate.DeepCopy()
	providerUpdate.Spec.ProviderID = "azure:///subscriptions/test"
	if !watch.Update(event.UpdateEvent{ObjectOld: statusUpdate, ObjectNew: providerUpdate}) {
		t.Fatal("Node provider ID update must enqueue topology reconciliation")
	}
	if !watch.Create(event.CreateEvent{Object: original}) {
		t.Fatal("Node creation must enqueue topology reconciliation")
	}
	if !watch.Delete(event.DeleteEvent{Object: original}) {
		t.Fatal("Node deletion must enqueue topology reconciliation")
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
	reconciler := newTestWorkspaceReconciler(c)

	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}}
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
	var clusterQueueBinding rbacv1.ClusterRoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: clusterQueueReaderBindingName("aurora")}, &clusterQueueBinding); err != nil {
		t.Fatalf("ClusterQueue reader ClusterRoleBinding not reconciled: %v", err)
	}
	if clusterQueueBinding.RoleRef.Kind != "ClusterRole" || clusterQueueBinding.RoleRef.Name != clusterQueueReaderRoleName {
		t.Fatalf("ClusterQueue reader roleRef = %#v, want ClusterRole %s", clusterQueueBinding.RoleRef, clusterQueueReaderRoleName)
	}
	if len(clusterQueueBinding.Subjects) != 1 || clusterQueueBinding.Subjects[0].Name != "aurora-researchers" || clusterQueueBinding.Subjects[0].APIGroup != rbacv1.GroupName {
		t.Fatalf("ClusterQueue reader subjects = %#v, want aurora-researchers group", clusterQueueBinding.Subjects)
	}
	var readerRole rbacv1.Role
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.SystemNamespace}, &readerRole); err != nil {
		t.Fatalf("workspace reader role not reconciled: %v", err)
	}
	if len(readerRole.Rules) == 0 || len(readerRole.Rules[0].ResourceNames) != 1 || readerRole.Rules[0].ResourceNames[0] != "aurora" {
		t.Fatalf("reader role rules = %#v, want resourceNames scoped to aurora", readerRole.Rules)
	}
	if len(readerRole.Rules) != 3 || !reflect.DeepEqual(readerRole.Rules[2], rbacv1.PolicyRule{
		APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{"tau-log-connection"}, Verbs: []string{"get"},
	}) {
		t.Fatalf("reader role must grant only named get access to logging metadata: %#v", readerRole.Rules)
	}
	var readerBinding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.SystemNamespace}, &readerBinding); err != nil {
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
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &got); err != nil {
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
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &got); err != nil {
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
	staleReaderRole := testRole(tauv1alpha1.SystemNamespace, "tau-workspace-reader-aurora", "aurora")
	staleReaderBinding := testRoleBinding(tauv1alpha1.SystemNamespace, "tau-workspace-reader-aurora", "aurora")
	staleClusterQueueBinding := testClusterRoleBinding(clusterQueueReaderBindingName("aurora"), "aurora")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(
			workspace,
			testLocalQueue("aurora", "aurora", "aurora-cq"),
			staleBinding,
			staleReaderRole,
			staleReaderBinding,
			staleClusterQueueBinding,
		).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}}

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
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.SystemNamespace}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.SystemNamespace}},
		&rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: clusterQueueReaderBindingName("aurora")}},
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
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	rbacReady := findCondition(got.Status.Conditions, tauv1alpha1.ConditionRBACReady)
	if rbacReady == nil || rbacReady.Status != metav1.ConditionTrue || rbacReady.Reason != "ExistingClusterAuthorization" {
		t.Fatalf("RBACReady = %#v, want ExistingClusterAuthorization=True", rbacReady)
	}
}

func TestWorkspaceRoleAliasPreservesExistingRBAC(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, testLocalQueue("aurora", "aurora", "aurora-cq"), testClusterQueue("aurora-cq")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}
	for range 3 {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
	}
	var beforeBindings rbacv1.RoleBindingList
	var beforeClusterBindings rbacv1.ClusterRoleBindingList
	var beforeRoles rbacv1.RoleList
	for _, list := range []client.ObjectList{&beforeBindings, &beforeClusterBindings, &beforeRoles} {
		if err := c.List(ctx, list); err != nil {
			t.Fatal(err)
		}
	}
	if len(beforeBindings.Items) != 2 || len(beforeClusterBindings.Items) != 1 || len(beforeRoles.Items) != 1 {
		t.Fatal("expected researcher, workspace reader and ClusterQueue reader RBAC")
	}
	for _, role := range []string{"researcher", "tau-researcher-v1"} {
		if err := c.Get(ctx, req.NamespacedName, workspace); err != nil {
			t.Fatal(err)
		}
		workspace.Spec.Role = role
		if err := c.Update(ctx, workspace); err != nil {
			t.Fatal(err)
		}
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatal(err)
		}
		var bindings rbacv1.RoleBindingList
		var clusterBindings rbacv1.ClusterRoleBindingList
		var roles rbacv1.RoleList
		for _, list := range []client.ObjectList{&bindings, &clusterBindings, &roles} {
			if err := c.List(ctx, list); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(beforeBindings.Items, bindings.Items) ||
			!reflect.DeepEqual(beforeClusterBindings.Items, clusterBindings.Items) ||
			!reflect.DeepEqual(beforeRoles.Items, roles.Items) {
			t.Fatalf("switching to role %q changed RBAC objects", role)
		}
		if err := c.Get(ctx, req.NamespacedName, workspace); err != nil {
			t.Fatal(err)
		}
		if workspace.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
			t.Fatalf("role %q: workspace phase = %q", role, workspace.Status.Phase)
		}
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
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}}

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
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &gotWorkspace); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	rbacReady := findCondition(gotWorkspace.Status.Conditions, tauv1alpha1.ConditionRBACReady)
	if rbacReady == nil || rbacReady.Status != metav1.ConditionFalse ||
		rbacReady.Reason != "AuthorizationNotReady" || !strings.Contains(rbacReady.Message, "refusing to delete") {
		t.Fatalf("RBACReady = %#v, want refusal to delete foreign RBAC", rbacReady)
	}
}

func TestCleanupSystemReaderRBACDoesNotDeleteForeignObject(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	foreignRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.SystemNamespace},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreignRole).Build()
	reconciler := newTestWorkspaceReconciler(c)

	err := reconciler.cleanupSystemReaderRBAC(ctx, &tauv1alpha1.TauWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "aurora", UID: types.UID("aurora-uid")},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("cleanup error = %v, want refusal to delete foreign reader Role", err)
	}
	var got rbacv1.Role
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreignRole), &got); err != nil {
		t.Fatalf("foreign reader Role was deleted: %v", err)
	}
}

func TestCleanupClusterQueueReaderRBACDoesNotDeleteForeignObject(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	foreignBinding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: clusterQueueReaderBindingName("aurora")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreignBinding).Build()
	reconciler := newTestWorkspaceReconciler(c)

	err := reconciler.cleanupClusterQueueReaderRBAC(ctx, &tauv1alpha1.TauWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "aurora", UID: types.UID("aurora-uid")},
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete") {
		t.Fatalf("cleanup error = %v, want refusal to delete foreign ClusterRoleBinding", err)
	}
	var got rbacv1.ClusterRoleBinding
	if err := c.Get(ctx, client.ObjectKeyFromObject(foreignBinding), &got); err != nil {
		t.Fatalf("foreign ClusterRoleBinding was deleted: %v", err)
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
		WithObjects(workspace, namespace, localQueue, existingBinding, testClusterQueue("aurora-cq")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

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
	otherWorkspace := testWorkspace("other-workspace")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, foreignBinding, foreignServiceAccount, otherWorkspace, testClusterQueue("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      "aurora",
		Namespace: tauv1alpha1.SystemNamespace,
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
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &gotWorkspace); err != nil {
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
	reconciler := newTestWorkspaceReconciler(c)

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name:      "aurora",
		Namespace: tauv1alpha1.SystemNamespace,
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
	reconciler := newTestWorkspaceReconciler(c)

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
		annotationOwnerUID:    "aurora-uid",
		"preserved":           "true",
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, localQueue, staleRole, staleBinding, staleServiceAccount, staleNamespace, testClusterQueue("aurora-cq")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

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

func TestWorkspaceRevokesObsoletePreviousNamespaceSubjectBeforeTargetReady(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Target.Namespace = "new-aurora"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.KubernetesSubject = &tauv1alpha1.KubernetesSubject{Kind: "Group", Name: "current-researchers"}
	workspace.Status = tauv1alpha1.TauWorkspaceStatus{
		Phase:  tauv1alpha1.WorkspacePhaseReady,
		Target: tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: "old-aurora"},
	}
	oldBinding := testRoleBinding("old-aurora", defaultRoleName, workspace.Name)
	oldBinding.Subjects = []rbacv1.Subject{
		{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "obsolete-researchers"},
		{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "current-researchers"},
	}
	oldNamespace := testNamespace("old-aurora")
	oldNamespace.Labels = workspaceLabels(workspace.Name)
	oldNamespace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, oldNamespace, oldBinding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	if _, err := reconciler.syncWorkspace(ctx, workspace); err != nil {
		t.Fatalf("syncWorkspace() error = %v", err)
	}
	var got rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKeyFromObject(oldBinding), &got); err != nil {
		t.Fatalf("Get previous namespace RoleBinding: %v", err)
	}
	want := []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, oldBinding.Namespace)}
	if !reflect.DeepEqual(got.Subjects, want) {
		t.Fatalf("previous namespace subjects = %#v, want %#v", got.Subjects, want)
	}
}

func TestWorkspaceRevokesObsoletePreviousNamespaceIdentityBeforeTargetReady(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.Target.Namespace = "new-aurora"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.WorkloadIdentity.ServiceAccountName = "tau-workload-v2"
	workspace.Status.Target.ResolvedNamespace = "old-aurora"
	current := testServiceAccount("old-aurora", "tau-workload-v2", workspace.Name)
	current.Annotations[annotationOwnerUID] = string(workspace.UID)
	current.Annotations[annotationAzureWIClientID] = workspace.Spec.WorkloadIdentity.ClientID
	obsolete := testServiceAccount("old-aurora", "tau-workload", workspace.Name)
	obsolete.Annotations[annotationOwnerUID] = string(workspace.UID)
	oldNamespace := testNamespace("old-aurora")
	oldNamespace.Labels = workspaceLabels(workspace.Name)
	oldNamespace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, oldNamespace, current, obsolete).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	if _, err := reconciler.syncWorkspace(ctx, workspace); err != nil {
		t.Fatalf("syncWorkspace() error = %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obsolete), &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete previous namespace ServiceAccount survived: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(current), &corev1.ServiceAccount{}); err != nil {
		t.Fatalf("current previous namespace ServiceAccount was removed: %v", err)
	}
}

func TestWorkspaceRevokesPreviousNamespaceIdentityWhenClientIDChanges(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.Target.Namespace = "new-aurora"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.WorkloadIdentity.ClientID = "new-client-id"
	workspace.Status.Target.ResolvedNamespace = "old-aurora"
	obsolete := testServiceAccount("old-aurora", workspace.Spec.WorkloadIdentity.ServiceAccountName, workspace.Name)
	obsolete.Annotations[annotationOwnerUID] = string(workspace.UID)
	obsolete.Annotations[annotationAzureWIClientID] = "old-client-id"
	oldNamespace := testNamespace("old-aurora")
	oldNamespace.Labels = workspaceLabels(workspace.Name)
	oldNamespace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, oldNamespace, obsolete).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	if _, err := reconciler.syncWorkspace(ctx, workspace); err != nil {
		t.Fatalf("syncWorkspace() error = %v", err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(obsolete), &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete previous namespace identity binding survived: %v", err)
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
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var current tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &current); err != nil {
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

	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &current); err != nil {
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
	readerRole := testRole(tauv1alpha1.SystemNamespace, "tau-workspace-reader-aurora", "aurora")
	readerBinding := testRoleBinding(tauv1alpha1.SystemNamespace, "tau-workspace-reader-aurora", "aurora")
	clusterQueueBinding := testClusterRoleBinding(clusterQueueReaderBindingName("aurora"), "aurora")
	localQueue := testLocalQueue("aurora", "aurora", "aurora")
	localQueue.SetLabels(workspaceLabels("aurora"))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, targetNamespace, targetRole, targetBinding, targetServiceAccount, readerRole, readerBinding, clusterQueueBinding, localQueue).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}}); err != nil {
		t.Fatalf("Reconcile delete error = %v", err)
	}
	for _, key := range []client.ObjectKey{
		{Name: defaultRoleName, Namespace: "aurora"},
		{Name: "tau-workload", Namespace: "aurora"},
		{Name: "tau-workspace-reader-aurora", Namespace: tauv1alpha1.SystemNamespace},
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
	if err := c.Get(ctx, client.ObjectKey{Name: clusterQueueReaderBindingName("aurora")}, &rbacv1.ClusterRoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("expected ClusterQueue reader ClusterRoleBinding deleted, got err=%v", err)
	}
	deletedLocalQueue := &unstructured.Unstructured{}
	deletedLocalQueue.SetGroupVersionKind(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: "aurora"}, deletedLocalQueue); !apierrors.IsNotFound(err) {
		t.Fatalf("expected controller-owned LocalQueue deleted, got err=%v", err)
	}
}

func TestWorkspaceDeleteWaitsForClusterQueueFinalizer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Finalizers = []string{workspaceFinalizer}
	now := metav1.Now()
	workspace.DeletionTimestamp = &now
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		ClusterQueue:    workspaceClusterQueueName(workspace.Name),
		ClusterQueueUID: "cluster-queue-uid",
	}
	queue := newQueueObject(clusterQueueGVK)
	queue.SetName(workspaceClusterQueueName(workspace.Name))
	queue.SetUID(types.UID("cluster-queue-uid"))
	queue.SetLabels(workspaceLabels(workspace.Name))
	queue.SetAnnotations(map[string]string{annotationOwnerUID: string(workspace.UID)})
	queue.SetFinalizers([]string{"kueue.x-k8s.io/resource-in-use"})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, queue).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
	if err == nil || !strings.Contains(err.Error(), "waiting for ClusterQueue") {
		t.Fatalf("cleanup error = %v, want pending deletion", err)
	}
	var remainingWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &remainingWorkspace); err != nil {
		t.Fatalf("workspace disappeared before ClusterQueue deletion completed: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&remainingWorkspace, workspaceFinalizer) {
		t.Fatal("workspace cleanup finalizer was removed before ClusterQueue deletion completed")
	}
	remainingQueue := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(queue), remainingQueue); err != nil {
		t.Fatalf("ClusterQueue disappeared despite resource-in-use finalizer: %v", err)
	}
	if remainingQueue.GetDeletionTimestamp() == nil {
		t.Fatal("ClusterQueue deletion was not requested")
	}
}

func TestWorkspaceRefusesReservedTargetNamespace(t *testing.T) {
	for _, reserved := range []string{
		tauv1alpha1.SystemNamespace,
		tauv1alpha1.LegacySystemNamespace,
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
			reconciler := newTestWorkspaceReconciler(c)

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
			if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &updated); err != nil {
				t.Fatalf("Get workspace: %v", err)
			}
			if updated.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
				t.Fatalf("workspace phase = %q, want Degraded", updated.Status.Phase)
			}
		})
	}
}

func TestWorkspaceRetargetDoesNotStripReservedNamespaceMetadata(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Target.Namespace = "aurora"
	workspace.Status.Target.ResolvedNamespace = tauv1alpha1.SystemNamespace
	platform := testNamespace(tauv1alpha1.SystemNamespace)
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
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: tauv1alpha1.SystemNamespace}, &got); err != nil {
		t.Fatalf("Get system namespace: %v", err)
	}
	if got.Labels[labelKueueDefaultLocalQueue] != "platform-queue" {
		t.Fatalf("system namespace lost its Kueue default queue label: %#v", got.Labels)
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
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &got); err != nil {
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
	reconciler := newTestWorkspaceReconciler(c)

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
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	if got.Status.Queue.LocalQueue != "jobqueue" || got.Status.Queue.ClusterQueue != "jobqueue" {
		t.Fatalf("queue status = %#v, want jobqueue -> jobqueue", got.Status.Queue)
	}
}

func TestMultipleWorkspacesReconcileIndependently(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	first := testWorkspace("research")
	first.Spec.Queue = "jobqueue"
	second := testWorkspace("evaluation")
	second.Spec.Queue = "jobqueue"
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(first, second, testClusterQueue("jobqueue")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	for _, name := range []string{"research", "evaluation"} {
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: tauv1alpha1.SystemNamespace}}
		for i := 0; i < 2; i++ {
			if _, err := reconciler.Reconcile(ctx, req); err != nil {
				t.Fatalf("reconcile %s iteration %d: %v", name, i, err)
			}
		}
		var namespace corev1.Namespace
		if err := c.Get(ctx, client.ObjectKey{Name: name}, &namespace); err != nil {
			t.Fatalf("workspace %s namespace: %v", name, err)
		}
		var workspace tauv1alpha1.TauWorkspace
		if err := c.Get(ctx, req.NamespacedName, &workspace); err != nil {
			t.Fatalf("Get workspace %s: %v", name, err)
		}
		if workspace.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
			t.Fatalf("workspace %s phase = %q, want Ready", name, workspace.Status.Phase)
		}
	}
}

func TestLegacyWorkspaceUpgradeMigratesUIDOwnership(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	namespace := testNamespace("aurora")
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = map[string]string{annotationResultScope: workspace.Spec.Defaults.OutputRoot}
	localQueue := testLocalQueue("aurora", "aurora", "aurora")
	localQueue.SetLabels(workspaceLabels("aurora"))
	localQueue.SetAnnotations(nil)
	binding := testRoleBinding("aurora", defaultRoleName, "aurora")
	binding.Annotations = nil
	account := testServiceAccount("aurora", "tau-workload", "aurora")
	account.Annotations = map[string]string{annotationAzureWIClientID: workspace.Spec.WorkloadIdentity.ClientID}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, localQueue, binding, account, testClusterQueue("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspaceTimes(t, reconciler, ctx, workspace.Name, 5)

	for name, obj := range map[string]client.Object{
		"workspace":       &tauv1alpha1.TauWorkspace{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace}},
		"namespace":       &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace.Name}},
		"role binding":    &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: binding.Name, Namespace: binding.Namespace}},
		"service account": &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: account.Name, Namespace: account.Namespace}},
	} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("Get migrated %s: %v", name, err)
		}
		if got := obj.GetAnnotations()[annotationOwnerUID]; got != string(workspace.UID) {
			t.Fatalf("%s owner UID = %q, want %q", name, got, workspace.UID)
		}
	}
	gotQueue := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(localQueue), gotQueue); err != nil {
		t.Fatalf("Get migrated LocalQueue: %v", err)
	}
	if got := gotQueue.GetAnnotations()[annotationOwnerUID]; got != string(workspace.UID) {
		t.Fatalf("LocalQueue owner UID = %q, want %q", got, workspace.UID)
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get migrated workspace status: %v", err)
	}
	if gotWorkspace.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("workspace phase = %q, want Ready; conditions=%#v", gotWorkspace.Status.Phase, gotWorkspace.Status.Conditions)
	}
}

func TestLegacyWorkspaceFinalizerMigratesBeforeCleanup(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.DeletionTimestamp = &now
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	namespace := testNamespace("aurora")
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = nil
	binding := testRoleBinding("aurora", defaultRoleName, "aurora")
	binding.Annotations = nil
	localQueue := testLocalQueue("aurora", "aurora", "aurora")
	localQueue.SetLabels(workspaceLabels("aurora"))
	localQueue.SetAnnotations(nil)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, binding, localQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile deleting legacy workspace iteration %d: %v", i, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy RoleBinding survived finalizer cleanup: %v", err)
	}
	gotQueue := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(localQueue), gotQueue); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy LocalQueue survived finalizer cleanup: %v", err)
	}
}

func TestLegacyAdoptedWorkspaceUpgradeMigratesUIDOwnership(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Spec.Target.CreateNamespace = false
	namespace := testNamespace("aurora")
	namespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    workspace.Spec.Queue,
		labelKueueDefaultLocalQueue: workspace.Spec.Queue,
	}
	namespace.Annotations = map[string]string{annotationResultScope: workspace.Spec.Defaults.OutputRoot}
	localQueue := testLocalQueue("aurora", "aurora", "aurora")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, localQueue, testClusterQueue("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspaceTimes(t, reconciler, ctx, workspace.Name, 5)

	for name, obj := range map[string]client.Object{
		"workspace": &tauv1alpha1.TauWorkspace{ObjectMeta: metav1.ObjectMeta{Name: workspace.Name, Namespace: workspace.Namespace}},
		"namespace": &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace.Name}},
	} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("Get migrated %s: %v", name, err)
		}
		if got := obj.GetAnnotations()[annotationOwnerUID]; got != string(workspace.UID) {
			t.Fatalf("%s owner UID = %q, want %q", name, got, workspace.UID)
		}
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get migrated workspace status: %v", err)
	}
	if gotWorkspace.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("workspace phase = %q, want Ready; conditions=%#v", gotWorkspace.Status.Phase, gotWorkspace.Status.Conditions)
	}
}

func TestLegacyAdoptedWorkspaceMigrationRejectsQueueMismatch(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Spec.Target.CreateNamespace = false
	namespace := testNamespace("aurora")
	namespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    "old-queue",
		labelKueueDefaultLocalQueue: "different-queue",
	}
	namespace.Annotations = nil
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, workspace.Name)

	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if _, exists := gotWorkspace.Annotations[annotationOwnerUID]; exists {
		t.Fatalf("queue-mismatched workspace was migrated: %#v", gotWorkspace.Annotations)
	}
	var gotNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), &gotNamespace); err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if _, exists := gotNamespace.Annotations[annotationOwnerUID]; exists {
		t.Fatalf("queue-mismatched namespace was migrated: %#v", gotNamespace.Annotations)
	}
}

func TestLegacyAdoptedWorkspaceMigratesBeforePendingQueueChange(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.Queue = "new-queue"
	namespace := testNamespace("aurora")
	namespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    "old-queue",
		labelKueueDefaultLocalQueue: "old-queue",
	}
	namespace.Annotations = nil
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, testClusterQueue("new-queue")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspaceTimes(t, reconciler, ctx, workspace.Name, 5)

	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get migrated workspace: %v", err)
	}
	if gotWorkspace.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("workspace owner UID = %q, want %q", gotWorkspace.Annotations[annotationOwnerUID], workspace.UID)
	}
	var gotNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), &gotNamespace); err != nil {
		t.Fatalf("Get migrated Namespace: %v", err)
	}
	if gotNamespace.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("Namespace owner UID = %q, want %q", gotNamespace.Annotations[annotationOwnerUID], workspace.UID)
	}
	if gotNamespace.Labels[labelWorkspaceLocalQueue] != "new-queue" ||
		gotNamespace.Labels[labelKueueDefaultLocalQueue] != "new-queue" {
		t.Fatalf("Namespace queue labels = %#v, want new-queue", gotNamespace.Labels)
	}
}

func TestLegacyAdoptedWorkspaceMigratesAfterCreateNamespaceChange(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Spec.Target.CreateNamespace = true
	namespace := testNamespace("aurora")
	namespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    workspace.Spec.Queue,
		labelKueueDefaultLocalQueue: workspace.Spec.Queue,
	}
	namespace.Annotations = nil
	binding := testRoleBinding("aurora", defaultRoleName, workspace.Name)
	binding.Annotations = nil
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, binding, testClusterQueue(workspace.Spec.Queue)).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	reconcileWorkspaceTimes(t, reconciler, ctx, workspace.Name, 4)

	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, req.NamespacedName, &gotWorkspace); err != nil {
		t.Fatalf("Get migrated workspace: %v", err)
	}
	if gotWorkspace.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("workspace owner UID = %q, want %q", gotWorkspace.Annotations[annotationOwnerUID], workspace.UID)
	}
	var gotNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), &gotNamespace); err != nil {
		t.Fatalf("Get migrated Namespace: %v", err)
	}
	if gotNamespace.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("Namespace owner UID = %q, want %q", gotNamespace.Annotations[annotationOwnerUID], workspace.UID)
	}

	now := metav1.Now()
	gotWorkspace.DeletionTimestamp = &now
	deletionClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&gotWorkspace, &gotNamespace, binding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	deletionReconciler := newTestWorkspaceReconciler(deletionClient)
	if _, err := deletionReconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile migrated workspace deletion: %v", err)
	}
	if err := deletionClient.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("UID-less legacy RoleBinding survived deletion: %v", err)
	}
	if err := deletionClient.Get(ctx, req.NamespacedName, &tauv1alpha1.TauWorkspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace finalizer was not removed: %v", err)
	}
}

func TestLegacyWorkspaceFinalizerMigratesHelpersWhenNamespaceIsGone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.DeletionTimestamp = &now
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Spec.Target.CreateNamespace = false
	binding := testRoleBinding("aurora", defaultRoleName, "aurora")
	binding.Annotations = nil
	localQueue := testLocalQueue("aurora", "aurora", "aurora")
	localQueue.SetLabels(workspaceLabels("aurora"))
	localQueue.SetAnnotations(nil)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, binding, localQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile deleting legacy workspace iteration %d: %v", i, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy RoleBinding survived finalizer cleanup: %v", err)
	}
	gotQueue := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(localQueue), gotQueue); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy LocalQueue survived finalizer cleanup: %v", err)
	}
}

func TestLegacyMigrationPreservesHelpersUntilNamespaceOwnershipIsVisible(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	namespace := testNamespace("aurora")
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = nil
	binding := testRoleBinding("aurora", defaultRoleName, "aurora")
	binding.Annotations = nil
	account := testServiceAccount("aurora", "tau-workload", "aurora")
	account.Annotations = map[string]string{annotationAzureWIClientID: workspace.Spec.WorkloadIdentity.ClientID}
	localQueue := testLocalQueue("aurora", "aurora", "aurora")
	localQueue.SetLabels(workspaceLabels("aurora"))
	localQueue.SetAnnotations(nil)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, binding, account, localQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	recordingClient := &resourceMutationRecordingClient{Client: c}
	reconciler := newTestWorkspaceReconciler(recordingClient)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile legacy migration: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("migration result = %#v, want immediate requeue", result)
	}
	for _, obj := range []client.Object{binding, account, localQueue} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("migration deleted helper %T %s: %v", obj, client.ObjectKeyFromObject(obj), err)
		}
	}
	for _, mutation := range recordingClient.mutations {
		if strings.HasPrefix(mutation, "delete ") {
			t.Fatalf("migration deleted helper before ownership became visible: %v", recordingClient.mutations)
		}
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if _, exists := gotWorkspace.Annotations[annotationOwnerUID]; exists {
		t.Fatalf("workspace ownership advanced before Namespace visibility: %#v", gotWorkspace.Annotations)
	}
}

func TestLegacyMigrationPreservesHelpersWhenWorkspaceCacheLeadsNamespaceCache(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{
		annotationV0Primary: "true",
		annotationOwnerUID:  string(workspace.UID),
	}
	cachedNamespace := testNamespace("aurora")
	cachedNamespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	cachedNamespace.Annotations = nil
	authoritativeNamespace := cachedNamespace.DeepCopy()
	setOwnerUIDAnnotation(authoritativeNamespace, workspace.UID)
	binding := testRoleBinding("aurora", defaultRoleName, "aurora")
	binding.Annotations = nil
	account := testServiceAccount("aurora", "tau-workload", "aurora")
	account.Annotations = map[string]string{annotationAzureWIClientID: workspace.Spec.WorkloadIdentity.ClientID}
	localQueue := testLocalQueue("aurora", "aurora", "aurora")
	localQueue.SetLabels(workspaceLabels("aurora"))
	localQueue.SetAnnotations(nil)
	cache := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, cachedNamespace, binding, account, localQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	api := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace.DeepCopy(), authoritativeNamespace).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	recordingClient := &resourceMutationRecordingClient{Client: cache}
	reconciler := newTestWorkspaceReconciler(recordingClient)
	reconciler.APIReader = api
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	result, err := reconciler.Reconcile(ctx, req)
	if err != nil {
		t.Fatalf("Reconcile cache-skewed legacy migration: %v", err)
	}
	if !result.Requeue {
		t.Fatalf("cache-skewed migration result = %#v, want immediate requeue", result)
	}
	for _, obj := range []client.Object{binding, account, localQueue} {
		if err := cache.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			t.Fatalf("cache-skewed migration deleted helper %T %s: %v", obj, client.ObjectKeyFromObject(obj), err)
		}
	}
	for _, mutation := range recordingClient.mutations {
		if strings.HasPrefix(mutation, "delete ") {
			t.Fatalf("cache-skewed migration deleted helper before Namespace ownership was cached: %v", recordingClient.mutations)
		}
	}
}

func TestCompletedLegacyMigrationDoesNotBlockExplicitRetargetAdoption(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Spec.Target.Namespace = "new-target"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Annotations = map[string]string{
		annotationV0Primary:            "true",
		annotationOwnerUID:             string(workspace.UID),
		annotationAdoptionNamespaceUID: "new-target-uid",
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "new-target",
		UID:  types.UID("new-target-uid"),
	}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, testClusterQueue(workspace.Spec.Queue)).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspaceTimes(t, reconciler, ctx, workspace.Name, 4)

	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get retargeted workspace: %v", err)
	}
	if gotWorkspace.Annotations[annotationV0Primary] != "true" {
		t.Fatalf("retargeted workspace lost v0 primary marker: %#v", gotWorkspace.Annotations)
	}
	var gotNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), &gotNamespace); err != nil {
		t.Fatalf("Get adopted target Namespace: %v", err)
	}
	if gotNamespace.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("retargeted Namespace owner UID = %q, want %q", gotNamespace.Annotations[annotationOwnerUID], workspace.UID)
	}
}

func TestLegacyAdoptedWorkspaceWithoutQueueUsesClusterDefaultForMigration(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.Queue = ""
	namespace := testNamespace("aurora")
	namespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    "jobqueue",
		labelKueueDefaultLocalQueue: "jobqueue",
	}
	namespace.Annotations = nil
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, cluster, testLocalQueue("aurora", "jobqueue", "jobqueue"), testClusterQueue("jobqueue")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspaceTimes(t, reconciler, ctx, workspace.Name, 5)

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Spec.Queue != "" {
		t.Fatalf("spec.queue = %q, want stored spec unchanged", got.Spec.Queue)
	}
	if got.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("workspace owner UID = %q, want %q", got.Annotations[annotationOwnerUID], workspace.UID)
	}
}

func TestLegacyWorkspaceDeletionWithoutQueueUsesClusterDefaultForMigration(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.DeletionTimestamp = &now
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.Queue = ""
	binding := testRoleBinding("aurora", defaultRoleName, workspace.Name)
	binding.Annotations = nil
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, binding, cluster).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile deleting queue-defaulted workspace iteration %d: %v", i, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy RoleBinding survived queue-defaulted finalizer cleanup: %v", err)
	}
}

func TestMigratedWorkspaceDeletionWithoutQueueDoesNotRequireClusterDefault(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.DeletionTimestamp = &now
	workspace.Spec.Queue = ""
	workspace.Annotations = map[string]string{
		annotationV0Primary: "true",
		annotationOwnerUID:  string(workspace.UID),
	}
	binding := testRoleBinding("aurora", defaultRoleName, workspace.Name)
	setOwnerUIDAnnotation(binding, workspace.UID)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, binding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile migrated workspace without cluster default iteration %d: %v", i, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned RoleBinding survived deletion without cluster default: %v", err)
	}
}

func TestUnmigratedWorkspaceDeletionWithoutQueueOrClusterWhenNamespaceIsGone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.DeletionTimestamp = &now
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.Queue = ""
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	binding := testRoleBinding("aurora", defaultRoleName, workspace.Name)
	binding.Annotations = nil
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, binding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile unmigrated workspace without Namespace or cluster iteration %d: %v", i, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy RoleBinding survived deletion without Namespace or cluster: %v", err)
	}
}

func TestUnmigratedAdoptedWorkspaceDeletionWithoutQueueOrCluster(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.DeletionTimestamp = &now
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.Queue = ""
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	namespace := testNamespace("aurora")
	namespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    "jobqueue",
		labelKueueDefaultLocalQueue: "jobqueue",
	}
	namespace.Annotations = nil
	binding := testRoleBinding("aurora", defaultRoleName, workspace.Name)
	binding.Annotations = nil
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, binding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile unmigrated adopted workspace without cluster iteration %d: %v", i, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy RoleBinding survived adopted Namespace deletion without cluster: %v", err)
	}
}

func TestUnmigratedWorkspaceDeletionDuringRetargetUsesPreviousNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.DeletionTimestamp = &now
	workspace.Spec.Target.Namespace = "new-target"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Status.Target.ResolvedNamespace = "old-target"
	oldNamespace := testNamespace("old-target")
	oldNamespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    workspace.Spec.Queue,
		labelKueueDefaultLocalQueue: workspace.Spec.Queue,
	}
	oldNamespace.Annotations = nil
	newNamespace := testNamespace("new-target")
	newNamespace.Labels = nil
	newNamespace.Annotations = nil
	binding := testRoleBinding("old-target", defaultRoleName, workspace.Name)
	binding.Annotations = nil
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, oldNamespace, newNamespace, binding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile unmigrated retarget deletion iteration %d: %v", i, err)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy RoleBinding survived retarget deletion: %v", err)
	}
	var gotNew corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(newNamespace), &gotNew); err != nil {
		t.Fatalf("Get new desired Namespace: %v", err)
	}
	if gotNew.Annotations[annotationOwnerUID] != "" || gotNew.Labels[labelWorkspace] != "" {
		t.Fatalf("retarget deletion mutated new desired Namespace: labels=%#v annotations=%#v", gotNew.Labels, gotNew.Annotations)
	}
}

func TestUnmigratedWorkspaceRetargetMigratesHistoryBeforeDeletion(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Spec.Target.Namespace = "new-target"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Status.Target.ResolvedNamespace = "old-target"
	oldNamespace := testNamespace("old-target")
	oldNamespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    workspace.Spec.Queue,
		labelKueueDefaultLocalQueue: workspace.Spec.Queue,
	}
	oldNamespace.Annotations = nil
	newNamespace := testNamespace("new-target")
	newNamespace.Labels = map[string]string{labelWorkspace: workspace.Name}
	newNamespace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	binding := testRoleBinding("old-target", defaultRoleName, workspace.Name)
	binding.Annotations = nil
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, oldNamespace, newNamespace, binding, testClusterQueue(workspace.Spec.Queue)).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile normal retarget iteration %d: %v", i, err)
		}
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get retargeted workspace: %v", err)
	}
	if gotWorkspace.Status.Target.ResolvedNamespace != "new-target" {
		t.Fatalf("resolved namespace = %q, want new-target", gotWorkspace.Status.Target.ResolvedNamespace)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy RoleBinding survived normal retarget reconciliation: %v", err)
	}
	var gotOld corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(oldNamespace), &gotOld); err != nil {
		t.Fatalf("Get historical Namespace: %v", err)
	}
	var gotNew corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(newNamespace), &gotNew); err != nil {
		t.Fatalf("Get new desired Namespace: %v", err)
	}
	now := metav1.Now()
	gotWorkspace.DeletionTimestamp = &now
	deletionClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&gotWorkspace, &gotOld, &gotNew).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	deletionReconciler := newTestWorkspaceReconciler(deletionClient)
	if _, err := deletionReconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile retargeted workspace deletion: %v", err)
	}
	if err := deletionClient.Get(ctx, client.ObjectKeyFromObject(newNamespace), &gotNew); err != nil {
		t.Fatalf("Get new desired Namespace: %v", err)
	}
	if gotNew.Annotations[annotationOwnerUID] != string(workspace.UID) ||
		gotNew.Labels[labelWorkspace] != workspace.Name {
		t.Fatalf("normal retarget deletion did not preserve adopted Namespace ownership: labels=%#v annotations=%#v", gotNew.Labels, gotNew.Annotations)
	}
}

func TestUnmigratedWorkspaceWithoutDefaultMigratesHistoryBeforeDeletion(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Spec.Queue = ""
	workspace.Spec.Target.Namespace = "new-target"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Status.Target.ResolvedNamespace = "old-target"
	oldNamespace := testNamespace("old-target")
	oldNamespace.Labels = map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    "historical-queue",
		labelKueueDefaultLocalQueue: "historical-queue",
	}
	oldNamespace.Annotations = nil
	newNamespace := testNamespace("new-target")
	newNamespace.Labels = nil
	newNamespace.Annotations = nil
	binding := testRoleBinding("old-target", defaultRoleName, workspace.Name)
	binding.Annotations = nil
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, oldNamespace, newNamespace, binding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile unresolved default iteration %d: %v", i, err)
		}
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get migrated workspace: %v", err)
	}
	if gotWorkspace.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("workspace owner UID = %q, want %q", gotWorkspace.Annotations[annotationOwnerUID], workspace.UID)
	}
	if gotWorkspace.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
		t.Fatalf("workspace phase = %q, want Degraded for unavailable default", gotWorkspace.Status.Phase)
	}
	var gotOld corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(oldNamespace), &gotOld); err != nil {
		t.Fatalf("Get historical Namespace: %v", err)
	}
	if gotOld.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("historical Namespace owner UID = %q, want %q", gotOld.Annotations[annotationOwnerUID], workspace.UID)
	}
	var gotNew corev1.Namespace
	if err := c.Get(ctx, client.ObjectKeyFromObject(newNamespace), &gotNew); err != nil {
		t.Fatalf("Get desired Namespace: %v", err)
	}
	if gotNew.Annotations[annotationOwnerUID] != "" || gotNew.Labels[labelWorkspace] != "" {
		t.Fatalf("unresolved default mutated desired Namespace: labels=%#v annotations=%#v", gotNew.Labels, gotNew.Annotations)
	}

	now := metav1.Now()
	gotWorkspace.DeletionTimestamp = &now
	deletionClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(&gotWorkspace, &gotOld, &gotNew, binding).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	deletionReconciler := newTestWorkspaceReconciler(deletionClient)
	if _, err := deletionReconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile deletion after unresolved default: %v", err)
	}
	if err := deletionClient.Get(ctx, client.ObjectKeyFromObject(binding), &rbacv1.RoleBinding{}); !apierrors.IsNotFound(err) {
		t.Fatalf("UID-less historical RoleBinding survived deletion: %v", err)
	}
	if err := deletionClient.Get(ctx, client.ObjectKeyFromObject(newNamespace), &gotNew); err != nil {
		t.Fatalf("Get desired Namespace after deletion: %v", err)
	}
	if gotNew.Annotations[annotationOwnerUID] != "" || gotNew.Labels[labelWorkspace] != "" {
		t.Fatalf("deletion mutated desired Namespace: labels=%#v annotations=%#v", gotNew.Labels, gotNew.Annotations)
	}
}

func TestLegacyWorkspaceRoleCleanupDoesNotRequireRoleUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	role := testRole("old-aurora", defaultRoleName, workspace.Name)
	role.UID = types.UID("legacy-role-uid")
	role.Annotations = nil
	baseClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(role).Build()
	c := &roleUpdateDenyingClient{Client: baseClient}
	reconciler := newTestWorkspaceReconciler(c)

	if err := reconciler.cleanupStaleTargetRBAC(ctx, workspace, "", "", false); err != nil {
		t.Fatalf("cleanup legacy Role with shipped permissions: %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKeyFromObject(role), &rbacv1.Role{}); !apierrors.IsNotFound(err) {
		t.Fatalf("legacy Role survived cleanup: %v", err)
	}
	if c.roleUpdates != 0 {
		t.Fatalf("legacy Role cleanup attempted %d forbidden updates", c.roleUpdates)
	}
}

func TestTeamWorkspaceStagesManagedQueueWithoutMutatingExistingQueue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	existing := testLocalQueue("aurora", "aurora", "external-cq")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, team, flavor, existing, testClusterQueue("external-cq")).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	status, ready, message, err := reconciler.reconcileQueue(ctx, workspace, "aurora")
	if err != nil || !ready {
		t.Fatalf("reconcileQueue() status=%#v ready=%v message=%q err=%v, want existing compatible queue preserved", status, ready, message, err)
	}
	if status.LocalQueue != workspaceClusterQueueName(workspace.Name) ||
		status.ClusterQueue != workspaceClusterQueueName(workspace.Name) {
		t.Fatalf("managed queue status = %#v", status)
	}
	gotExisting := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(existing), gotExisting); err != nil {
		t.Fatalf("Get existing LocalQueue: %v", err)
	}
	if target, _, _ := unstructured.NestedString(gotExisting.Object, "spec", "clusterQueue"); target != "external-cq" {
		t.Fatalf("existing LocalQueue target = %q, want external-cq", target)
	}
	gotManaged := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: workspaceLocalQueueName(workspace), Namespace: "aurora"}, gotManaged); err != nil {
		t.Fatalf("Get managed LocalQueue: %v", err)
	}
}

func TestTeamWorkspaceQueueConflictFailsClosed(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: workspace.Name,
		Labels: map[string]string{
			labelWorkspace:              workspace.Name,
			labelWorkspaceLocalQueue:    workspace.Spec.Queue,
			labelKueueDefaultLocalQueue: workspace.Spec.Queue,
		},
		Annotations: map[string]string{annotationOwnerUID: string(workspace.UID)},
	}}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	foreign := testLocalQueue(workspace.Name, workspaceLocalQueueName(workspace), "external-cq")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, team, flavor, foreign, testClusterQueue(workspace.Spec.Queue), testClusterQueue("external-cq")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	if _, err := reconciler.syncWorkspace(ctx, workspace); err != nil {
		t.Fatalf("syncWorkspace() error = %v", err)
	}
	var gotNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace.Name}, &gotNamespace); err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got := gotNamespace.Labels[labelKueueDefaultLocalQueue]; got != workspace.Spec.Queue {
		t.Fatalf("namespace default queue = %q, want unchanged %q", got, workspace.Spec.Queue)
	}
	var gotWorkspace tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &gotWorkspace); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if gotWorkspace.Status.Queue.LocalQueue != workspaceLocalQueueName(workspace) ||
		gotWorkspace.Status.Queue.ClusterQueue != "external-cq" {
		t.Fatalf("status.queue = %#v, want desired managed LocalQueue and conflicting ClusterQueue", gotWorkspace.Status.Queue)
	}
	assertCondition(t, gotWorkspace.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionFalse)
}

func TestTeamWorkspaceCutsOverToManagedQueue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.Queue = "jobqueue"
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseReady
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      workspace.Spec.Queue,
		ClusterQueue:    workspace.Spec.Queue,
		ClusterQueueUID: "legacy-cluster-queue-uid",
	}
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	previousQueue := testLocalQueue(workspace.Name, workspace.Spec.Queue, workspace.Spec.Queue)
	previousQueue.SetLabels(workspaceLabels(workspace.Name))
	setOwnerUIDAnnotation(previousQueue, workspace.UID)
	workload := newQueueObject(schema.GroupVersionKind{
		Group: "kueue.x-k8s.io", Version: "v1beta1", Kind: "Workload",
	})
	workload.SetName("running")
	workload.SetNamespace(workspace.Name)
	workload.Object["spec"] = map[string]any{"queueName": workspace.Spec.Queue}
	suspendedJob := newQueueObject(schema.GroupVersionKind{Group: "batch", Version: "v1", Kind: "Job"})
	suspendedJob.SetName("accepted-suspended")
	suspendedJob.SetNamespace(workspace.Name)
	suspendedJob.SetLabels(map[string]string{"kueue.x-k8s.io/queue-name": workspace.Spec.Queue})
	suspendedJob.Object["spec"] = map[string]any{"suspend": true}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        workspace.Name,
		Labels:      workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue),
		Annotations: map[string]string{annotationOwnerUID: string(workspace.UID)},
	}}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, team, flavor, namespace, previousQueue, workload, suspendedJob, clusterQueueWithUID(workspace.Spec.Queue, "legacy-cluster-queue-uid")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	result, err := reconciler.syncWorkspace(ctx, workspace)
	if err != nil {
		t.Fatalf("sync workspace before drain: %v", err)
	}
	var gotNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: workspace.Name}, &gotNamespace); err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got := gotNamespace.Labels[labelKueueDefaultLocalQueue]; got != workspaceLocalQueueName(workspace) {
		t.Fatalf("namespace default queue = %q, want managed route %q", got, workspaceLocalQueueName(workspace))
	}
	compatibleQueue := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(previousQueue), compatibleQueue); err != nil {
		t.Fatalf("compatible LocalQueue disappeared during cutover: %v", err)
	}
	if target, _, _ := unstructured.NestedString(compatibleQueue.Object, "spec", "clusterQueue"); target != workspace.Spec.Queue {
		t.Fatalf("compatible LocalQueue target = %q, want preserved target %q", target, workspace.Spec.Queue)
	}
	var waiting tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &waiting); err != nil {
		t.Fatalf("Get waiting workspace: %v", err)
	}
	assertCondition(t, waiting.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionTrue)
	if waiting.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready after compatible cutover", waiting.Status.Phase)
	}
	if waiting.Status.Queue.LocalQueue != workspace.Spec.Queue ||
		waiting.Status.Queue.ClusterQueue != workspace.Spec.Queue ||
		waiting.Status.Queue.ClusterQueueUID != "legacy-cluster-queue-uid" {
		t.Fatalf("status.queue = %#v, want complete legacy client route", waiting.Status.Queue)
	}
	if result.RequeueAfter != readyRequeue {
		t.Fatalf("RequeueAfter = %v, want %v after compatible cutover", result.RequeueAfter, readyRequeue)
	}

	workload.Object["status"] = map[string]any{"conditions": []any{
		map[string]any{"type": "Finished", "status": "True"},
	}}
	if err := c.Update(ctx, workload); err != nil {
		t.Fatalf("finish workload: %v", err)
	}
	if _, err := reconciler.syncWorkspace(ctx, &waiting); err != nil {
		t.Fatalf("sync workspace after drain: %v", err)
	}
	if err := c.Get(ctx, client.ObjectKey{Name: workspace.Name}, &gotNamespace); err != nil {
		t.Fatalf("Get activated namespace: %v", err)
	}
	if got := gotNamespace.Labels[labelKueueDefaultLocalQueue]; got != workspaceLocalQueueName(workspace) {
		t.Fatalf("namespace default queue = %q, want %q", got, workspaceLocalQueueName(workspace))
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(previousQueue), compatibleQueue); err != nil {
		t.Fatalf("compatible LocalQueue disappeared after workload drain: %v", err)
	}
	gotWorkload := newQueueObject(workload.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(workload), gotWorkload); err != nil {
		t.Fatalf("active workload using previous queue disappeared: %v", err)
	}
	gotJob := newQueueObject(suspendedJob.GroupVersionKind())
	if err := c.Get(ctx, client.ObjectKeyFromObject(suspendedJob), gotJob); err != nil {
		t.Fatalf("accepted suspended Job disappeared during compatible cutover: %v", err)
	}
}

func TestLegacyTeamWorkspacePreservesExistingQueueContract(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationV0Primary: "true"}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = map[string]string{annotationResultScope: workspace.Spec.Defaults.OutputRoot}
	legacyQueue := testLocalQueue(workspace.Name, workspace.Spec.Queue, workspace.Spec.Queue)
	legacyQueue.SetLabels(workspaceLabels(workspace.Name))
	legacyQueue.SetAnnotations(nil)
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, legacyQueue, team, flavor, testClusterQueue(workspace.Spec.Queue)).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 6; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() iteration %d error = %v", i, err)
		}
	}
	gotLegacyQueue := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(legacyQueue), gotLegacyQueue); err != nil {
		t.Fatalf("legacy LocalQueue was not retained as a compatibility alias: %v", err)
	}
	if target, _, _ := unstructured.NestedString(gotLegacyQueue.Object, "spec", "clusterQueue"); target != workspace.Spec.Queue {
		t.Fatalf("legacy compatibility LocalQueue target = %q, want preserved target %q", target, workspace.Spec.Queue)
	}
	if gotLegacyQueue.GetAnnotations()[annotationOwnerUID] != "" {
		t.Fatalf("legacy compatibility LocalQueue was adopted: %#v", gotLegacyQueue.GetAnnotations())
	}
	var gotNamespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: workspace.Name}, &gotNamespace); err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got := gotNamespace.Labels[labelKueueDefaultLocalQueue]; got != workspaceLocalQueueName(workspace) {
		t.Fatalf("namespace default queue = %q, want %q", got, workspaceLocalQueueName(workspace))
	}
}

func TestExistingTeamWorkspaceWithoutQueuePreservesClientRoute(t *testing.T) {
	workspace := testWorkspace("aurora")
	workspace.Spec.Queue = ""
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseReady
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:   "jobqueue",
		ClusterQueue: "jobqueue",
	}
	managed := tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:   workspaceClusterQueueName(workspace.Name),
		ClusterQueue: workspaceClusterQueueName(workspace.Name),
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			testLocalQueue(workspace.Status.Target.ResolvedNamespace, "jobqueue", "jobqueue"),
			testClusterQueue("jobqueue"),
		).
		Build()
	got, err := newTestWorkspaceReconciler(c).workspaceClientQueueStatus(
		context.Background(),
		workspace,
		workspace.Status.Target.ResolvedNamespace,
		managed,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got.LocalQueue != "jobqueue" || got.ClusterQueue != "jobqueue" {
		t.Fatalf("client queue status = %#v, want complete inherited legacy route", got)
	}
}

func TestExistingTeamWorkspacePreservesClientRouteAcrossRecovery(t *testing.T) {
	workspace := testWorkspace("aurora")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "jobqueue",
		ClusterQueue:    "legacy-cq",
		ClusterQueueUID: "legacy-cq-uid",
	}
	managed := tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      workspaceClusterQueueName(workspace.Name),
		ClusterQueue:    workspaceClusterQueueName(workspace.Name),
		ClusterQueueUID: "managed-cq-uid",
	}

	for _, phase := range []string{
		tauv1alpha1.WorkspacePhaseReady,
		tauv1alpha1.WorkspacePhaseDegraded,
		tauv1alpha1.WorkspacePhaseReady,
	} {
		workspace.Status.Phase = phase
		c := fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(
				testLocalQueue(workspace.Status.Target.ResolvedNamespace, "jobqueue", "legacy-cq"),
				clusterQueueWithUID("legacy-cq", "legacy-cq-uid"),
			).
			Build()
		got, err := newTestWorkspaceReconciler(c).workspaceClientQueueStatus(
			context.Background(),
			workspace,
			workspace.Status.Target.ResolvedNamespace,
			managed,
		)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, workspace.Status.Queue) {
			t.Fatalf("phase %q client queue status = %#v, want established route %#v", phase, got, workspace.Status.Queue)
		}
	}
}

func TestTeamWorkspaceRecoversInheritedAliasFromEmptyQueueStatus(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.Queue = ""
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseDegraded
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, "jobqueue")
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	alias := testLocalQueue(workspace.Name, "jobqueue", "external-cq")
	alias.SetAnnotations(nil)
	external := clusterQueueWithUID("external-cq", "external-cq-uid")
	external.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")},
			true,
		),
	}
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, namespace, cluster, team, alias, external, flavor).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspaceTimes(t, reconciler, ctx, workspace.Name, 3)

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	want := tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      alias.GetName(),
		ClusterQueue:    external.GetName(),
		ClusterQueueUID: string(external.GetUID()),
	}
	if !reflect.DeepEqual(got.Status.Queue, want) {
		t.Fatalf("recovered queue status = %#v, want complete cached route %#v", got.Status.Queue, want)
	}
	preserved := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(alias), preserved); err != nil {
		t.Fatal(err)
	}
	if backing, _, _ := unstructured.NestedString(preserved.Object, "spec", "clusterQueue"); backing != external.GetName() {
		t.Fatalf("cached LocalQueue backing = %q, want %q", backing, external.GetName())
	}
}

func TestTeamWorkspacePersistsRecoveredRouteBeforeActivation(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.Queue = ""
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseDegraded
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, "jobqueue")
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	alias := testLocalQueue(workspace.Name, "jobqueue", "external-cq")
	alias.SetAnnotations(nil)
	external := clusterQueueWithUID("external-cq", "external-cq-uid")
	external.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")},
			true,
		),
	}
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	base := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, namespace, cluster, team, alias, external, flavor).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	statusFailure := errors.New("status update unavailable")
	failing := &statusUpdateFailingClient{Client: base, updateErr: statusFailure}
	reconciler := newTestWorkspaceReconciler(failing)
	resolved := workspace.DeepCopy()
	resolved.Spec.Queue = "jobqueue"

	_, err := reconciler.syncWorkspace(ctx, resolved)
	if !errors.Is(err, statusFailure) {
		t.Fatalf("sync error = %v, want status update failure", err)
	}
	var gotNamespace corev1.Namespace
	if err := base.Get(ctx, client.ObjectKey{Name: workspace.Name}, &gotNamespace); err != nil {
		t.Fatal(err)
	}
	if gotNamespace.Labels[labelKueueDefaultLocalQueue] != alias.GetName() {
		t.Fatalf("namespace route changed before recovered status persisted: %#v", gotNamespace.Labels)
	}
	var stored tauv1alpha1.TauWorkspace
	if err := base.Get(ctx, client.ObjectKeyFromObject(workspace), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.Queue.LocalQueue != "" {
		t.Fatalf("failed status update persisted queue %#v", stored.Status.Queue)
	}

	failing.updateErr = nil
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := base.Get(ctx, client.ObjectKeyFromObject(workspace), &stored); err != nil {
		t.Fatal(err)
	}
	want := tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      alias.GetName(),
		ClusterQueue:    external.GetName(),
		ClusterQueueUID: string(external.GetUID()),
	}
	if !reflect.DeepEqual(stored.Status.Queue, want) {
		t.Fatalf("checkpointed queue status = %#v, want %#v", stored.Status.Queue, want)
	}
	if err := base.Get(ctx, client.ObjectKey{Name: workspace.Name}, &gotNamespace); err != nil {
		t.Fatal(err)
	}
	if gotNamespace.Labels[labelKueueDefaultLocalQueue] != alias.GetName() {
		t.Fatalf("namespace activated in route checkpoint reconcile: %#v", gotNamespace.Labels)
	}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("activation reconcile %d: %v", i, err)
		}
	}
	if err := base.Get(ctx, client.ObjectKey{Name: workspace.Name}, &gotNamespace); err != nil {
		t.Fatal(err)
	}
	if gotNamespace.Labels[labelKueueDefaultLocalQueue] != workspaceClusterQueueName(workspace.Name) {
		t.Fatalf("namespace route = %q, want managed route after checkpoint", gotNamespace.Labels[labelKueueDefaultLocalQueue])
	}
}

func TestTeamWorkspaceRecoversInheritedAliasWithoutQuota(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.Queue = ""
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = nil
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseDegraded
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, "jobqueue")
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	managed := desiredWorkspaceClusterQueue(workspace)
	managed.SetUID(types.UID("managed-cq-uid"))
	alias := testLocalQueue(workspace.Name, "jobqueue", managed.GetName())
	alias.SetAnnotations(nil)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, namespace, cluster, team, managed, alias).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	reconcileWorkspaceTimes(t, newTestWorkspaceReconciler(c), ctx, workspace.Name, 3)

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	want := tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      alias.GetName(),
		ClusterQueue:    managed.GetName(),
		ClusterQueueUID: string(managed.GetUID()),
	}
	if !reflect.DeepEqual(got.Status.Queue, want) {
		t.Fatalf("recovered queue status = %#v, want complete inherited alias %#v", got.Status.Queue, want)
	}
}

func TestBrokenLegacyQueuePersistsDegradedStatusBeforeRetry(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = "jobqueue"
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseReady
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "jobqueue",
		ClusterQueue:    "missing-cq",
		ClusterQueueUID: "missing-cq-uid",
	}
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, "jobqueue")
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	alias := testLocalQueue(workspace.Name, "jobqueue", "missing-cq")
	alias.SetAnnotations(nil)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, namespace, team, flavor, alias).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	_, err := newTestWorkspaceReconciler(c).Reconcile(
		ctx,
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)},
	)
	if err == nil || !strings.Contains(err.Error(), "backing ClusterQueue") {
		t.Fatalf("Reconcile() error = %v, want missing backing retry", err)
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseDegraded {
		t.Fatalf("phase = %q, want Degraded", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionFalse)
}

func TestExistingTeamWorkspaceResolvesMovedCompatibilityQueueBacking(t *testing.T) {
	workspace := testWorkspace("aurora")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Status.Target.ResolvedNamespace = "old-aurora"
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "jobqueue",
		ClusterQueue:    "legacy-cq",
		ClusterQueueUID: "legacy-cq-uid",
	}
	alias := testLocalQueue("new-aurora", "jobqueue", workspaceClusterQueueName(workspace.Name))
	managedQueue := clusterQueueWithUID(workspaceClusterQueueName(workspace.Name), "managed-cq-uid")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(alias, managedQueue).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	got, err := reconciler.workspaceClientQueueStatus(
		context.Background(),
		workspace,
		"new-aurora",
		tauv1alpha1.WorkspaceQueueStatus{
			LocalQueue:      workspaceClusterQueueName(workspace.Name),
			ClusterQueue:    workspaceClusterQueueName(workspace.Name),
			ClusterQueueUID: "managed-cq-uid",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	want := tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "jobqueue",
		ClusterQueue:    workspaceClusterQueueName(workspace.Name),
		ClusterQueueUID: "managed-cq-uid",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("moved client queue status = %#v, want %#v", got, want)
	}
}

func TestWorkspaceTeamWatchRequiresAvailableAPI(t *testing.T) {
	if objects := workspaceWatchObjects(false); len(objects) != 0 {
		t.Fatalf("workspace watches without Team API = %#v, want none", objects)
	}
	objects := workspaceWatchObjects(true)
	if len(objects) != 1 {
		t.Fatalf("workspace watches with Team API = %#v, want Team watch", objects)
	}
	if _, ok := objects[0].(*tauv1alpha1.TauTeam); !ok {
		t.Fatalf("workspace watch object = %T, want *TauTeam", objects[0])
	}
}

func TestExistingTeamWorkspacePreservesManagedQueueContract(t *testing.T) {
	workspace := testWorkspace("aurora")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = nil
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "jobqueue",
		ClusterQueue:    "legacy-cq",
		ClusterQueueUID: "legacy-cq-uid",
	}
	legacyQueue := clusterQueueWithUID("legacy-cq", "legacy-cq-uid")
	legacyQueue.Object["spec"] = map[string]any{
		"admissionChecks": []any{"multikueue"},
		"admissionChecksStrategy": map[string]any{
			"admissionChecks": []any{map[string]any{"name": "policy"}},
		},
		"resourceGroups": []any{
			map[string]any{
				"coveredResources": []any{nvidiaGPUResourceName},
				"flavors": []any{
					map[string]any{
						"name": "taugrid-gpu-h200",
						"resources": []any{
							map[string]any{"name": nvidiaGPUResourceName, "nominalQuota": "8"},
						},
					},
				},
			},
		},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(legacyQueue).
		Build()
	desired := desiredWorkspaceClusterQueue(workspace)

	if err := newTestWorkspaceReconciler(c).preserveEstablishedWorkspaceQueueContract(
		context.Background(),
		workspace,
		desired,
	); err != nil {
		t.Fatal(err)
	}
	spec := desired.Object["spec"].(map[string]any)
	if !reflect.DeepEqual(spec["admissionChecks"], legacyQueue.Object["spec"].(map[string]any)["admissionChecks"]) {
		t.Fatalf("managed admissionChecks = %#v, want established checks", spec["admissionChecks"])
	}
	if !reflect.DeepEqual(spec["admissionChecksStrategy"], legacyQueue.Object["spec"].(map[string]any)["admissionChecksStrategy"]) {
		t.Fatalf("managed admissionChecksStrategy = %#v, want established strategy", spec["admissionChecksStrategy"])
	}
	if !reflect.DeepEqual(spec["resourceGroups"], legacyQueue.Object["spec"].(map[string]any)["resourceGroups"]) {
		t.Fatalf("managed resourceGroups = %#v, want established quota contract", spec["resourceGroups"])
	}
}

func TestTeamWorkspaceInheritsAdmissionContractFromLocalQueueBacking(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = "jobqueue"
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = map[string]string{
		annotationOwnerUID:    string(workspace.UID),
		annotationResultScope: workspace.Spec.Defaults.OutputRoot,
	}
	legacyQueue := clusterQueueWithUID("legacy-cq", "legacy-cq-uid")
	legacyQueue.Object["spec"] = map[string]any{
		"admissionChecks": []any{"multikueue"},
		"admissionChecksStrategy": map[string]any{
			"admissionChecks": []any{map[string]any{"name": "policy"}},
		},
		"resourceGroups": quotaResourceGroups(workspace.Spec.Quota, true),
	}
	unrelatedQueue := clusterQueueWithUID(workspace.Spec.Queue, "unrelated-jobqueue-uid")
	unrelatedQueue.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(workspace.Spec.Quota, true),
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			workspace,
			namespace,
			team,
			flavor,
			testLocalQueue(workspace.Name, workspace.Spec.Queue, legacyQueue.GetName()),
			legacyQueue,
			unrelatedQueue,
		).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}); err != nil {
			t.Fatalf("Reconcile() iteration %d error = %v", i, err)
		}
	}
	managed := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, managed); err != nil {
		t.Fatal(err)
	}
	assertAdmissionContract(t, managed, legacyQueue)
}

func TestEmptyQuotaTeamWorkspaceRetainsOversizedLocalQueueBacking(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = "jobqueue"
	workspace.Spec.Quota = nil
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = map[string]string{
		annotationOwnerUID:    string(workspace.UID),
		annotationResultScope: workspace.Spec.Defaults.OutputRoot,
	}
	external := clusterQueueWithUID("platform-cq", "platform-cq-uid")
	external.Object["spec"] = map[string]any{
		"admissionChecks": []any{"multikueue"},
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "16", "0", "0")},
			true,
		),
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			workspace,
			namespace,
			team,
			testLocalQueue(workspace.Name, workspace.Spec.Queue, external.GetName()),
			external,
		).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() iteration %d error = %v", i, err)
		}
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready on retained external allocation; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	if got.Status.Queue.LocalQueue != workspace.Spec.Queue ||
		got.Status.Queue.ClusterQueue != external.GetName() ||
		got.Status.Queue.ClusterQueueUID != string(external.GetUID()) {
		t.Fatalf("status.queue = %#v, want profile LocalQueue backed by retained ClusterQueue %#v", got.Status.Queue, external)
	}
	managed := newQueueObject(clusterQueueGVK)
	err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, managed)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("managed ClusterQueue was created beside retained external allocation: %v", err)
	}
}

func TestTeamWorkspaceRetainsExistingTeamChildFromLocalQueue(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = "jobqueue"
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = map[string]string{
		annotationOwnerUID:    string(workspace.UID),
		annotationResultScope: workspace.Spec.Defaults.OutputRoot,
	}
	legacyChild := clusterQueueWithUID("legacy-child", "legacy-child-uid")
	legacyChild.Object["spec"] = map[string]any{
		"cohortName": teamCohortName(workspace.Spec.TeamRef.Name),
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")},
			true,
		),
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			workspace,
			namespace,
			team,
			testLocalQueue(workspace.Name, workspace.Spec.Queue, legacyChild.GetName()),
			legacyChild,
		).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() iteration %d error = %v", i, err)
		}
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady ||
		got.Status.Queue.ClusterQueue != legacyChild.GetName() {
		t.Fatalf("workspace status = %#v, want Ready on retained Team child", got.Status)
	}
	managed := newQueueObject(clusterQueueGVK)
	err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, managed)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("managed ClusterQueue was created beside retained Team child: %v", err)
	}
	remaining, err := (&TauTeamReconciler{Client: c, APIReader: c}).sharedTeamQuotaForWorkspace(ctx, team, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].NominalQuota.Cmp(resource.MustParse("4")) != 0 {
		t.Fatalf("shared Team quota = %#v, want retained child charged once", remaining)
	}
}

func TestTeamWorkspacePreservesCanonicalExternalLocalQueue(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = workspaceClusterQueueName(workspace.Name)
	workspace.Spec.Quota = nil
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = map[string]string{
		annotationOwnerUID:    string(workspace.UID),
		annotationResultScope: workspace.Spec.Defaults.OutputRoot,
	}
	external := clusterQueueWithUID("platform-cq", "platform-cq-uid")
	external.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")},
			true,
		),
	}
	localQueue := testLocalQueue(workspace.Name, workspace.Spec.Queue, external.GetName())
	localQueue.SetAnnotations(nil)
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, namespace, team, localQueue, external).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("Reconcile() iteration %d error = %v", i, err)
		}
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady ||
		got.Status.Queue.LocalQueue != localQueue.GetName() ||
		got.Status.Queue.ClusterQueue != external.GetName() {
		t.Fatalf("workspace status = %#v, want Ready on canonical external route", got.Status)
	}
	preserved := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(localQueue), preserved); err != nil {
		t.Fatal(err)
	}
	if ownedByWorkspace(preserved.GetLabels(), workspace.Name) ||
		preserved.GetAnnotations()[annotationOwnerUID] != "" {
		t.Fatalf("canonical external LocalQueue was adopted: labels=%#v annotations=%#v", preserved.GetLabels(), preserved.GetAnnotations())
	}
}

func TestTeamWorkspacePreservesSameNameExternalQueue(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = workspaceClusterQueueName(workspace.Name)
	workspace.Spec.Quota = nil
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = map[string]string{
		annotationOwnerUID:    string(workspace.UID),
		annotationResultScope: workspace.Spec.Defaults.OutputRoot,
	}
	external := clusterQueueWithUID(workspace.Spec.Queue, "platform-cq-uid")
	external.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")},
			true,
		),
	}
	localQueue := testLocalQueue(workspace.Name, workspace.Spec.Queue, external.GetName())
	localQueue.SetAnnotations(nil)
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, namespace, team, localQueue, external).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}); err != nil {
			t.Fatalf("Reconcile() iteration %d error = %v", i, err)
		}
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady ||
		got.Status.Queue.LocalQueue != localQueue.GetName() ||
		got.Status.Queue.ClusterQueue != external.GetName() {
		t.Fatalf("workspace status = %#v, want Ready on same-name external route", got.Status)
	}
	preserved := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(external), preserved); err != nil {
		t.Fatal(err)
	}
	if ownedByWorkspace(preserved.GetLabels(), workspace.Name) ||
		preserved.GetAnnotations()[annotationOwnerUID] != "" {
		t.Fatalf("same-name external ClusterQueue was adopted: labels=%#v annotations=%#v", preserved.GetLabels(), preserved.GetAnnotations())
	}
	if err := c.Delete(ctx, &got); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(external), preserved); err != nil {
		t.Fatalf("same-name external ClusterQueue was deleted with Workspace: %v", err)
	}
}

func TestWorkspaceDeletionPreservesSameNameExternalQueue(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.Queue = workspaceClusterQueueName(workspace.Name)
	now := metav1.Now()
	workspace.DeletionTimestamp = &now
	external := clusterQueueWithUID(workspace.Spec.Queue, "platform-cq-uid")
	localQueue := testLocalQueue(workspace.Name, workspace.Spec.Queue, external.GetName())
	localQueue.SetAnnotations(nil)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, localQueue, external).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	if workspace.Status.Queue.ClusterQueueUID != "" {
		t.Fatalf("test requires UID-less base status, got %#v", workspace.Status.Queue)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, request.NamespacedName, &tauv1alpha1.TauWorkspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace finalizer was not removed: %v", err)
	}
	preserved := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(external), preserved); err != nil {
		t.Fatalf("same-name external ClusterQueue was deleted with Workspace: %v", err)
	}
}

func TestWorkspaceDeletionIgnoresUnrelatedCanonicalQueue(t *testing.T) {
	for _, tt := range []struct {
		name       string
		teamBacked bool
	}{
		{name: "non-Team"},
		{name: "Team-backed", teamBacked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			workspace := testWorkspace("aurora")
			workspace.Finalizers = []string{workspaceFinalizer}
			workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
			workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
			workspace.Spec.WorkloadIdentity = nil
			workspace.Spec.Queue = "jobqueue"
			if tt.teamBacked {
				workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
			}
			now := metav1.Now()
			workspace.DeletionTimestamp = &now
			external := clusterQueueWithUID(workspaceClusterQueueName(workspace.Name), "unrelated-cq-uid")
			localQueue := testLocalQueue(workspace.Name, workspace.Spec.Queue, "jobqueue-cq")
			localQueue.SetAnnotations(nil)
			c := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(workspace, localQueue, external).
				WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
				Build()
			reconciler := newTestWorkspaceReconciler(c)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

			if _, err := reconciler.Reconcile(ctx, request); err != nil {
				t.Fatal(err)
			}
			if err := c.Get(ctx, request.NamespacedName, &tauv1alpha1.TauWorkspace{}); !apierrors.IsNotFound(err) {
				t.Fatalf("workspace finalizer was not removed: %v", err)
			}
			preserved := newQueueObject(clusterQueueGVK)
			if err := c.Get(ctx, client.ObjectKeyFromObject(external), preserved); err != nil {
				t.Fatalf("unrelated canonical ClusterQueue was deleted: %v", err)
			}
		})
	}
}

func TestTeamWorkspacePreservesManagedAdmissionContractAfterSourceDeletion(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = "legacy-cq"
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	namespace.Annotations = map[string]string{
		annotationOwnerUID:    string(workspace.UID),
		annotationResultScope: workspace.Spec.Defaults.OutputRoot,
	}
	source := clusterQueueWithUID("legacy-cq", "legacy-cq-uid")
	source.Object["spec"] = map[string]any{
		"admissionChecks": []any{"multikueue"},
		"admissionChecksStrategy": map[string]any{
			"admissionChecks": []any{map[string]any{"name": "policy"}},
		},
		"resourceGroups": quotaResourceGroups(workspace.Spec.Quota, true),
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			workspace,
			namespace,
			team,
			flavor,
			testLocalQueue(workspace.Name, workspace.Spec.Queue, source.GetName()),
			source,
		).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}
	for i := 0; i < 5; i++ {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatalf("initial Reconcile() iteration %d error = %v", i, err)
		}
	}
	managed := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, managed); err != nil {
		t.Fatal(err)
	}
	assertAdmissionContract(t, managed, source)
	if err := c.Delete(ctx, source); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err == nil ||
		!strings.Contains(err.Error(), `compatible LocalQueue "legacy-cq" backing ClusterQueue "legacy-cq" is not ready`) {
		t.Fatalf("missing external compatibility backing did not fail closed: %v", err)
	}
	preserved := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: managed.GetName()}, preserved); err != nil {
		t.Fatal(err)
	}
	assertAdmissionContract(t, preserved, managed)
}

func TestTeamWorkspacePreservesSharedLegacyQueueBeforeCreatingManagedQueues(t *testing.T) {
	ctx := context.Background()
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	first := testWorkspace("aurora")
	first.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	first.Spec.Queue = "legacy"
	first.Spec.Quota = nil
	first.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{LocalQueue: "legacy", ClusterQueue: "legacy"}
	second := testWorkspace("borealis")
	second.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	second.Spec.Queue = "legacy"
	second.Spec.Quota = nil
	second.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{LocalQueue: "legacy", ClusterQueue: "legacy"}
	legacyQueue := testClusterQueue("legacy")
	legacyQueue.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")},
			true,
		),
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(team, first, second, legacyQueue).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	remaining, err := (&TauTeamReconciler{Client: c, APIReader: c}).sharedTeamQuotaForWorkspace(ctx, team, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].NominalQuota.Cmp(resource.MustParse("8")) != 0 {
		t.Fatalf("shared team quota = %#v, want retained external allocation excluded", remaining)
	}
	got, err := reconciler.reconcileWorkspaceClusterQueue(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if got != legacyQueue.GetName() {
		t.Fatalf("reconcileWorkspaceClusterQueue() = %q, want shared legacy queue %q", got, legacyQueue.GetName())
	}
	for _, workspace := range []*tauv1alpha1.TauWorkspace{first, second} {
		queue := newQueueObject(clusterQueueGVK)
		err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, queue)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("managed ClusterQueue %q was created before shared accounting: %v", queue.GetName(), err)
		}
	}
}

func TestTeamWorkspaceRetainsEstablishedQueueUntilQuotaCanTransfer(t *testing.T) {
	ctx := context.Background()
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	workspace := testWorkspace("aurora")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Queue = "legacy"
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{LocalQueue: "legacy", ClusterQueue: "legacy"}
	legacyQueue := testClusterQueue("legacy")
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(team, workspace, legacyQueue, flavor, testLocalQueue(workspace.Name, "legacy", "legacy")).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	got, err := reconciler.reconcileWorkspaceClusterQueue(ctx, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if got != legacyQueue.GetName() {
		t.Fatalf("reconcileWorkspaceClusterQueue() = %q, want established queue %q", got, legacyQueue.GetName())
	}
	managed := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, managed); !apierrors.IsNotFound(err) {
		t.Fatalf("managed ClusterQueue was created before route transfer: %v", err)
	}
}

func TestTeamWorkspaceRetainedQueueSurvivesCapacityDropThroughReconcile(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = "legacy"
	workspace.Spec.Quota = nil
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseReady
	workspace.Status.ObservedGeneration = workspace.Generation
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "legacy",
		ClusterQueue:    "legacy",
		ClusterQueueUID: "legacy-uid",
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Status: tauv1alpha1.TauClusterStatus{
			DiscoveredCapacity: []tauv1alpha1.TauResourceCapacityStatus{{
				Flavor:   "taugrid-gpu-h200",
				Resource: nvidiaGPUResourceName,
				Capacity: resource.MustParse("4"),
			}},
		},
	}
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, "legacy")
	legacyQueue := clusterQueueWithUID("legacy", "legacy-uid")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, team, cluster, namespace, testLocalQueue(workspace.Name, "legacy", "legacy"), legacyQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}); err != nil {
		t.Fatal(err)
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready on retained external route; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	if !reflect.DeepEqual(got.Status.Queue, workspace.Status.Queue) {
		t.Fatalf("status.queue = %#v, want retained route %#v", got.Status.Queue, workspace.Status.Queue)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionTrue)
}

func TestTeamWorkspaceManagedQueueSurvivesCapacityDropThroughReconcile(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = workspaceClusterQueueName(workspace.Name)
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseReady
	workspace.Status.ObservedGeneration = workspace.Generation
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      workspaceClusterQueueName(workspace.Name),
		ClusterQueue:    workspaceClusterQueueName(workspace.Name),
		ClusterQueueUID: "managed-cq-uid",
	}
	team := testTeam("vision", "8")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Status: tauv1alpha1.TauClusterStatus{
			DiscoveredCapacity: []tauv1alpha1.TauResourceCapacityStatus{{
				Flavor:   "taugrid-gpu-h200",
				Resource: nvidiaGPUResourceName,
				Capacity: resource.MustParse("4"),
			}},
		},
	}
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspaceLocalQueueName(workspace))
	namespace.Annotations = map[string]string{
		annotationOwnerUID:    string(workspace.UID),
		annotationResultScope: workspace.Spec.Defaults.OutputRoot,
	}
	managed := desiredWorkspaceClusterQueue(workspace)
	managed.SetUID(types.UID("managed-cq-uid"))
	localQueue := testLocalQueue(workspace.Name, workspaceLocalQueueName(workspace), managed.GetName())
	localQueue.SetLabels(workspaceLabels(workspace.Name))
	setOwnerUIDAnnotation(localQueue, workspace.UID)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, team, cluster, namespace, managed, localQueue, flavor).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	if _, err := newTestWorkspaceReconciler(c).Reconcile(
		ctx,
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)},
	); err != nil {
		t.Fatal(err)
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready on unchanged managed allocation; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	if !reflect.DeepEqual(got.Status.Queue, workspace.Status.Queue) {
		t.Fatalf("status.queue = %#v, want unchanged managed route %#v", got.Status.Queue, workspace.Status.Queue)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionTrue)
}

func TestTeamWorkspaceManagedQueueSurvivesTeamDeletionThroughReconcile(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = workspaceClusterQueueName(workspace.Name)
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")}
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseReady
	workspace.Status.ObservedGeneration = workspace.Generation
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      workspaceClusterQueueName(workspace.Name),
		ClusterQueue:    workspaceClusterQueueName(workspace.Name),
		ClusterQueueUID: "managed-cq-uid",
	}
	team := testTeam("vision", "8")
	team.UID = types.UID("vision-uid")
	team.Finalizers = []string{teamFinalizer}
	team.DeletionTimestamp = &now
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspaceLocalQueueName(workspace))
	namespace.Annotations = map[string]string{
		annotationOwnerUID:    string(workspace.UID),
		annotationResultScope: workspace.Spec.Defaults.OutputRoot,
	}
	managed := desiredWorkspaceClusterQueue(workspace)
	managed.SetUID(types.UID("managed-cq-uid"))
	localQueue := testLocalQueue(workspace.Name, workspaceLocalQueueName(workspace), managed.GetName())
	localQueue.SetLabels(workspaceLabels(workspace.Name))
	setOwnerUIDAnnotation(localQueue, workspace.UID)
	cohort := desiredTeamCohort(team, nil)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, team, namespace, managed, localQueue, cohort, flavor).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	if _, err := newTestWorkspaceReconciler(c).Reconcile(
		ctx,
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)},
	); err != nil {
		t.Fatal(err)
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready on unchanged managed route while Team terminates; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	if !reflect.DeepEqual(got.Status.Queue, workspace.Status.Queue) {
		t.Fatalf("status.queue = %#v, want unchanged managed route %#v", got.Status.Queue, workspace.Status.Queue)
	}
}

func TestTeamWorkspaceRetainedQueueSurvivesTeamDeletionThroughReconcile(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()
	workspace := testWorkspace("aurora")
	workspace.Finalizers = []string{workspaceFinalizer}
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = "legacy"
	workspace.Spec.Quota = nil
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	workspace.Spec.WorkloadIdentity = nil
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseReady
	workspace.Status.ObservedGeneration = workspace.Generation
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "legacy",
		ClusterQueue:    "legacy",
		ClusterQueueUID: "legacy-uid",
	}
	team := testTeam("vision", "8")
	team.Finalizers = []string{teamFinalizer}
	team.DeletionTimestamp = &now
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, "legacy")
	legacyQueue := clusterQueueWithUID("legacy", "legacy-uid")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, team, namespace, testLocalQueue(workspace.Name, "legacy", "legacy"), legacyQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	if _, err := newTestWorkspaceReconciler(c).Reconcile(
		ctx,
		ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)},
	); err != nil {
		t.Fatal(err)
	}
	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready while Team is terminating; conditions=%#v", got.Status.Phase, got.Status.Conditions)
	}
	if !reflect.DeepEqual(got.Status.Queue, workspace.Status.Queue) {
		t.Fatalf("status.queue = %#v, want retained route %#v", got.Status.Queue, workspace.Status.Queue)
	}
}

func TestRetainedTeamCohortChildGuaranteeRemainsCharged(t *testing.T) {
	ctx := context.Background()
	team := testTeam("vision", "8")
	workspace := testWorkspace("aurora")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Queue = "legacy-child"
	workspace.Spec.Quota = nil
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:   "legacy-child",
		ClusterQueue: "legacy-child",
	}
	child := testClusterQueue("legacy-child")
	child.Object["spec"] = map[string]any{
		"cohortName": teamCohortName(team.Name),
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")},
			true,
		),
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(team, workspace, child).
		Build()

	shared, err := (&TauTeamReconciler{Client: c, APIReader: c}).sharedTeamQuotaForWorkspace(ctx, team, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 1 || shared[0].NominalQuota.Cmp(resource.MustParse("4")) != 0 {
		t.Fatalf("shared quota = %#v, want retained child guarantee charged exactly once", shared)
	}
}

func TestSharedRetainedTeamCohortChildGuaranteeChargedOnce(t *testing.T) {
	ctx := context.Background()
	team := testTeam("vision", "8")
	first := testWorkspace("aurora")
	first.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	first.Spec.Queue = "legacy-child"
	first.Spec.Quota = nil
	first.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:   "legacy-child",
		ClusterQueue: "legacy-child",
	}
	second := testWorkspace("borealis")
	second.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	second.Spec.Queue = "legacy-child"
	second.Spec.Quota = nil
	second.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:   "legacy-child",
		ClusterQueue: "legacy-child",
	}
	child := clusterQueueWithUID("legacy-child", "legacy-child-uid")
	child.Object["spec"] = map[string]any{
		"cohortName": teamCohortName(team.Name),
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")},
			true,
		),
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(team, first, second, child).
		Build()

	shared, err := (&TauTeamReconciler{Client: c, APIReader: c}).sharedTeamQuotaForWorkspace(ctx, team, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 1 || shared[0].NominalQuota.Cmp(resource.MustParse("0")) != 0 {
		t.Fatalf("shared quota = %#v, want shared retained child charged once", shared)
	}
	reconciler := newTestWorkspaceReconciler(c)
	for _, workspace := range []*tauv1alpha1.TauWorkspace{first, second} {
		got, err := reconciler.reconcileWorkspaceClusterQueue(ctx, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if got != child.GetName() {
			t.Fatalf("reconcileWorkspaceClusterQueue(%q) = %q, want shared retained child %q", workspace.Name, got, child.GetName())
		}
		managed := newQueueObject(clusterQueueGVK)
		err = c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, managed)
		if !apierrors.IsNotFound(err) {
			t.Fatalf("managed ClusterQueue %q was created beside shared retained child: %v", managed.GetName(), err)
		}
	}
}

func TestCanonicalOwnerAndRetainedConsumerChargeSharedChildOnce(t *testing.T) {
	ctx := context.Background()
	team := testTeam("vision", "4")
	owner := testWorkspace("aurora")
	owner.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	owner.Spec.Queue = workspaceClusterQueueName(owner.Name)
	owner.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	consumer := testWorkspace("borealis")
	consumer.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	consumer.Spec.Queue = "shared"
	consumer.Spec.Quota = nil
	consumer.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "shared",
		ClusterQueue:    workspaceClusterQueueName(owner.Name),
		ClusterQueueUID: "shared-child-uid",
	}
	child := desiredWorkspaceClusterQueue(owner)
	child.SetUID(types.UID("shared-child-uid"))
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(team, owner, consumer, child).
		Build()

	remaining, err := (&TauTeamReconciler{Client: c, APIReader: c}).sharedTeamQuotaForWorkspace(ctx, team, owner)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 1 || remaining[0].NominalQuota.Cmp(resource.MustParse("0")) != 0 {
		t.Fatalf("shared Team quota = %#v, want canonical child charged once", remaining)
	}
}

func TestConsumerFirstQuotaIncreaseRejectedBeforeManagedQueueWrite(t *testing.T) {
	ctx := context.Background()
	team := testTeam("vision", "4")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	consumer := testWorkspace("aardvark")
	consumer.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	consumer.Spec.Queue = "shared"
	consumer.Spec.Quota = nil
	consumer.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "shared",
		ClusterQueue:    workspaceClusterQueueName("zeta"),
		ClusterQueueUID: "shared-child-uid",
	}
	owner := testWorkspace("zeta")
	owner.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	owner.Spec.Queue = workspaceClusterQueueName(owner.Name)
	owner.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")}
	child := desiredWorkspaceClusterQueue(owner)
	child.SetUID(types.UID("shared-child-uid"))
	child.Object["spec"].(map[string]any)["resourceGroups"] = quotaResourceGroups(
		[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")},
		true,
	)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(team, consumer, owner, child, flavor).
		Build()
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := c.List(ctx, &workspaces, client.InNamespace(team.Namespace)); err != nil {
		t.Fatal(err)
	}
	if len(workspaces.Items) != 2 || workspaces.Items[0].Name != consumer.Name {
		t.Fatalf("workspace traversal order = %#v, want consumer first", workspaces.Items)
	}

	_, err := newTestWorkspaceReconciler(c).reconcileWorkspaceClusterQueue(ctx, owner)
	if err == nil || !strings.Contains(err.Error(), "workspace guarantees exceed team quota") {
		t.Fatalf("reconcileWorkspaceClusterQueue() error = %v, want Team quota rejection", err)
	}
	preserved := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(child), preserved); err != nil {
		t.Fatal(err)
	}
	guarantees, err := clusterQueueNominalQuota(preserved)
	if err != nil {
		t.Fatal(err)
	}
	key := quotaKey(owner.Spec.Quota[0])
	if got := guarantees[key]; got.Cmp(resource.MustParse("4")) != 0 {
		t.Fatalf("persisted managed guarantee = %s, want original 4", got.String())
	}
}

func TestEmptyQuotaWorkspacesRetainPlatformQueueAcrossTeams(t *testing.T) {
	ctx := context.Background()
	vision := testTeam("vision", "4")
	language := testTeam("language", "4")
	for _, team := range []*tauv1alpha1.TauTeam{vision, language} {
		team.Status.Phase = tauv1alpha1.TeamPhaseReady
		team.Status.ObservedGeneration = team.Generation
	}
	first := testWorkspace("aurora")
	first.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: vision.Name}
	first.Spec.Queue = "platform"
	first.Spec.Quota = nil
	second := testWorkspace("borealis")
	second.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: language.Name}
	second.Spec.Queue = "platform"
	second.Spec.Quota = nil
	platformQueue := testClusterQueue("platform")
	platformQueue.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")},
			true,
		),
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(vision, language, first, second, platformQueue).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	teamReconciler := &TauTeamReconciler{Client: c, APIReader: c}

	for _, item := range []struct {
		team      *tauv1alpha1.TauTeam
		workspace *tauv1alpha1.TauWorkspace
	}{
		{vision, first},
		{language, second},
	} {
		shared, err := teamReconciler.sharedTeamQuotaForWorkspace(ctx, item.team, item.workspace)
		if err != nil {
			t.Fatal(err)
		}
		if len(shared) != 1 || shared[0].NominalQuota.Cmp(resource.MustParse("4")) != 0 {
			t.Fatalf("team %q shared quota = %#v, want unchanged platform-independent allocation", item.team.Name, shared)
		}
		got, err := reconciler.reconcileWorkspaceClusterQueue(ctx, item.workspace)
		if err != nil {
			t.Fatal(err)
		}
		if got != platformQueue.GetName() {
			t.Fatalf("workspace %q queue = %q, want platform queue", item.workspace.Name, got)
		}
	}
	if err := c.Delete(ctx, second); err != nil {
		t.Fatal(err)
	}
	shared, err := teamReconciler.sharedTeamQuotaForWorkspace(ctx, vision, first)
	if err != nil {
		t.Fatal(err)
	}
	if len(shared) != 1 || shared[0].NominalQuota.Cmp(resource.MustParse("4")) != 0 {
		t.Fatalf("sole consumer shared quota = %#v, want retained external allocation excluded after peer deletion", shared)
	}
}

func TestWorkspaceRevokesAccessFromAdoptedNamespaceBeforeTargetReady(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Spec.Target.Namespace = "unavailable"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.KubernetesSubject = &tauv1alpha1.KubernetesSubject{Kind: rbacv1.GroupKind, Name: "current-researchers"}
	workspace.Spec.WorkloadIdentity.ServiceAccountName = "current-workload"
	adopted := testNamespace("adopted")
	adopted.Labels = map[string]string{labelWorkspace: workspace.Name}
	adopted.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	binding := testRoleBinding(adopted.Name, defaultRoleName, workspace.Name)
	binding.Subjects = []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "obsolete-researchers"}}
	identity := testServiceAccount(adopted.Name, "obsolete-workload", workspace.Name)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, adopted, binding, identity).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	if _, err := newTestWorkspaceReconciler(c).syncWorkspace(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	var gotBinding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &gotBinding); err != nil {
		t.Fatal(err)
	}
	want := []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, adopted.Name)}
	if !reflect.DeepEqual(gotBinding.Subjects, want) {
		t.Fatalf("adopted namespace subjects = %#v, want %#v", gotBinding.Subjects, want)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(identity), &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete adopted namespace identity survived: %v", err)
	}
}

func TestExistingTeamWorkspaceRejectsEmptyManagedQueueWithoutEstablishedContract(t *testing.T) {
	workspace := testWorkspace("aurora")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = nil
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:   "jobqueue",
		ClusterQueue: "missing-cq",
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		Build()

	err := newTestWorkspaceReconciler(c).preserveEstablishedWorkspaceQueueContract(
		context.Background(),
		workspace,
		desiredWorkspaceClusterQueue(workspace),
	)
	if err == nil || !strings.Contains(err.Error(), "established workspace queue contract is unavailable") {
		t.Fatalf("preserveEstablishedWorkspaceQueueContract() error = %v", err)
	}
}

func TestWorkspaceRevokesAccessAcrossAbandonedOwnedNamespaces(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Target.Namespace = "unavailable-c"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.KubernetesSubject = &tauv1alpha1.KubernetesSubject{Kind: rbacv1.GroupKind, Name: "current-researchers"}
	workspace.Spec.WorkloadIdentity.ServiceAccountName = "current-workload"
	workspace.Status.Target.ResolvedNamespace = "active-a"
	active := testNamespace("active-a")
	active.Labels = workspaceLabels(workspace.Name)
	active.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	staging := testNamespace("staging-b")
	staging.Labels = workspaceLabels(workspace.Name)
	staging.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	activeBinding := testRoleBinding(active.Name, defaultRoleName, workspace.Name)
	stagingBinding := testRoleBinding(staging.Name, defaultRoleName, workspace.Name)
	for _, binding := range []*rbacv1.RoleBinding{activeBinding, stagingBinding} {
		binding.Subjects = []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "obsolete-researchers"}}
	}
	activeIdentity := testServiceAccount(active.Name, "obsolete-workload", workspace.Name)
	stagingIdentity := testServiceAccount(staging.Name, "obsolete-workload", workspace.Name)
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, active, staging, activeBinding, stagingBinding, activeIdentity, stagingIdentity).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	if _, err := newTestWorkspaceReconciler(c).syncWorkspace(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	for _, namespace := range []string{active.Name, staging.Name} {
		var binding rbacv1.RoleBinding
		if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: namespace}, &binding); err != nil {
			t.Fatal(err)
		}
		want := []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, namespace)}
		if !reflect.DeepEqual(binding.Subjects, want) {
			t.Fatalf("namespace %q subjects = %#v, want %#v", namespace, binding.Subjects, want)
		}
		if err := c.Get(ctx, client.ObjectKey{Name: "obsolete-workload", Namespace: namespace}, &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
			t.Fatalf("namespace %q obsolete identity survived: %v", namespace, err)
		}
	}
}

func TestPreviousNamespaceResearcherAccessRejectsForeignBinding(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: "old-aurora"},
		Subjects:   []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: "foreign"}},
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(binding).
		Build()

	err := newTestWorkspaceReconciler(c).reconcileExistingResearcherAccess(ctx, workspace, binding.Namespace)
	if err == nil || !strings.Contains(err.Error(), "is not owned by workspace") {
		t.Fatalf("reconcileExistingResearcherAccess() error = %v, want ownership refusal", err)
	}
	var got rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKeyFromObject(binding), &got); err != nil {
		t.Fatal(err)
	}
	if got.Annotations[annotationOwnerUID] != "" || got.Subjects[0].Name != "foreign" {
		t.Fatalf("foreign RoleBinding mutated: %#v", got)
	}
}

func TestWorkspaceRevokesOwnerlessLegacyIdentityBeforeTargetReady(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	workspace.Spec.Target.Namespace = "unavailable"
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.WorkloadIdentity.ServiceAccountName = "current-workload"
	ownedNamespace := testNamespace("staging")
	ownedNamespace.Labels = workspaceLabels(workspace.Name)
	ownedNamespace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	legacy := testServiceAccount(ownedNamespace.Name, "obsolete-workload", workspace.Name)
	legacy.Annotations = map[string]string{annotationAzureWIClientID: "obsolete-client"}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, ownedNamespace, legacy).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	if _, err := newTestWorkspaceReconciler(c).syncWorkspace(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(legacy), &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
		t.Fatalf("ownerless legacy ServiceAccount survived cleanup: %v", err)
	}
}

func TestExistingTeamWorkspaceRecreatesCompatibilityQueueWithEstablishedBacking(t *testing.T) {
	workspace := testWorkspace("aurora")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Queue = "jobqueue"
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "jobqueue",
		ClusterQueue:    "legacy-cq",
		ClusterQueueUID: "legacy-cq-uid",
	}
	legacyQueue := clusterQueueWithUID("legacy-cq", "legacy-cq-uid")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(legacyQueue).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	ready, message, err := reconciler.reconcileWorkspaceCompatibilityLocalQueues(
		context.Background(),
		workspace,
		workspace.Name,
		workspaceClusterQueueName(workspace.Name),
	)
	if err != nil || !ready {
		t.Fatalf("reconcile compatibility LocalQueue ready=%v message=%q error=%v", ready, message, err)
	}
	queue := newQueueObject(localQueueGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: "jobqueue", Namespace: workspace.Name}, queue); err != nil {
		t.Fatal(err)
	}
	if target, _, _ := unstructured.NestedString(queue.Object, "spec", "clusterQueue"); target != "legacy-cq" {
		t.Fatalf("recreated compatibility LocalQueue target = %q, want legacy-cq", target)
	}
}

func TestWorkspaceRejectsTeamRemovalBeforeSwitchingLocalQueue(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	managedQueue := desiredWorkspaceClusterQueue(workspace)
	managedQueue.SetUID(types.UID("managed-queue-uid"))
	localQueue := testLocalQueue(workspace.Name, workspace.Spec.Queue, managedQueue.GetName())
	localQueue.SetLabels(workspaceLabels(workspace.Name))
	setOwnerUIDAnnotation(localQueue, workspace.UID)
	workspace.Spec.TeamRef = nil

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, managedQueue, localQueue, testClusterQueue(workspace.Spec.Queue)).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	status, ready, message, _ := reconciler.reconcileQueue(ctx, workspace, workspace.Name)
	if ready {
		t.Fatalf("queue unexpectedly Ready: %#v", status)
	}
	if !strings.Contains(message, "cannot remove teamRef while managed ClusterQueue") {
		t.Fatalf("queue message = %q, want managed ClusterQueue removal refusal", message)
	}
	gotLocalQueue := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(localQueue), gotLocalQueue); err != nil {
		t.Fatalf("Get LocalQueue: %v", err)
	}
	if target, _, _ := unstructured.NestedString(gotLocalQueue.Object, "spec", "clusterQueue"); target != managedQueue.GetName() {
		t.Fatalf("LocalQueue target = %q, want %q", target, managedQueue.GetName())
	}
}

func TestWorkspaceReconcileReplacesOwnedLocalQueueClusterQueue(t *testing.T) {
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
	recordingClient := &resourceMutationRecordingClient{Client: c}
	reconciler := newTestWorkspaceReconciler(recordingClient)

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: "aurora"}, got); err != nil {
		t.Fatalf("Get LocalQueue: %v", err)
	}
	clusterQueue, _, _ := unstructured.NestedString(got.Object, "spec", "clusterQueue")
	if clusterQueue != "aurora" {
		t.Fatalf("owned LocalQueue clusterQueue = %q, want replacement targeting aurora", clusterQueue)
	}
	deleteIndex := slices.Index(recordingClient.mutations, "delete aurora")
	if deleteIndex < 0 || !slices.Contains(recordingClient.mutations[deleteIndex+1:], "create aurora") {
		t.Fatalf("LocalQueue replacement mutations = %v, want delete then create", recordingClient.mutations)
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
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, "aurora")
	if err := c.Delete(ctx, clusterQueue); err != nil {
		t.Fatalf("delete backing ClusterQueue: %v", err)
	}
	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &got); err != nil {
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
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, "aurora")

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("phase = %q, want Ready without a PVC", got.Status.Phase)
	}
	assertConditionAbsent(t, got.Status.Conditions, "StorageReady")
}

func TestWorkspaceReconcileRequeuesWhileNotReady(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(testWorkspace("aurora"), testNamespace("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}}

	var result ctrl.Result
	for i := 0; i < 3; i++ {
		var err error
		if result, err = reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKey{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}, &got); err != nil {
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

func newTestWorkspaceReconciler(c client.Client) *TauWorkspaceReconciler {
	return &TauWorkspaceReconciler{Client: c, APIReader: c}
}

type resourceMutationRecordingClient struct {
	client.Client
	mutations []string
}

type statusUpdateFailingClient struct {
	client.Client
	updateErr error
}

func (c *statusUpdateFailingClient) Status() client.SubResourceWriter {
	return &statusUpdateFailingWriter{SubResourceWriter: c.Client.Status(), client: c}
}

type statusUpdateFailingWriter struct {
	client.SubResourceWriter
	client *statusUpdateFailingClient
}

func (w *statusUpdateFailingWriter) Update(
	ctx context.Context,
	obj client.Object,
	opts ...client.SubResourceUpdateOption,
) error {
	if w.client.updateErr != nil {
		return w.client.updateErr
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

type namespaceMutationFailingClient struct {
	client.Client
	createErr error
	updateErr error
}

type roleUpdateDenyingClient struct {
	client.Client
	roleUpdates int
}

func (c *roleUpdateDenyingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if role, ok := obj.(*rbacv1.Role); ok {
		c.roleUpdates++
		return apierrors.NewForbidden(schema.GroupResource{
			Group:    rbacv1.GroupName,
			Resource: "roles",
		}, role.Name, errors.New("role updates are not permitted"))
	}
	return c.Client.Update(ctx, obj, opts...)
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
	reconcileWorkspaceTimes(t, reconciler, ctx, name, 3)
}

func reconcileWorkspaceTimes(t *testing.T, reconciler *TauWorkspaceReconciler, ctx context.Context, name string, count int) {
	t.Helper()
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: tauv1alpha1.SystemNamespace}}
	for i := 0; i < count; i++ {
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
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
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

func TestTeamWorkspaceWithoutQueueResolvesTauClusterDefault(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.Queue = ""
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, team, cluster, flavor).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, workspace.Name)

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatalf("Get workspace: %v", err)
	}
	if got.Status.Queue.LocalQueue != workspaceClusterQueueName(workspace.Name) ||
		got.Status.Queue.ClusterQueue != workspaceClusterQueueName(workspace.Name) {
		t.Fatalf("status.queue = %#v, want Team-backed queue", got.Status.Queue)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionTrue)
}

func TestNewTeamWorkspacePublishesProfileCompatibleQueue(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.Queue = "jobqueue"
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	source := testClusterQueue("jobqueue")
	source.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")},
			true,
		),
	}
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, team, source, flavor).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()

	reconcileWorkspaceTimes(t, newTestWorkspaceReconciler(c), ctx, workspace.Name, 5)

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	managedName := workspaceClusterQueueName(workspace.Name)
	if got.Status.Queue.LocalQueue != workspace.Spec.Queue ||
		got.Status.Queue.ClusterQueue != managedName {
		t.Fatalf("status.queue = %#v, want profile LocalQueue %q backed by %q", got.Status.Queue, workspace.Spec.Queue, managedName)
	}
	alias := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: workspace.Spec.Queue, Namespace: workspace.Name}, alias); err != nil {
		t.Fatal(err)
	}
	if backing, _, _ := unstructured.NestedString(alias.Object, "spec", "clusterQueue"); backing != managedName {
		t.Fatalf("profile LocalQueue backing = %q, want managed ClusterQueue %q", backing, managedName)
	}
	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: workspace.Name}, &namespace); err != nil {
		t.Fatal(err)
	}
	if namespace.Labels[labelKueueDefaultLocalQueue] != managedName {
		t.Fatalf("namespace default queue = %q, want managed route %q", namespace.Labels[labelKueueDefaultLocalQueue], managedName)
	}
}

func TestEmptyQuotaTeamWorkspaceWithoutQueueRetainsResolvedDefault(t *testing.T) {
	ctx := context.Background()
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.Queue = ""
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = nil
	team := testTeam("vision", "4")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	defaultQueue := testClusterQueue("jobqueue")
	defaultQueue.Object["spec"] = map[string]any{
		"resourceGroups": quotaResourceGroups(
			[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")},
			true,
		),
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(workspace, team, cluster, defaultQueue).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspaceTimes(t, reconciler, ctx, workspace.Name, 5)

	var got tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &got); err != nil {
		t.Fatal(err)
	}
	if got.Spec.Queue != "" {
		t.Fatalf("spec.queue = %q, want stored spec unchanged", got.Spec.Queue)
	}
	if got.Status.Queue.ClusterQueue != defaultQueue.GetName() {
		t.Fatalf("status.queue = %#v, want resolved default backing", got.Status.Queue)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQueueReady, metav1.ConditionTrue)
	managed := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, managed); !apierrors.IsNotFound(err) {
		t.Fatalf("managed ClusterQueue was created for inherited default: %v", err)
	}
}

func TestWorkspaceWithoutQueueOrClusterDefaultIsDegraded(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("aurora")
	workspace.Spec.Queue = ""
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	workspace.Status.Phase = tauv1alpha1.WorkspacePhaseReady
	workspace.Status.Target.ResolvedNamespace = workspace.Name
	workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
		LocalQueue:      "jobqueue",
		ClusterQueue:    "jobqueue",
		ClusterQueueUID: "jobqueue-uid",
	}
	namespace := testNamespace(workspace.Name)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, "jobqueue")
	namespace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	legacyQueue := testLocalQueue(workspace.Name, "jobqueue", "jobqueue")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, namespace, legacyQueue, clusterQueueWithUID("jobqueue", "jobqueue-uid")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	recordingClient := &resourceMutationRecordingClient{Client: c}
	reconciler := newTestWorkspaceReconciler(recordingClient)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("add finalizer: %v", err)
	}
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
	if !reflect.DeepEqual(got.Status.Queue, workspace.Status.Queue) {
		t.Fatalf("unresolved queue status = %#v, want preserved %#v", got.Status.Queue, workspace.Status.Queue)
	}

	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			WorkspaceDefaults: tauv1alpha1.TauClusterWorkspaceDefaults{DefaultQueue: "jobqueue"},
		},
	}
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, team); err != nil {
		t.Fatal(err)
	}
	if err := c.Create(ctx, flavor); err != nil {
		t.Fatal(err)
	}
	reconcileWorkspace(t, reconciler, ctx, workspace.Name)
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Status.Queue, workspace.Status.Queue) {
		t.Fatalf("recovered queue status = %#v, want preserved %#v", got.Status.Queue, workspace.Status.Queue)
	}
}

func testWorkspace(name string) *tauv1alpha1.TauWorkspace {
	return &tauv1alpha1.TauWorkspace{
		TypeMeta:   metav1.TypeMeta{APIVersion: tauv1alpha1.GroupVersion.String(), Kind: tauv1alpha1.KindTauWorkspace},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tauv1alpha1.SystemNamespace, UID: types.UID(name + "-uid")},
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
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tauv1alpha1.SystemNamespace},
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
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        name,
		UID:         types.UID(name + "-namespace-uid"),
		Labels:      map[string]string{labelWorkspace: name},
		Annotations: map[string]string{annotationOwnerUID: name + "-uid"},
	}}
}

func testRole(namespace, name, workspace string) *rbacv1.Role {
	return &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      workspaceLabels(workspace),
			Annotations: map[string]string{annotationOwnerUID: workspace + "-uid"},
		},
	}
}

func testRoleBinding(namespace, name, workspace string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      workspaceLabels(workspace),
			Annotations: map[string]string{annotationOwnerUID: workspace + "-uid"},
		},
	}
}

func testClusterRoleBinding(name, workspace string) *rbacv1.ClusterRoleBinding {
	return &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Labels:      workspaceLabels(workspace),
			Annotations: map[string]string{annotationOwnerUID: workspace + "-uid"},
		},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterQueueReaderRoleName},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: workspace + "-researchers"}},
	}
}

func testServiceAccount(namespace, name, workspace string) *corev1.ServiceAccount {
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      workspaceLabels(workspace),
			Annotations: map[string]string{annotationOwnerUID: workspace + "-uid"},
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
	obj.SetAnnotations(map[string]string{annotationOwnerUID: namespace + "-uid"})
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

func clusterQueueWithUID(name, uid string) *unstructured.Unstructured {
	queue := testClusterQueue(name)
	queue.SetUID(types.UID(uid))
	return queue
}

func assertAdmissionContract(t *testing.T, got, want *unstructured.Unstructured) {
	t.Helper()
	for _, field := range []string{"admissionChecks", "admissionChecksStrategy"} {
		gotValue, _, err := unstructured.NestedFieldNoCopy(got.Object, "spec", field)
		if err != nil {
			t.Fatal(err)
		}
		wantValue, _, err := unstructured.NestedFieldNoCopy(want.Object, "spec", field)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(gotValue, wantValue) {
			t.Fatalf("managed %s = %#v, want %#v", field, gotValue, wantValue)
		}
	}
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

func TestWorkspaceRequiresExplicitAdoptionOfRetainedNamespace(t *testing.T) {
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
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "aurora", Namespace: tauv1alpha1.SystemNamespace}}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	var reclaimed corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: "shared"}, &reclaimed); err != nil {
		t.Fatalf("namespace missing: %v", err)
	}
	if reclaimed.Labels[labelWorkspace] != "ghost" {
		t.Fatalf("retained namespace was adopted without operator action, labels = %#v", reclaimed.Labels)
	}
}

func TestWorkspaceRejectsNegativeQuotaBeforeCreatingNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("invalid-quota")
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{{
		Flavor:       "taugrid-gpu-h200",
		Resource:     nvidiaGPUResourceName,
		NominalQuota: resource.MustParse("-1"),
	}}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, workspace.Name)

	var namespace corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: workspace.Name}, &namespace); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid workspace created a namespace: %v", err)
	}
}

func TestWorkspaceRecreationCannotAdoptRetainedNamespaceFromOldUID(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	retained := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "aurora",
		Labels:      map[string]string{labelWorkspace: "aurora"},
		Annotations: map[string]string{annotationOwnerUID: "old-workspace-uid"},
	}}
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("new-workspace-uid")
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, retained).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}

	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: retained.Name}, &got); err != nil {
		t.Fatalf("Get retained namespace: %v", err)
	}
	if got.Annotations[annotationOwnerUID] != "old-workspace-uid" {
		t.Fatalf("retained namespace owner UID = %q, want old-workspace-uid", got.Annotations[annotationOwnerUID])
	}
	var binding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: retained.Name}, &binding); !apierrors.IsNotFound(err) {
		t.Fatalf("recreated workspace received retained namespace RBAC: %v", err)
	}
}

func TestWorkspaceRecreationCannotAdoptUnannotatedNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	namespaceCreated := metav1.NewTime(time.Now().Add(-time.Hour))
	workspaceCreated := metav1.NewTime(time.Now())
	retained := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:              "aurora",
		CreationTimestamp: namespaceCreated,
		Labels: map[string]string{
			labelWorkspace:              "aurora",
			labelWorkspaceLocalQueue:    "aurora",
			labelKueueDefaultLocalQueue: "aurora",
		},
	}}
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("new-workspace-uid")
	workspace.CreationTimestamp = workspaceCreated
	workspace.Spec.Target.CreateNamespace = false
	workspace.Spec.Authorization = &tauv1alpha1.WorkspaceAuthorization{Mode: tauv1alpha1.AuthorizationModeClusterWide}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, retained).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: retained.Name}, &got); err != nil {
		t.Fatalf("Get retained namespace: %v", err)
	}
	if _, exists := got.Annotations[annotationOwnerUID]; exists {
		t.Fatalf("unannotated namespace was claimed by recreated workspace: %#v", got.Annotations)
	}
	var binding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: retained.Name}, &binding); !apierrors.IsNotFound(err) {
		t.Fatalf("recreated workspace received unowned namespace RBAC: %v", err)
	}
}

func TestWorkspaceStatusCannotAuthorizeUnownedNamespace(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	retained := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "aurora",
		Labels: map[string]string{labelWorkspace: "aurora"},
	}}
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("new-workspace-uid")
	workspace.Status = tauv1alpha1.TauWorkspaceStatus{
		Phase:  tauv1alpha1.WorkspacePhaseDegraded,
		Target: tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: "aurora"},
		Conditions: []metav1.Condition{{
			Type:   tauv1alpha1.ConditionRBACReady,
			Status: metav1.ConditionFalse,
			Reason: "AdditionalWorkspaceBlocked",
		}},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, retained).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}

	for i := 0; i < 3; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: retained.Name}, &got); err != nil {
		t.Fatalf("Get retained namespace: %v", err)
	}
	if _, exists := got.Annotations[annotationOwnerUID]; exists {
		t.Fatalf("workspace status authorized namespace adoption: %#v", got.Annotations)
	}
	var binding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: retained.Name}, &binding); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace status authorized namespace RBAC: %v", err)
	}
}

func TestAdditionalWorkspacePreservesLegacyPrimaryDiscovery(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	incumbent := testWorkspace("research-primary")
	incumbent.Annotations = map[string]string{annotationV0Primary: "true"}
	incumbent.Status = tauv1alpha1.TauWorkspaceStatus{
		Phase:              tauv1alpha1.WorkspacePhaseReady,
		ObservedGeneration: incumbent.Generation,
		Target:             tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: "research"},
	}
	additional := testWorkspace("taugrid-default")
	additional.Finalizers = []string{workspaceFinalizer}
	additional.Status = tauv1alpha1.TauWorkspaceStatus{
		Phase: tauv1alpha1.WorkspacePhaseDegraded,
		Conditions: []metav1.Condition{{
			Type:   tauv1alpha1.ConditionRBACReady,
			Status: metav1.ConditionFalse,
			Reason: reasonAdditionalWorkspaceBlocked,
		}},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(incumbent, additional, testClusterQueue(additional.Spec.Queue)).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspaceTimes(t, reconciler, ctx, additional.Name, 4)

	var gotAdditional tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(additional), &gotAdditional); err != nil {
		t.Fatalf("Get additional workspace: %v", err)
	}
	if gotAdditional.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
		t.Fatalf("additional workspace phase = %q, want Ready", gotAdditional.Status.Phase)
	}
	compatibility := findCondition(gotAdditional.Status.Conditions, conditionV0PrimaryDiscovery)
	if compatibility == nil || compatibility.Reason != reasonAdditionalWorkspaceBlocked {
		t.Fatalf("legacy discovery compatibility condition = %#v", compatibility)
	}
	if legacyWorkspaceHasPrimaryHistory(&gotAdditional) {
		t.Fatal("old CLI would treat the active additional workspace as primary")
	}

	var gotIncumbent tauv1alpha1.TauWorkspace
	if err := c.Get(ctx, client.ObjectKeyFromObject(incumbent), &gotIncumbent); err != nil {
		t.Fatalf("Get incumbent workspace: %v", err)
	}
	selected := legacyPrimaryWorkspace(&gotAdditional, []tauv1alpha1.TauWorkspace{gotAdditional, gotIncumbent})
	if selected.Name != incumbent.Name {
		t.Fatalf("old CLI selected %q, want marked incumbent %q", selected.Name, incumbent.Name)
	}

	for _, job := range []*batchv1.Job{
		{ObjectMeta: metav1.ObjectMeta{Name: "training", Namespace: "research", UID: types.UID("incumbent-job")}},
		{ObjectMeta: metav1.ObjectMeta{Name: "training", Namespace: additional.Name, UID: types.UID("additional-job")}},
	} {
		if err := c.Create(ctx, job); err != nil {
			t.Fatalf("Create job %s/%s: %v", job.Namespace, job.Name, err)
		}
	}
	var selectedJob batchv1.Job
	if err := c.Get(ctx, client.ObjectKey{Name: "training", Namespace: selected.Status.Target.ResolvedNamespace}, &selectedJob); err != nil {
		t.Fatalf("Get selected job: %v", err)
	}
	if selectedJob.UID != types.UID("incumbent-job") {
		t.Fatalf("old context-only lifecycle routing selected job UID %q", selectedJob.UID)
	}
}

func legacyWorkspaceHasPrimaryHistory(workspace *tauv1alpha1.TauWorkspace) bool {
	if workspace.Status.Phase == "" &&
		workspace.Status.ObservedGeneration == 0 &&
		len(workspace.Status.Conditions) == 0 {
		return false
	}
	for _, condition := range workspace.Status.Conditions {
		if condition.Reason == reasonAdditionalWorkspaceBlocked {
			return false
		}
	}
	return true
}

func legacyPrimaryWorkspace(named *tauv1alpha1.TauWorkspace, listed []tauv1alpha1.TauWorkspace) *tauv1alpha1.TauWorkspace {
	if legacyWorkspaceHasPrimaryHistory(named) {
		return named
	}
	for i := range listed {
		if listed[i].Annotations[annotationV0Primary] == "true" {
			return &listed[i]
		}
	}
	return nil
}

func TestExplicitAdoptionEvidenceMustMatchNamespaceUID(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	retained := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "aurora",
		UID:    types.UID("namespace-uid"),
		Labels: map[string]string{labelWorkspace: "aurora"},
		Annotations: map[string]string{
			annotationOwnerUID: "old-workspace-uid",
		},
	}}
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("new-workspace-uid")
	workspace.Annotations = map[string]string{annotationAdoptionNamespaceUID: "namespace-uid"}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, retained, testClusterQueue("aurora")).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, workspace.Name)

	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: retained.Name}, &got); err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got.Annotations[annotationOwnerUID] != string(workspace.UID) {
		t.Fatalf("namespace owner UID = %q, want %q", got.Annotations[annotationOwnerUID], workspace.UID)
	}
}

func TestExplicitAdoptionEvidenceRejectsDifferentNamespaceUID(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	retained := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:        "aurora",
		UID:         types.UID("namespace-uid"),
		Labels:      map[string]string{labelWorkspace: "aurora"},
		Annotations: map[string]string{annotationOwnerUID: "old-workspace-uid"},
	}}
	workspace := testWorkspace("aurora")
	workspace.UID = types.UID("new-workspace-uid")
	workspace.Annotations = map[string]string{annotationAdoptionNamespaceUID: "different-namespace-uid"}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(workspace, retained).
		WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).
		Build()
	reconciler := newTestWorkspaceReconciler(c)

	reconcileWorkspace(t, reconciler, ctx, workspace.Name)

	var got corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: retained.Name}, &got); err != nil {
		t.Fatalf("Get namespace: %v", err)
	}
	if got.Annotations[annotationOwnerUID] != "old-workspace-uid" {
		t.Fatalf("mismatched adoption evidence changed owner UID to %q", got.Annotations[annotationOwnerUID])
	}
	var binding rbacv1.RoleBinding
	if err := c.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: retained.Name}, &binding); !apierrors.IsNotFound(err) {
		t.Fatalf("mismatched adoption evidence created RBAC: %v", err)
	}
}
