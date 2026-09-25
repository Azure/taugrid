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
		labelRegion:       "centralus",
		azureVMSizeLabel:  "Standard_ND96isr_H200_v5",
		labelAKSAgentPool: "research",
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
		labelkeys.LabelSite:          "azure-centralus",
		labelkeys.LabelRegion:        "centralus",
		labelkeys.LabelNetworkDomain: "azure-ib-centralus-research-standard_nd96isr_h200_v5",
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
		map[string]any{"nodeLabel": labelkeys.LabelSite},
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
		labelAKSCloud:          "azure",
		labelAKSRegion:         "eastus2",
		labelAzureManaged:      "false",
		labelFlexSite:          "research-site",
		labelFlexNetworkDomain: "research-fabric",
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
		labelkeys.LabelSite:          "azure-site-research-site",
		labelkeys.LabelRegion:        "eastus2",
		labelkeys.LabelNetworkDomain: "azure-fabric-research-fabric",
		labelkeys.LabelInfiniband:    "true",
	}
	if !nodeHasLabels(&got, wantLabels) {
		t.Fatalf("topology labels = %#v, want %#v", got.Labels, wantLabels)
	}
}

func TestTauClusterIsolatesAzureFlexNodesWithoutInfiniBand(t *testing.T) {
	cluster := topologyTestCluster()
	first := topologyTestNode("flex-a", map[string]string{
		labelAKSCloud:     "azure",
		labelAKSRegion:    "westus3",
		labelAzureManaged: "false",
		labelFlexSite:     "batch-site",
	}, "")
	second := topologyTestNode("flex-b", map[string]string{
		labelAKSCloud:     "azure",
		labelAKSRegion:    "westus3",
		labelAzureManaged: "false",
		labelFlexSite:     "batch-site",
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
		if got.Labels[labelkeys.LabelSite] != "azure-site-batch-site" ||
			got.Labels[labelkeys.LabelRegion] != "westus3" ||
			got.Labels[labelkeys.LabelInfiniband] != "false" {
			t.Fatalf("Node %q topology labels = %#v", name, got.Labels)
		}
		domains[name] = got.Labels[labelkeys.LabelNetworkDomain]
	}
	if domains[first.Name] == domains[second.Name] ||
		domains[first.Name] == "" || domains[second.Name] == "" {
		t.Fatalf("non-IB network domains = %#v, want distinct singleton domains", domains)
	}
}

func TestTauClusterAllowsAzureFlexSiteWithoutFabric(t *testing.T) {
	cluster := topologyTestCluster()
	flex := topologyTestNode("flex-h200", map[string]string{
		labelAKSCloud:     "azure",
		labelAKSRegion:    "eastus2",
		labelAzureManaged: "false",
		labelFlexSite:     "research-site",
	}, "")
	valid := topologyTestNode("aws-cpu", map[string]string{
		labelAKSCloud:  "aws",
		labelAKSRegion: "us-east-1",
	}, "aws:///us-east-1/i-test")
	delete(valid.Labels, labelkeys.LabelGPUClass)
	topology := desiredTauGPUTopology()
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, flex, valid, topology).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	reconciler := &TauClusterReconciler{Client: c}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	var gotValid corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: valid.Name}, &gotValid); err != nil {
		t.Fatalf("Get valid Node: %v", err)
	}
	if gotValid.Labels[labelkeys.LabelSite] == "" ||
		gotValid.Labels[labelkeys.LabelNetworkDomain] == "" {
		t.Fatalf("Flex site reconciliation blocked valid Node reconciliation: %#v", gotValid.Labels)
	}
	var gotFlex corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: flex.Name}, &gotFlex); err != nil {
		t.Fatalf("Get Flex Node: %v", err)
	}
	if gotFlex.Labels[labelkeys.LabelSite] != "azure-site-research-site" ||
		gotFlex.Labels[labelkeys.LabelNetworkDomain] != isolatedTopologyLabel("isolated-domain", flex.Name) ||
		gotFlex.Labels[labelkeys.LabelInfiniband] != "false" {
		t.Fatalf("Flex Node without a fabric was not isolated within its site: %#v", gotFlex.Labels)
	}
}

func TestTauClusterRewritesStaleTopologyLabelsOnNonAzureNode(t *testing.T) {
	cluster := topologyTestCluster()
	node := topologyTestNode("aws-h200", map[string]string{
		labelAKSCloud:                "aws",
		labelkeys.LabelSite:          "azure-eastus2",
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
	want := map[string]string{
		labelkeys.LabelSite:          isolatedTopologyLabel("isolated-site", node.Name),
		labelkeys.LabelRegion:        isolatedTopologyLabel("unplaced", node.Name),
		labelkeys.LabelNetworkDomain: isolatedTopologyLabel("isolated-domain", node.Name),
		labelkeys.LabelInfiniband:    "false",
	}
	if !nodeHasLabels(&got, want) {
		t.Fatalf("topology labels = %#v, want %#v", got.Labels, want)
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
		labelAKSCloud:          "azure",
		labelAKSRegion:         "eastus2",
		labelAzureManaged:      "false",
		labelFlexSite:          site,
		labelFlexNetworkDomain: site,
	}, "")
	labels, err := desiredNodeTopologyLabels(node)
	if err != nil {
		t.Fatalf("desiredNodeTopologyLabels() error = %v", err)
	}
	domain := labels[labelkeys.LabelNetworkDomain]
	if problems := validation.IsValidLabelValue(domain); len(problems) > 0 {
		t.Fatalf("network domain %q is invalid: %v", domain, problems)
	}
	if domain != networkDomainLabel("azure-fabric", site) {
		t.Fatalf("network domain = %q, want deterministic helper result", domain)
	}
}

func TestTauClusterReconcilesSampleFlexCluster(t *testing.T) {
	cluster := topologyTestCluster()
	ibNodes := []*corev1.Node{
		sampleFlexNode("flex-a100-a", "Standard_ND96amsr_A100_v4", "a100-80gb", "research-flex-eastus2", "azure-a100"),
		sampleFlexNode("flex-a100-b", "Standard_ND96amsr_A100_v4", "a100-80gb", "research-flex-eastus2", "azure-a100"),
		sampleFlexNode("flex-h100-a", "Standard_ND96isr_H100_v5", "h100-80gb", "research-flex-eastus2", "azure-h100"),
		sampleFlexNode("flex-h100-b", "Standard_ND96isr_H100_v5", "h100-80gb", "research-flex-eastus2", "azure-h100"),
		sampleFlexNode("flex-h200-a", "Standard_ND96isr_H200_v5", "h200-141gb", "research-flex-eastus2", "azure-h200"),
		sampleFlexNode("flex-h200-b", "Standard_ND96isr_H200_v5", "h200-141gb", "research-flex-eastus2", "azure-h200"),
	}
	secondSiteH200s := []*corev1.Node{
		sampleNebiusFlexNode("flex-h200-nebius-a", "gpu-h200-sxm", "partner-flex-finland"),
		sampleNebiusFlexNode("flex-h200-nebius-b", "gpu-h200-sxm", "partner-flex-finland"),
	}
	nonIB := sampleFlexNode("flex-a10", "Standard_NV36ads_A10_v5", "a10-24gb", "batch-flex-eastus2", "")

	objects := []client.Object{cluster, nonIB}
	for _, node := range ibNodes {
		objects = append(objects, node)
	}
	for _, node := range secondSiteH200s {
		objects = append(objects, node)
	}
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recording := &resourceMutationRecordingClient{Client: c}
	reconciler := &TauClusterReconciler{Client: recording}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	wantDomains := map[string]string{
		"a100-80gb":  "azure-fabric-azure-a100",
		"h100-80gb":  "azure-fabric-azure-h100",
		"h200-141gb": "azure-fabric-azure-h200",
	}
	for _, node := range ibNodes {
		var got corev1.Node
		if err := c.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
			t.Fatalf("Get Node %q: %v", node.Name, err)
		}
		want := map[string]string{
			labelkeys.LabelSite:          "azure-site-research-flex-eastus2",
			labelkeys.LabelRegion:        "eastus2",
			labelkeys.LabelNetworkDomain: wantDomains[node.Labels[labelkeys.LabelGPUClass]],
			labelkeys.LabelInfiniband:    "true",
		}
		if !nodeHasLabels(&got, want) {
			t.Fatalf("Node %q labels = %#v, want %#v", node.Name, got.Labels, want)
		}
		if got.Labels[labelkeys.LabelGPUClass] != node.Labels[labelkeys.LabelGPUClass] {
			t.Fatalf("Node %q GPU class = %q, want %q", node.Name, got.Labels[labelkeys.LabelGPUClass], node.Labels[labelkeys.LabelGPUClass])
		}
	}
	wantSecondSite := map[string]string{
		labelkeys.LabelSite:          "nebius-site-partner-flex-finland",
		labelkeys.LabelRegion:        "eu-north1",
		labelkeys.LabelNetworkDomain: "nebius-fabric-nebius-h200",
		labelkeys.LabelInfiniband:    "true",
	}
	for _, node := range secondSiteH200s {
		var got corev1.Node
		if err := c.Get(context.Background(), client.ObjectKey{Name: node.Name}, &got); err != nil {
			t.Fatalf("Get Node %q: %v", node.Name, err)
		}
		if !nodeHasLabels(&got, wantSecondSite) {
			t.Fatalf("Node %q labels = %#v, want %#v", node.Name, got.Labels, wantSecondSite)
		}
	}
	if wantDomains["h200-141gb"] == wantSecondSite[labelkeys.LabelNetworkDomain] {
		t.Fatal("H200 Nodes in different sites received the same network domain")
	}
	var gotNonIB corev1.Node
	if err := c.Get(context.Background(), client.ObjectKey{Name: nonIB.Name}, &gotNonIB); err != nil {
		t.Fatalf("Get Node %q: %v", nonIB.Name, err)
	}
	if gotNonIB.Labels[labelkeys.LabelSite] != "azure-site-batch-flex-eastus2" ||
		gotNonIB.Labels[labelkeys.LabelInfiniband] != "false" ||
		gotNonIB.Labels[labelkeys.LabelNetworkDomain] != isolatedTopologyLabel("isolated-domain", nonIB.Name) {
		t.Fatalf("non-IB Flex labels = %#v", gotNonIB.Labels)
	}
	topology := newQueueObject(topologyGVK)
	if err := c.Get(context.Background(), client.ObjectKey{Name: tauGPUNodeTopologyName}, topology); err != nil {
		t.Fatalf("Get Topology: %v", err)
	}
	levels, found, err := unstructured.NestedSlice(topology.Object, "spec", "levels")
	if err != nil || !found {
		t.Fatalf("Topology levels: found=%v err=%v", found, err)
	}
	wantLevels := []any{
		map[string]any{"nodeLabel": labelkeys.LabelSite},
		map[string]any{"nodeLabel": labelkeys.LabelNetworkDomain},
		map[string]any{"nodeLabel": labelHostname},
	}
	if !reflect.DeepEqual(levels, wantLevels) {
		t.Fatalf("Topology levels = %#v, want %#v", levels, wantLevels)
	}

	recording.mutations = nil
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if len(recording.mutations) != 0 {
		t.Fatalf("idempotent Flex cluster reconcile mutations = %v", recording.mutations)
	}
}

func sampleFlexNode(name, sku, gpuClass, site, networkDomain string) *corev1.Node {
	node := topologyTestNode(name, map[string]string{
		labelAKSCloud:                           "azure",
		labelAKSRegion:                          "eastus2",
		labelAzureManaged:                       "false",
		labelStretchManaged:                     "true",
		"kubernetes.azure.com/cluster":          "flex-research",
		"node.kubernetes.io/instance-type":      sku,
		"aks.azure.com/instance-type":           sku,
		labelFlexSite:                           site,
		"kubernetes.azure.com/agentpool":        "research-gpu",
		"kubernetes.azure.com/nodepool-type":    "FlexNodes",
		"kubernetes.azure.com/mode":             "user",
		"kubernetes.azure.com/os-sku":           "Ubuntu",
		"kubernetes.azure.com/os-sku-effective": "Ubuntu2404",
	}, "")
	if networkDomain != "" {
		node.Labels[labelFlexNetworkDomain] = networkDomain
	}
	node.Labels[labelkeys.LabelGPUClass] = gpuClass
	return node
}

func sampleNebiusFlexNode(name, instanceType, site string) *corev1.Node {
	node := topologyTestNode(name, map[string]string{
		labelAKSCloud:                        "nebius",
		labelAKSRegion:                       "eu-north1",
		labelAzureManaged:                    "false",
		labelStretchManaged:                  "true",
		"kubernetes.azure.com/cluster":       "flex-research",
		"node.kubernetes.io/instance-type":   instanceType,
		"aks.azure.com/instance-type":        instanceType,
		labelFlexSite:                        site,
		labelFlexNetworkDomain:               "nebius-h200",
		"kubernetes.azure.com/nodepool-type": "FlexNodes",
	}, "")
	node.Labels[labelkeys.LabelGPUClass] = "h200-141gb"
	return node
}

func TestDesiredNodeTopologyLabelsEdgeCases(t *testing.T) {
	tests := []struct {
		name      string
		node      *corev1.Node
		want      map[string]string
		wantError bool
	}{
		{
			name: "managed Azure ND pool derives shared fabric",
			node: topologyTestNode("azure-nd-a", map[string]string{
				labelRegion:       "eastus2",
				azureVMSizeLabel:  "Standard_ND96isr_H200_v5",
				labelAKSAgentPool: "research",
			}, "azure:///azure-nd-a"),
			want: map[string]string{
				labelkeys.LabelSite:          "azure-eastus2",
				labelkeys.LabelRegion:        "eastus2",
				labelkeys.LabelNetworkDomain: "azure-ib-eastus2-research-standard_nd96isr_h200_v5",
				labelkeys.LabelInfiniband:    "true",
			},
		},
		{
			name: "managed Azure ND without agent pool is isolated",
			node: topologyTestNode("azure-nd-no-pool", map[string]string{
				labelRegion:      "eastus2",
				azureVMSizeLabel: "Standard_ND96isr_H200_v5",
			}, "azure:///azure-nd-no-pool"),
			want: map[string]string{
				labelkeys.LabelSite:          "azure-eastus2",
				labelkeys.LabelRegion:        "eastus2",
				labelkeys.LabelNetworkDomain: isolatedTopologyLabel("isolated-domain", "azure-nd-no-pool"),
				labelkeys.LabelInfiniband:    "false",
			},
		},
		{
			name: "managed Azure non-ND GPU is isolated",
			node: topologyTestNode("azure-a10", map[string]string{
				labelRegion:       "eastus2",
				azureVMSizeLabel:  "Standard_NV36ads_A10_v5",
				labelAKSAgentPool: "batch",
			}, "azure:///azure-a10"),
			want: map[string]string{
				labelkeys.LabelSite:          "azure-eastus2",
				labelkeys.LabelRegion:        "eastus2",
				labelkeys.LabelNetworkDomain: isolatedTopologyLabel("isolated-domain", "azure-a10"),
				labelkeys.LabelInfiniband:    "false",
			},
		},
		{
			name: "provider namespaces explicit site and fabric",
			node: topologyTestNode("nebius-h200", map[string]string{
				labelAKSCloud:          "nebius",
				labelAKSRegion:         "eu-north1",
				labelFlexSite:          "research",
				labelFlexNetworkDomain: "h200-fabric",
			}, ""),
			want: map[string]string{
				labelkeys.LabelSite:          "nebius-site-research",
				labelkeys.LabelRegion:        "eu-north1",
				labelkeys.LabelNetworkDomain: "nebius-fabric-h200-fabric",
				labelkeys.LabelInfiniband:    "true",
			},
		},
		{
			name: "same explicit names remain provider isolated",
			node: topologyTestNode("aws-h200", map[string]string{
				labelAKSCloud:          "aws",
				labelAKSRegion:         "us-east-1",
				labelFlexSite:          "research",
				labelFlexNetworkDomain: "h200-fabric",
			}, ""),
			want: map[string]string{
				labelkeys.LabelSite:          "aws-site-research",
				labelkeys.LabelRegion:        "us-east-1",
				labelkeys.LabelNetworkDomain: "aws-fabric-h200-fabric",
				labelkeys.LabelInfiniband:    "true",
			},
		},
		{
			name: "external Azure without site fails closed",
			node: topologyTestNode("azure-flex-missing-site", map[string]string{
				labelAKSCloud:     "azure",
				labelAKSRegion:    "eastus2",
				labelAzureManaged: "false",
			}, ""),
			want: map[string]string{
				labelkeys.LabelSite:          isolatedTopologyLabel("isolated-site", "azure-flex-missing-site"),
				labelkeys.LabelRegion:        "eastus2",
				labelkeys.LabelNetworkDomain: isolatedTopologyLabel("isolated-domain", "azure-flex-missing-site"),
				labelkeys.LabelInfiniband:    "false",
			},
			wantError: true,
		},
		{
			name: "invalid explicit fabric fails closed",
			node: topologyTestNode("invalid-fabric", map[string]string{
				labelAKSCloud:          "nebius",
				labelAKSRegion:         "eu-north1",
				labelFlexSite:          "research",
				labelFlexNetworkDomain: strings.Repeat("x", validation.LabelValueMaxLength+1),
			}, ""),
			want: map[string]string{
				labelkeys.LabelSite:          isolatedTopologyLabel("isolated-site", "invalid-fabric"),
				labelkeys.LabelRegion:        "eu-north1",
				labelkeys.LabelNetworkDomain: isolatedTopologyLabel("isolated-domain", "invalid-fabric"),
				labelkeys.LabelInfiniband:    "false",
			},
			wantError: true,
		},
		{
			name: "unknown provider without topology is isolated",
			node: topologyTestNode("unknown-provider", map[string]string{
				labelRegion: "moon-1",
			}, "custom:///unknown-provider"),
			want: map[string]string{
				labelkeys.LabelSite:          isolatedTopologyLabel("isolated-site", "unknown-provider"),
				labelkeys.LabelRegion:        "moon-1",
				labelkeys.LabelNetworkDomain: isolatedTopologyLabel("isolated-domain", "unknown-provider"),
				labelkeys.LabelInfiniband:    "false",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := desiredNodeTopologyLabels(tt.node)
			if (err != nil) != tt.wantError {
				t.Fatalf("desiredNodeTopologyLabels() error = %v, wantError %v", err, tt.wantError)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("desiredNodeTopologyLabels() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestManagedAzureFabricIdentityBoundaries(t *testing.T) {
	node := func(name, region, pool, sku string) *corev1.Node {
		return topologyTestNode(name, map[string]string{
			labelRegion:       region,
			labelAKSAgentPool: pool,
			azureVMSizeLabel:  sku,
		}, "azure:///"+name)
	}
	domain := func(t *testing.T, node *corev1.Node) string {
		t.Helper()
		labels, err := desiredNodeTopologyLabels(node)
		if err != nil {
			t.Fatal(err)
		}
		if labels[labelkeys.LabelInfiniband] != "true" {
			t.Fatalf("Node %q was not classified as InfiniBand: %#v", node.Name, labels)
		}
		return labels[labelkeys.LabelNetworkDomain]
	}

	base := domain(t, node("base", "eastus2", "research", "Standard_ND96isr_H200_v5"))
	if got := domain(t, node("peer", "eastus2", "research", "Standard_ND96isr_H200_v5")); got != base {
		t.Fatalf("identical region/pool/SKU domains differ: %q != %q", got, base)
	}
	for _, tt := range []struct {
		name   string
		region string
		pool   string
		sku    string
	}{
		{name: "different region", region: "centralus", pool: "research", sku: "Standard_ND96isr_H200_v5"},
		{name: "different pool", region: "eastus2", pool: "training", sku: "Standard_ND96isr_H200_v5"},
		{name: "different SKU", region: "eastus2", pool: "research", sku: "Standard_ND96isr_H100_v5"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := domain(t, node(tt.name, tt.region, tt.pool, tt.sku)); got == base {
				t.Fatalf("changed fabric identity reused domain %q", got)
			}
		})
	}
}

func TestTauClusterDowngradesManagedAzureNodeWhenSKUIsNotNDCapable(t *testing.T) {
	ctx := context.Background()
	cluster := topologyTestCluster()
	node := topologyTestNode("managed-gpu", map[string]string{
		labelRegion:       "eastus2",
		azureVMSizeLabel:  "Standard_ND96isr_H200_v5",
		labelAKSAgentPool: "research",
	}, "azure:///managed-gpu")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, node).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	reconciler := &TauClusterReconciler{Client: c}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("initial Reconcile() error = %v", err)
	}
	var changed corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node.Name}, &changed); err != nil {
		t.Fatalf("Get Node: %v", err)
	}
	changed.Labels[azureVMSizeLabel] = "Standard_NV36ads_A10_v5"
	if err := c.Update(ctx, &changed); err != nil {
		t.Fatalf("change GPU SKU: %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("downgrade Reconcile() error = %v", err)
	}
	var got corev1.Node
	if err := c.Get(ctx, client.ObjectKey{Name: node.Name}, &got); err != nil {
		t.Fatalf("Get downgraded Node: %v", err)
	}
	if got.Labels[labelkeys.LabelSite] != "azure-eastus2" ||
		got.Labels[labelkeys.LabelNetworkDomain] != isolatedTopologyLabel("isolated-domain", node.Name) ||
		got.Labels[labelkeys.LabelInfiniband] != "false" {
		t.Fatalf("downgraded topology labels = %#v", got.Labels)
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
