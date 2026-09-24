// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTauClusterDiscoversManagedAzureGPURegion(t *testing.T) {
	ctx := context.Background()
	cluster := topologyTestCluster()
	node := topologyTestNode("managed-h200", map[string]string{
		labelRegion: "centralus",
	}, "azure:///subscriptions/test/resourceGroups/nodes/providers/Microsoft.Compute/virtualMachines/managed-h200")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, node).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recording := &resourceMutationRecordingClient{Client: c}
	reconciler := &TauClusterReconciler{Client: recording}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var gotNode corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node.Name}, &gotNode); err != nil {
		t.Fatalf("Get Node: %v", err)
	}
	wantLabels := map[string]string{
		labelkeys.LabelRegion:        "centralus",
		labelkeys.LabelNetworkDomain: "azure-centralus",
		labelkeys.LabelInfiniband:    "true",
	}
	if !nodeHasLabels(&gotNode, wantLabels) {
		t.Fatalf("topology labels = %#v, want %#v", gotNode.Labels, wantLabels)
	}

	topology := newQueueObject(topologyGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: tauGPUNodeTopologyName}, topology); err != nil {
		t.Fatalf("Get Topology: %v", err)
	}
	levels, found, err := unstructured.NestedSlice(topology.Object, "spec", "levels")
	if err != nil || !found {
		t.Fatalf("Topology levels: found=%v err=%v", found, err)
	}
	wantLevels := []any{
		map[string]any{"nodeLabel": labelkeys.LabelRegion},
		map[string]any{"nodeLabel": labelkeys.LabelNetworkDomain},
		map[string]any{"nodeLabel": labelHostname},
	}
	if !reflect.DeepEqual(levels, wantLevels) {
		t.Fatalf("Topology levels = %#v, want %#v", levels, wantLevels)
	}

	var gotCluster tauv1alpha1.TauCluster
	if err := c.Get(ctx, client.ObjectKey{Name: cluster.Name}, &gotCluster); err != nil {
		t.Fatalf("Get TauCluster: %v", err)
	}
	assertCondition(t, gotCluster.Status.Conditions, tauv1alpha1.ConditionQueuesReady, metav1.ConditionTrue)
	assertCondition(t, gotCluster.Status.Conditions, tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse)
	if len(gotCluster.Status.ManagedResources) != 1 ||
		gotCluster.Status.ManagedResources[0].Name != tauGPUNodeTopologyName {
		t.Fatalf("managed resources = %#v", gotCluster.Status.ManagedResources)
	}

	recording.mutations = nil
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(recording.mutations) != 0 {
		t.Fatalf("idempotent reconcile mutations = %v", recording.mutations)
	}
}

func TestTauClusterDiscoversAzureFlexInfiniBandDomain(t *testing.T) {
	cluster := topologyTestCluster()
	node := topologyTestNode("flex-h200", map[string]string{
		labelAKSCloud:      "azure",
		labelAKSRegion:     "eastus2",
		labelAzureManaged:  "false",
		labelFlexSite:      "research-site",
		labelAKSInfiniband: "true",
	}, "")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, node).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	reconciler := &TauClusterReconciler{Client: c}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatalf("Get Node: %v", err)
	}
	wantLabels := map[string]string{
		labelkeys.LabelRegion:        "eastus2",
		labelkeys.LabelNetworkDomain: "azure-site-research-site",
		labelkeys.LabelInfiniband:    "true",
	}
	if !nodeHasLabels(&got, wantLabels) {
		t.Fatalf("topology labels = %#v, want %#v", got.Labels, wantLabels)
	}
}

func TestTauClusterIsolatesAzureFlexNodesWithoutInfiniBand(t *testing.T) {
	cluster := topologyTestCluster()
	first := topologyTestNode("flex-a", map[string]string{
		labelAKSCloud:      "azure",
		labelAKSRegion:     "westus3",
		labelAzureManaged:  "false",
		labelAKSInfiniband: "false",
	}, "")
	second := topologyTestNode("flex-b", map[string]string{
		labelAKSCloud:      "azure",
		labelAKSRegion:     "westus3",
		labelAzureManaged:  "false",
		labelAKSInfiniband: "false",
	}, "")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, first, second).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	reconciler := &TauClusterReconciler{Client: c}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	domains := map[string]string{}
	for _, name := range []string{first.Name, second.Name} {
		var got corev1.Node
		if err := c.Get(context.Background(), client.ObjectKey{Name: name}, &got); err != nil {
			t.Fatalf("Get Node %q: %v", name, err)
		}
		if got.Labels[labelkeys.LabelRegion] != "westus3" || got.Labels[labelkeys.LabelInfiniband] != "false" {
			t.Fatalf("Node %q topology labels = %#v", name, got.Labels)
		}
		domains[name] = got.Labels[labelkeys.LabelNetworkDomain]
	}
	if domains[first.Name] == domains[second.Name] ||
		domains[first.Name] == "" || domains[second.Name] == "" {
		t.Fatalf("non-IB network domains = %#v, want distinct singleton domains", domains)
	}
}

func TestTauClusterRequiresAzureFlexInfiniBandDeclaration(t *testing.T) {
	cluster := topologyTestCluster()
	node := topologyTestNode("flex-h200", map[string]string{
		labelAKSCloud:     "azure",
		labelAKSRegion:    "eastus2",
		labelAzureManaged: "false",
		labelFlexSite:     "research-site",
	}, "")
	topology := desiredTauGPUTopology()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, node, topology).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recording := &resourceMutationRecordingClient{Client: c}
	reconciler := &TauClusterReconciler{Client: recording}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err == nil {
		t.Fatal("Reconcile() accepted an Azure Flex GPU node without an InfiniBand declaration")
	}
	if len(recording.mutations) != 0 {
		t.Fatalf("invalid Flex capability caused mutations: %v", recording.mutations)
	}
}

func TestTauClusterRemovesTopologyLabelsFromNonAzureNode(t *testing.T) {
	cluster := topologyTestCluster()
	node := topologyTestNode("aws-h200", map[string]string{
		labelAKSCloud:                "aws",
		labelkeys.LabelRegion:        "eastus2",
		labelkeys.LabelNetworkDomain: "azure-eastus2",
		labelkeys.LabelInfiniband:    "true",
	}, "aws:///us-east-1/i-test")
	topology := desiredTauGPUTopology()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, node, topology).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	reconciler := &TauClusterReconciler{Client: c}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatalf("Get Node: %v", err)
	}
	if hasManagedTopologyLabels(&got) {
		t.Fatalf("stale topology labels = %#v", got.Labels)
	}
}

func TestTauClusterReportsForeignTopologyOwnership(t *testing.T) {
	cluster := topologyTestCluster()
	node := topologyTestNode("managed-h200", map[string]string{labelRegion: "eastus2"}, "azure:///managed-h200")
	foreign := desiredTauGPUTopology()
	foreign.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "gitops"})
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, node, foreign).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recording := &resourceMutationRecordingClient{Client: c}
	reconciler := &TauClusterReconciler{Client: recording}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var got tauv1alpha1.TauCluster
	if err := c.Get(context.Background(), client.ObjectKey{Name: cluster.Name}, &got); err != nil {
		t.Fatalf("Get TauCluster: %v", err)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionTrue)
	if got.Status.Phase != tauv1alpha1.ClusterPhaseDegraded {
		t.Fatalf("phase = %q, want %q", got.Status.Phase, tauv1alpha1.ClusterPhaseDegraded)
	}
	var gotNode corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: node.Name}, &gotNode); err != nil {
		t.Fatalf("Get Node: %v", err)
	}
	if hasManagedTopologyLabels(&gotNode) {
		t.Fatalf("foreign Topology conflict mutated Node labels: %#v", gotNode.Labels)
	}
}

func TestAzureFlexDomainUsesValidLabelForLongSiteName(t *testing.T) {
	site := strings.Repeat("a", validation.DNS1123LabelMaxLength)
	node := topologyTestNode("flex-h200", map[string]string{
		labelAKSCloud:      "azure",
		labelAKSRegion:     "eastus2",
		labelAzureManaged:  "false",
		labelFlexSite:      site,
		labelAKSInfiniband: "true",
	}, "")
	labels, eligible, err := desiredAzureNodeTopologyLabels(node)
	if err != nil || !eligible {
		t.Fatalf("desiredAzureNodeTopologyLabels() eligible=%v err=%v", eligible, err)
	}
	domain := labels[labelkeys.LabelNetworkDomain]
	if problems := validation.IsValidLabelValue(domain); len(problems) > 0 {
		t.Fatalf("network domain %q is invalid: %v", domain, problems)
	}
	if domain != networkDomainLabel("azure-site", site) {
		t.Fatalf("network domain = %q, want deterministic helper result", domain)
	}
}

func TestAzureNodeDetectionUsesAuthoritativeIdentityLabels(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		provider string
		azure    bool
		external bool
	}{
		{
			name:     "cloud label",
			labels:   map[string]string{labelAKSCloud: "AZURE"},
			azure:    true,
			external: false,
		},
		{
			name:     "provider ID",
			provider: "AZURE:///subscriptions/test",
			azure:    true,
			external: false,
		},
		{
			name:     "managed AKS marker",
			labels:   map[string]string{labelAzureManagedCluster: "cluster"},
			azure:    true,
			external: false,
		},
		{
			name:     "external managed marker",
			labels:   map[string]string{labelAKSCloud: "azure", labelAzureManaged: "FALSE"},
			azure:    true,
			external: true,
		},
		{
			name:     "stretch managed marker",
			labels:   map[string]string{labelAKSCloud: "azure", labelStretchManaged: "TRUE"},
			azure:    true,
			external: true,
		},
		{
			name:     "non Azure cloud overrides provider ID",
			labels:   map[string]string{labelAKSCloud: "aws", labelAzureManagedCluster: "cluster"},
			provider: "azure:///subscriptions/test",
			azure:    false,
			external: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{Labels: tt.labels},
				Spec:       corev1.NodeSpec{ProviderID: tt.provider},
			}
			if got := isAzureNode(node); got != tt.azure {
				t.Fatalf("isAzureNode() = %v, want %v", got, tt.azure)
			}
			if got := isExternalAzureNode(node); got != tt.external {
				t.Fatalf("isExternalAzureNode() = %v, want %v", got, tt.external)
			}
		})
	}
}

func topologyTestCluster() *tauv1alpha1.TauCluster {
	return &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			ManagementMode: tauv1alpha1.ClusterManagementModeReconcile,
		},
	}
}

func topologyTestNode(name string, labels map[string]string, providerID string) *corev1.Node {
	if labels == nil {
		labels = map[string]string{}
	}
	labels[labelHostname] = name
	labels[labelkeys.LabelGPUClass] = "h200-141gb"
	labels["kubernetes.io/os"] = "linux"
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       corev1.NodeSpec{ProviderID: providerID},
	}
}
