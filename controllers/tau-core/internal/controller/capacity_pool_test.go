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
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestCapacityPoolSharesAndRefreshes(t *testing.T) {
	ctx := context.Background()
	cluster, team, workspaces, nodes := capacityPoolFixture()
	empty := desiredWorkspaceClusterQueue(workspaces[0])
	workspaces[0].Status.Queue = tauv1alpha1.WorkspaceQueueStatus{LocalQueue: "jobqueue", ClusterQueue: empty.GetName()}
	objects := []client.Object{cluster, team, empty, desiredTeamCohort(team, nil)}
	for _, workspace := range workspaces {
		objects = append(objects, workspace, testNamespace(workspace.Name))
	}
	for _, node := range nodes {
		objects = append(objects, node)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).
		WithStatusSubresource(&tauv1alpha1.TauTeam{}, &corev1.Node{}).Build()
	managed, capacity, drifted, err := (&TauClusterReconciler{Client: c}).reconcileDiscoveredGPUFlavors(ctx, cluster, true)
	if err != nil || drifted || len(managed) != 2 || len(capacity) != 8 {
		t.Fatalf("discovery without legacy queues: managed=%v capacity=%v drifted=%v err=%v", managed, capacity, drifted, err)
	}
	reconciler := &TauTeamReconciler{Client: c}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(team)}
	for range 2 {
		if _, err := reconciler.Reconcile(ctx, request); err != nil {
			t.Fatal(err)
		}
	}
	cohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, cohort); err != nil {
		t.Fatal(err)
	}
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "8")
	assertPoolQuantity(t, cohort, "taugrid-gpu-a100", nvidiaGPUResourceName, "nominalQuota", "4")
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", "rdma/ib", "nominalQuota", "63")
	assertPoolQuantity(t, cohort, "taugrid-gpu-a100", "rdma/ib", "nominalQuota", "0")
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", "cpu", "nominalQuota", "31500m")
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", "memory", "nominalQuota", "128Gi")
	groups, _, _ := unstructured.NestedSlice(cohort.Object, "spec", "resourceGroups")
	if len(groups) != 1 {
		t.Fatal("pool flavors must share one normalized covered-resource set")
	}
	for _, workspace := range workspaces {
		queueStatus, ready, message, err := newTestWorkspaceReconciler(c).reconcileQueue(ctx, workspace, workspace.Name)
		if err != nil || !ready {
			t.Fatalf("workspace %s: ready=%v message=%s err=%v", workspace.Name, ready, message, err)
		}
		queue := newQueueObject(clusterQueueGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: queueStatus.ClusterQueue}, queue); err != nil {
			t.Fatal(err)
		}
		assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "0")
		assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", "8")
		assertPoolQuantity(t, queue, "taugrid-gpu-a100", nvidiaGPUResourceName, "borrowingLimit", "4")
		cohortName, _, _ := unstructured.NestedString(queue.Object, "spec", "cohortName")
		if cohortName != cohort.GetName() {
			t.Fatalf("workspace %s cohort = %q", workspace.Name, cohortName)
		}
		for _, name := range []string{workspaceLocalQueueName(workspace), workspace.Spec.Queue} {
			local := newQueueObject(localQueueGVK)
			if err := c.Get(ctx, client.ObjectKey{Namespace: workspace.Name, Name: name}, local); err != nil {
				t.Fatal(err)
			}
			target, _, _ := unstructured.NestedString(local.Object, "spec", "clusterQueue")
			if target != queue.GetName() {
				t.Fatalf("LocalQueue %q targets %q instead of pool queue", name, target)
			}
		}
	}
	active := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspaces[0].Name)}, active); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedSlice(active.Object, []any{map[string]any{
		"name": "taugrid-gpu-h200", "resources": []any{map[string]any{"name": nvidiaGPUResourceName, "total": "8"}},
	}}, "status", "flavorsReservation"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(ctx, active); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, nodes[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cohort), cohort); err != nil {
		t.Fatal(err)
	}
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "0")
	for i, workspace := range workspaces {
		queue := newQueueObject(clusterQueueGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, queue); err != nil {
			t.Fatal(err)
		}
		limit := "0"
		if i == 0 {
			limit = "8"
		}
		assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", limit)
	}
	nodes[0].ResourceVersion = ""
	if err := c.Create(ctx, nodes[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cohort), cohort); err != nil {
		t.Fatal(err)
	}
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "8")
	node := &corev1.Node{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(nodes[0]), node); err != nil {
		t.Fatal(err)
	}
	node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("16")
	if err := c.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(cohort), cohort); err != nil {
		t.Fatal(err)
	}
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "16")
	for _, workspace := range workspaces {
		queue := newQueueObject(clusterQueueGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, queue); err != nil {
			t.Fatal(err)
		}
		assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "0")
		assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", "16")
	}
}

func TestCapacityPoolCountsEachNodeOnce(t *testing.T) {
	cluster, team, _, nodes := capacityPoolFixture()
	second := nodes[0].DeepCopy()
	second.Name = "second"
	cordoned := nodes[0].DeepCopy()
	cordoned.Name = "cordoned"
	cordoned.Spec.Unschedulable = true
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(nodes[0], second, cordoned).Build()
	flavors, quotas, err := discoveredPoolInventory(context.Background(), c, cluster)
	if err != nil || len(flavors) != 1 {
		t.Fatalf("inventory: flavors=%v err=%v", flavors, err)
	}
	cohort := desiredCapacityCohort(team, quotas)
	for resourceName, want := range map[string]string{
		nvidiaGPUResourceName: "16", "cpu": "63", "memory": "256Gi", "rdma/ib": "126",
	} {
		assertPoolQuantity(t, cohort, "taugrid-gpu-h200", resourceName, "nominalQuota", want)
	}
}

func TestCapacityPoolLegacyGuardUsesLiveReader(t *testing.T) {
	cluster, team, _, _ := capacityPoolFixture()
	cluster.Spec.Queues.CapacityTeamRef = nil
	legacy := newQueueObject(clusterQueueGVK)
	legacy.SetName("legacy")
	legacy.SetLabels(map[string]string{labelDiscoverGPUFlavors: "true"})
	legacy.Object["spec"] = map[string]any{"resourceGroups": []any{}}
	scheme := testScheme(t)
	cache := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	live := fake.NewClientBuilder().WithScheme(scheme).WithObjects(desiredCapacityCohort(team,
		[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")})).Build()
	reconciler := &TauClusterReconciler{Client: cache, APIReader: live}
	_, _, _, err := reconciler.reconcileDiscoveredGPUFlavors(context.Background(), cluster, true)
	if err == nil || !strings.Contains(err.Error(), "retains a discovered capacity pool") {
		t.Fatalf("stale cache bypassed retained capacity ownership: %v", err)
	}
}

func TestCapacityPoolDoesNotFallBackToExternalRouting(t *testing.T) {
	ctx := context.Background()
	cluster, team, workspaces, nodes := capacityPoolFixture()
	workspace := workspaces[0]
	external := newQueueObject(clusterQueueGVK)
	external.SetName("external")
	external.Object["spec"] = map[string]any{"resourceGroups": []any{}}
	local := testLocalQueue(workspace.Name, workspace.Spec.Queue, external.GetName())
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cluster, team, workspace, nodes[0], external, local,
		desiredDiscoveredGPUFlavor(discoveredGPUFlavor{name: "taugrid-gpu-h200", gpuClass: "h200"})).Build()
	_, ready, message, err := newTestWorkspaceReconciler(c).reconcileQueue(ctx, workspace, workspace.Name)
	if err != nil || ready || !strings.Contains(message, "must target capacity pool ClusterQueue") {
		t.Fatalf("external fallback: ready=%v message=%s err=%v", ready, message, err)
	}
	current := newQueueObject(localQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(local), current); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current.Object["spec"], local.Object["spec"]) {
		t.Fatal("external LocalQueue routing changed")
	}
}

func TestCapacityPoolEligibility(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*corev1.Node)
		want   string
	}{
		{"ready", func(*corev1.Node) {}, "8"},
		{"not ready", func(n *corev1.Node) { n.Status.Conditions[0].Status = corev1.ConditionFalse }, "0"},
		{"unknown ready", func(n *corev1.Node) { n.Status.Conditions = nil }, "0"},
		{"cordoned", func(n *corev1.Node) { n.Spec.Unschedulable = true }, "0"},
		{"deleting", func(n *corev1.Node) { now := metav1.Now(); n.DeletionTimestamp = &now; n.Finalizers = []string{"test"} }, "0"},
		{"wrong selector", func(n *corev1.Node) { n.Labels["pool"] = "other" }, ""},
		{"non linux", func(n *corev1.Node) { n.Labels[corev1.LabelOSStable] = "windows" }, ""},
		{"no class", func(n *corev1.Node) { delete(n.Labels, labelkeys.LabelGPUClass) }, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			cluster, _, _, nodes := capacityPoolFixture()
			cluster.Spec.Nodes.Selector = map[string]string{"pool": "shared"}
			node := nodes[0]
			node.Labels["pool"] = "shared"
			test.change(node)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(node).Build()
			_, quotas, err := discoveredPoolInventory(context.Background(), c, cluster)
			if err != nil {
				t.Fatal(err)
			}
			if test.want == "" {
				if len(quotas) != 0 {
					t.Fatalf("ineligible node counted: %v", quotas)
				}
				return
			}
			assertPoolQuantity(t, desiredTeamCohort(testTeam("pool", "0"), quotas), "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", test.want)
		})
	}
}

func TestCapacityPoolUnrelatedObjectsWithoutResourceGroups(t *testing.T) {
	for _, kind := range []string{"ClusterQueue", "Cohort"} {
		for _, state := range []string{"absent", "malformed", "reserved"} {
			if kind == "Cohort" && state == "reserved" {
				continue
			}
			t.Run(kind+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				cluster, team, workspaces, nodes := capacityPoolFixture()
				external := newQueueObject(clusterQueueGVK)
				if kind == "Cohort" {
					external.SetGroupVersionKind(cohortGVK)
				}
				external.SetName("unrelated")
				external.Object["spec"] = map[string]any{}
				if state == "malformed" {
					external.Object["spec"].(map[string]any)["resourceGroups"] = "invalid"
				}
				if state == "reserved" {
					external.Object["status"] = map[string]any{"flavorsReservation": []any{map[string]any{
						"name": "taugrid-gpu-h200", "resources": []any{map[string]any{"name": nvidiaGPUResourceName, "total": "1"}},
					}}}
				}
				if _, err := clusterQueueNominalQuota(external); err == nil {
					t.Fatal("strict explicit-quota parsing unexpectedly accepts absent or malformed groups")
				}
				cohort := desiredCapacityCohort(team, []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")})
				c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cluster, team, workspaces[0], nodes[0], external, cohort,
					desiredDiscoveredGPUFlavor(discoveredGPUFlavor{name: "taugrid-gpu-h200", gpuClass: "h200"})).Build()
				err := (&TauTeamReconciler{Client: c}).reconcileCapacityPool(ctx, cluster, team)
				want := "8"
				if state != "absent" {
					want = "0"
					if err == nil {
						t.Fatal("unsafe or malformed object did not block the pool")
					}
					if state == "reserved" && !strings.Contains(err.Error(), "retains reservations") {
						t.Fatalf("reservation conflict was not checked: %v", err)
					}
				} else if err != nil {
					t.Fatalf("valid quota-free %s blocked the pool: %v", kind, err)
				}
				current := newQueueObject(cohortGVK)
				if err := c.Get(ctx, client.ObjectKeyFromObject(cohort), current); err != nil {
					t.Fatal(err)
				}
				assertPoolQuantity(t, current, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", want)
				unchanged := newQueueObject(external.GroupVersionKind())
				if err := c.Get(ctx, client.ObjectKeyFromObject(external), unchanged); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(unchanged.Object["spec"], external.Object["spec"]) ||
					!reflect.DeepEqual(unchanged.Object["status"], external.Object["status"]) {
					t.Fatal("unrelated object was modified")
				}
			})
		}
	}
}

func TestCapacityPoolRejectsConflictsWithoutChangingExternalResources(t *testing.T) {
	for _, kind := range []string{"team", "legacy", "external queue", "external borrowing", "external reservations", "external cohort", "overlapping selector", "workspace guarantee", "flavor drift", "owner UID"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			cluster, team, workspaces, nodes := capacityPoolFixture()
			objects := []client.Object{cluster, team, workspaces[0], nodes[0],
				desiredDiscoveredGPUFlavor(discoveredGPUFlavor{name: "taugrid-gpu-h200", gpuClass: "h200"})}
			cohort := desiredCapacityCohort(team, []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")})
			objects = append(objects, cohort)
			external := desiredTeamCohort(testTeam("external", "1"), []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "1", "0", "0")})
			switch kind {
			case "team":
				objects = append(objects, testTeam("other", "1"))
			case "legacy":
				external = newQueueObject(clusterQueueGVK)
				external.SetName("legacy")
				external.SetLabels(map[string]string{labelDiscoverGPUFlavors: "true"})
				external.Object["spec"] = map[string]any{}
				objects = append(objects, external)
			case "external queue":
				external.SetGroupVersionKind(clusterQueueGVK)
				objects = append(objects, external)
			case "external borrowing":
				external.SetGroupVersionKind(clusterQueueGVK)
				external.Object["spec"] = map[string]any{
					"cohortName": "external",
					"resourceGroups": quotaResourceGroups([]tauv1alpha1.TauResourceQuota{
						testGPUQuota("taugrid-gpu-h200", "0", "1", "0"),
					}, true),
				}
				objects = append(objects, external)
			case "external reservations":
				external.SetGroupVersionKind(clusterQueueGVK)
				external.Object["spec"] = map[string]any{"resourceGroups": quotaResourceGroups(
					[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "0", "0", "0")}, true)}
				external.Object["status"] = map[string]any{"flavorsReservation": []any{map[string]any{
					"name": "taugrid-gpu-h200", "resources": []any{map[string]any{"name": nvidiaGPUResourceName, "total": "1"}},
				}}}
				objects = append(objects, external)
			case "external cohort":
				objects = append(objects, external)
			case "overlapping selector":
				flavor := newQueueObject(resourceFlavorGVK)
				flavor.SetName("all-linux")
				flavor.Object["spec"] = map[string]any{"nodeLabels": map[string]any{"kubernetes.io/os": "linux"}}
				external.Object["spec"] = map[string]any{"resourceGroups": quotaResourceGroups([]tauv1alpha1.TauResourceQuota{testGPUQuota("all-linux", "1", "0", "0")}, false)}
				objects = append(objects, flavor, external)
			case "workspace guarantee":
				workspaces[0].Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "1", "0", "0")}
			case "flavor drift":
				objects[4].SetLabels(nil)
			case "owner UID":
				queue := desiredWorkspaceClusterQueue(workspaces[0])
				queue.SetAnnotations(map[string]string{annotationOwnerUID: "previous-owner"})
				delete(queue.Object["spec"].(map[string]any), "resourceGroups")
				objects = append(objects, queue)
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
			if err := (&TauTeamReconciler{Client: c}).reconcileCapacityPool(ctx, cluster, team); err == nil {
				t.Fatal("expected capacity conflict")
			}
			got := newQueueObject(cohortGVK)
			if err := c.Get(ctx, client.ObjectKeyFromObject(cohort), got); err != nil {
				t.Fatal(err)
			}
			assertPoolQuantity(t, got, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "0")
			if kind == "external queue" || kind == "external cohort" || kind == "overlapping selector" || kind == "external reservations" {
				got := newQueueObject(external.GroupVersionKind())
				if err := c.Get(ctx, client.ObjectKeyFromObject(external), got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Object["spec"], external.Object["spec"]) {
					t.Fatal("external budget changed")
				}
				flavor := "taugrid-gpu-h200"
				if kind == "overlapping selector" {
					flavor = "all-linux"
				}
				want := "1"
				if kind == "external reservations" {
					want = "0"
				}
				assertPoolQuantity(t, got, flavor, nvidiaGPUResourceName, "nominalQuota", want)
			}
		})
	}
}

func TestCapacityPoolPreservesExplicitModeAndAdmissionChecks(t *testing.T) {
	ctx := context.Background()
	cluster, team, workspaces, nodes := capacityPoolFixture()
	queue := desiredWorkspaceClusterQueue(workspaces[0])
	queue.Object["spec"].(map[string]any)["admissionChecks"] = []any{"health"}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(cluster, team, workspaces[0], nodes[0], queue,
		desiredDiscoveredGPUFlavor(discoveredGPUFlavor{name: "taugrid-gpu-h200", gpuClass: "h200"})).Build()
	r := &TauTeamReconciler{Client: c}
	if err := r.reconcileCapacityPool(ctx, cluster, team); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(queue), queue); err != nil {
		t.Fatal(err)
	}
	checks, _, _ := unstructured.NestedStringSlice(queue.Object, "spec", "admissionChecks")
	if len(checks) != 1 || checks[0] != "health" {
		t.Fatalf("admission checks changed: %v", checks)
	}
	cluster.Spec.ManagementMode = tauv1alpha1.ClusterManagementModeObserve
	if err := r.reconcileCapacityPool(ctx, cluster, team); err == nil {
		t.Fatal("automatic mutation allowed in Observe mode")
	}
	cohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, cohort); err != nil {
		t.Fatal(err)
	}
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "8")
	cluster.Spec.ManagementMode = tauv1alpha1.ClusterManagementModeReconcile
	cluster.Spec.Queues.CapacityTeamRef = nil
	if err := c.Update(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if _, selected, err := selectedCapacityTeam(ctx, c, team.Name); err != nil || selected {
		t.Fatalf("implicit capacity selection: selected=%v err=%v", selected, err)
	}
	explicit := testWorkspace("explicit")
	explicit.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "other"}
	explicit.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "2", "0", "0")}
	assertPoolQuantity(t, desiredWorkspaceClusterQueue(explicit), "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "2")
	assertPoolQuantity(t, desiredWorkspaceClusterQueue(explicit), "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", "0")
	other := testTeam("other", "1")
	if err := c.Create(ctx, other); err != nil {
		t.Fatal(err)
	}
	if err := r.validateTeamCapacity(ctx, other); err == nil || !strings.Contains(err.Error(), "retains a discovered capacity pool") {
		t.Fatalf("retained pool failed to block reallocation: %v", err)
	}
	legacy := newQueueObject(clusterQueueGVK)
	legacy.SetName("legacy")
	legacy.SetLabels(map[string]string{labelDiscoverGPUFlavors: "true"})
	if err := c.Create(ctx, legacy); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := (&TauClusterReconciler{Client: c}).reconcileDiscoveredGPUFlavors(ctx, cluster, true); err == nil ||
		!strings.Contains(err.Error(), "retains a discovered capacity pool") {
		t.Fatalf("retained pool failed to block legacy discovery: %v", err)
	}
	workspaces[0].Finalizers = nil
	if err := c.Update(ctx, workspaces[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, workspaces[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileTeamCohort(ctx, team, nil); err != nil {
		t.Fatal(err)
	}
	if err := validateRetainedCapacityPools(ctx, c, ""); err != nil {
		t.Fatalf("drained pool still blocks reallocation: %v", err)
	}
}

func capacityPoolFixture() (*tauv1alpha1.TauCluster, *tauv1alpha1.TauTeam, []*tauv1alpha1.TauWorkspace, []*corev1.Node) {
	team := testTeam("pool", "0")
	team.Spec.Quota = nil
	team.Finalizers = []string{teamFinalizer}
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Spec: tauv1alpha1.TauClusterSpec{ManagementMode: tauv1alpha1.ClusterManagementModeReconcile,
			Queues: tauv1alpha1.TauClusterQueuesSpec{CapacityTeamRef: &tauv1alpha1.TauClusterObjectReference{Name: team.Name}}},
	}
	workspaces := []*tauv1alpha1.TauWorkspace{testWorkspace("research"), testWorkspace("development")}
	for _, workspace := range workspaces {
		workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
		workspace.Finalizers = []string{workspaceFinalizer}
	}
	nodes := []*corev1.Node{}
	for _, class := range []string{"h200", "a100"} {
		node := &corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: class, Labels: map[string]string{corev1.LabelOSStable: "linux", labelkeys.LabelGPUClass: class}},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
				Allocatable: corev1.ResourceList{
					corev1.ResourceName(nvidiaGPUResourceName): resource.MustParse("4"),
					corev1.ResourceCPU:                         resource.MustParse("31500m"), corev1.ResourceMemory: resource.MustParse("128Gi"),
				},
			},
		}
		if class == "h200" {
			node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("8")
			node.Status.Allocatable["rdma/ib"] = resource.MustParse("63")
		}
		nodes = append(nodes, node)
	}
	return cluster, team, workspaces, nodes
}

func assertPoolQuantity(t *testing.T, object *unstructured.Unstructured, flavor, resourceName, field, want string) {
	t.Helper()
	got := quotaFromResourceGroups(t, object, flavor, resourceName, field)
	actual, err := resource.ParseQuantity(got)
	if err != nil || actual.Cmp(resource.MustParse(want)) != 0 {
		t.Fatalf("%s %s/%s %s = %q, want %s (error %v)", object.GetName(), flavor, resourceName, field, got, want, err)
	}
}
