// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
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

func TestTauClusterReconcilesMicrosoftRegionTopology(t *testing.T) {
	ctx := context.Background()
	cluster := topologyTestCluster(tauv1alpha1.TauSiteSpec{
		Name:     "microsoft-eastus2",
		Provider: tauv1alpha1.SiteProviderMicrosoft,
		Region:   "eastus2",
	})
	node := topologyTestNode("msft-h200", "eastus2", "")
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
	if gotNode.Labels[labelkeys.LabelNetworkDomain] != "microsoft-eastus2" ||
		gotNode.Labels[labelkeys.LabelInfiniband] != "true" {
		t.Fatalf("topology labels = %#v", gotNode.Labels)
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
		map[string]any{"nodeLabel": labelRegion},
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

func TestTauClusterReconcilesFlexInfiniBandSite(t *testing.T) {
	enabled := true
	cluster := topologyTestCluster(tauv1alpha1.TauSiteSpec{
		Name:       "research-site",
		Provider:   tauv1alpha1.SiteProviderFlex,
		Region:     "eastus2",
		Infiniband: &enabled,
	})
	node := topologyTestNode("flex-h200", "eastus2", "research-site")
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
	if got.Labels[labelkeys.LabelNetworkDomain] != "flex-research-site" ||
		got.Labels[labelkeys.LabelInfiniband] != "true" {
		t.Fatalf("topology labels = %#v", got.Labels)
	}
}

func TestTauClusterIsolatesFlexNodesWithoutInfiniBand(t *testing.T) {
	disabled := false
	cluster := topologyTestCluster(tauv1alpha1.TauSiteSpec{
		Name:       "ethernet-site",
		Provider:   tauv1alpha1.SiteProviderFlex,
		Region:     "westus3",
		Infiniband: &disabled,
	})
	first := topologyTestNode("flex-a", "westus3", "ethernet-site")
	second := topologyTestNode("flex-b", "westus3", "ethernet-site")
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
		if got.Labels[labelkeys.LabelInfiniband] != "false" {
			t.Fatalf("Node %q InfiniBand = %q", name, got.Labels[labelkeys.LabelInfiniband])
		}
		domains[name] = got.Labels[labelkeys.LabelNetworkDomain]
	}
	if domains[first.Name] == domains[second.Name] ||
		domains[first.Name] == "" || domains[second.Name] == "" {
		t.Fatalf("non-IB network domains = %#v, want distinct singleton domains", domains)
	}
}

func TestTauClusterRemovesStaleTopologyLabelsWhenNodeLeavesSite(t *testing.T) {
	enabled := true
	cluster := topologyTestCluster(tauv1alpha1.TauSiteSpec{
		Name:       "research-site",
		Provider:   tauv1alpha1.SiteProviderFlex,
		Region:     "eastus2",
		Infiniband: &enabled,
	})
	node := topologyTestNode("flex-h200", "eastus2", "other-site")
	node.Labels[labelkeys.LabelNetworkDomain] = "flex-research-site"
	node.Labels[labelkeys.LabelInfiniband] = "true"
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
	if got.Labels[labelkeys.LabelNetworkDomain] != "" || got.Labels[labelkeys.LabelInfiniband] != "" {
		t.Fatalf("stale topology labels = %#v", got.Labels)
	}
}

func TestTauClusterRemovesTopologyLabelsWhenSitesAreCleared(t *testing.T) {
	cluster := topologyTestCluster()
	node := topologyTestNode("flex-h200", "eastus2", "old-site")
	node.Labels[labelkeys.LabelNetworkDomain] = "flex-old-site"
	node.Labels[labelkeys.LabelInfiniband] = "true"
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
	if got.Labels[labelkeys.LabelNetworkDomain] != "" || got.Labels[labelkeys.LabelInfiniband] != "" {
		t.Fatalf("stale topology labels = %#v", got.Labels)
	}
}

func TestTauClusterReportsForeignTopologyOwnership(t *testing.T) {
	cluster := topologyTestCluster(tauv1alpha1.TauSiteSpec{
		Name:     "microsoft-eastus2",
		Provider: tauv1alpha1.SiteProviderMicrosoft,
		Region:   "eastus2",
	})
	node := topologyTestNode("msft-h200", "eastus2", "")
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
	if gotNode.Labels[labelkeys.LabelNetworkDomain] != "" || gotNode.Labels[labelkeys.LabelInfiniband] != "" {
		t.Fatalf("foreign Topology conflict mutated Node labels: %#v", gotNode.Labels)
	}
	for _, mutation := range recording.mutations {
		if mutation == "update "+tauGPUNodeTopologyName || mutation == "patch "+tauGPUNodeTopologyName {
			t.Fatalf("foreign Topology was mutated: %v", recording.mutations)
		}
	}
}

func TestValidateSitesRequiresExplicitFlexInfiniBand(t *testing.T) {
	err := validateSites([]tauv1alpha1.TauSiteSpec{{
		Name:     "flex",
		Provider: tauv1alpha1.SiteProviderFlex,
		Region:   "eastus2",
	}})
	if err == nil {
		t.Fatal("validateSites() accepted a Flex site without an explicit InfiniBand declaration")
	}
}

func TestSiteNodeLabelsUseValidDomainForLongSiteName(t *testing.T) {
	enabled := true
	site := tauv1alpha1.TauSiteSpec{
		Name:       "a-site-name-that-is-long-enough-to-exceed-the-kubernetes-label-limit",
		Provider:   tauv1alpha1.SiteProviderFlex,
		Region:     "eastus2",
		Infiniband: &enabled,
	}
	labels := siteNodeLabels(topologyTestNode("flex-h200", "eastus2", site.Name), site)
	domain := labels[labelkeys.LabelNetworkDomain]
	if problems := validation.IsValidLabelValue(domain); len(problems) > 0 {
		t.Fatalf("network domain %q is invalid: %v", domain, problems)
	}
	if domain != networkDomainLabel("flex", site.Name) {
		t.Fatalf("network domain = %q, want deterministic helper result", domain)
	}
}

func topologyTestCluster(sites ...tauv1alpha1.TauSiteSpec) *tauv1alpha1.TauCluster {
	return &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{
			ManagementMode: tauv1alpha1.ClusterManagementModeReconcile,
			Sites:          sites,
		},
	}
}

func topologyTestNode(name, region, flexSite string) *corev1.Node {
	labels := map[string]string{
		labelRegion:                 region,
		labelHostname:               name,
		labelkeys.LabelGPUClass:     "h200-141gb",
		"kubernetes.io/os":          "linux",
		"kubernetes.azure.com/mode": "user",
	}
	if flexSite != "" {
		labels[labelFlexSite] = flexSite
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}}
}
