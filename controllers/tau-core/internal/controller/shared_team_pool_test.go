// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuecache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	kueueindex "sigs.k8s.io/kueue/pkg/controller/core/indexer"
	"sigs.k8s.io/kueue/pkg/resources"
	kueuewebhooks "sigs.k8s.io/kueue/pkg/webhooks"
	"sigs.k8s.io/kueue/pkg/workload"
)

func sharedTeamFixture(t *testing.T, intercept interceptor.Funcs) (*TauTeamReconciler, *tauv1alpha1.TauCluster, []*tauv1alpha1.TauTeam, []*tauv1alpha1.TauWorkspace) {
	t.Helper()
	cluster, _, workspaces, nodes := capacityPoolFixture()
	cluster.UID = "shared-cluster"
	cluster.Spec.Queues.CapacityTeamRef = nil
	cluster.Spec.Queues.ShareTeamQuotas = true
	teams := []*tauv1alpha1.TauTeam{}
	root := newQueueObject(cohortGVK)
	root.SetName(sharedTeamCohortName)
	root.SetLabels(map[string]string{labelSharedTeamQuotas: cluster.Name})
	root.Object["spec"] = map[string]any{"resourceGroups": quotaResourceGroups([]tauv1alpha1.TauResourceQuota{
		{Flavor: "taugrid-gpu-h200", Resource: nvidiaGPUResourceName, NominalQuota: resource.MustParse("2")},
		{Flavor: "taugrid-gpu-a100", Resource: nvidiaGPUResourceName, NominalQuota: resource.MustParse("4")},
		{Flavor: "taugrid-gpu-h200", Resource: "cpu", NominalQuota: resource.MustParse("31500m")},
		{Flavor: "taugrid-gpu-a100", Resource: "cpu", NominalQuota: resource.MustParse("31500m")},
	}, false)}
	objects := []client.Object{cluster, root}
	for i, workspace := range workspaces {
		team := testTeam(fmt.Sprintf("team-%d", i), "3")
		team.UID = types.UID(team.Name)
		team.Finalizers = []string{teamFinalizer}
		team.Spec.Quota[0].BorrowingLimit = nil
		team.Spec.Quota[0].LendingLimit = nil
		teams = append(teams, team)
		workspace.UID = types.UID(workspace.Name)
		workspace.Spec.Quota = nil
		workspace.Spec.TeamRef.Name = team.Name
		objects = append(objects, team, workspace)
	}
	for _, node := range nodes {
		objects = append(objects, node)
		objects = append(objects, desiredDiscoveredGPUFlavor(discoveredGPUFlavor{
			name: discoveredGPUFlavorName(node.Labels[labelkeys.LabelGPUClass]), gpuClass: node.Labels[labelkeys.LabelGPUClass],
		}))
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).
		WithStatusSubresource(&corev1.Node{}, &tauv1alpha1.TauTeam{}).WithInterceptorFuncs(intercept).Build()
	return &TauTeamReconciler{Client: c}, cluster, teams, workspaces
}

func TestSharedTeamStaticSpareAndBorrowingContract(t *testing.T) {
	ctx := context.Background()
	r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
	extraWorkspace := workspaces[0].DeepCopy()
	extraWorkspace.Name += "-second"
	extraWorkspace.UID = types.UID(extraWorkspace.Name)
	extraWorkspace.ResourceVersion = ""
	extraWorkspace.Spec.Target.Namespace = extraWorkspace.Name
	if err := r.Create(ctx, extraWorkspace); err != nil {
		t.Fatal(err)
	}
	workspaces = append(workspaces, extraWorkspace)
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	assertPoolQuantity(t, root, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "2")
	assertPoolQuantity(t, root, "taugrid-gpu-a100", nvidiaGPUResourceName, "nominalQuota", "4")
	assertPoolQuantity(t, root, "taugrid-gpu-h200", "cpu", "nominalQuota", "31500m")
	t.Logf("GPU supply=8; fixed team guarantees=3+3; explicit static spare=%s; workspace nominal=0",
		quotaFromResourceGroups(t, root, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"))
	scheme := runtime.NewScheme()
	if err := kueue.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cache := kueuecache.New(fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&kueue.LocalQueue{}, kueueindex.QueueClusterQueueKey, kueueindex.IndexQueueClusterQueue).
		WithIndex(&kueue.Workload{}, kueueindex.WorkloadClusterQueueKey, kueueindex.IndexWorkloadClusterQueue).Build())
	for _, name := range []string{"taugrid-gpu-h200", "taugrid-gpu-a100"} {
		cache.AddOrUpdateResourceFlavor(logr.Discard(), &kueue.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	addCohort := func(object *unstructured.Unstructured) {
		t.Helper()
		var cohort kueue.Cohort
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(object.Object, &cohort); err != nil {
			t.Fatal(err)
		}
		if _, err := (&kueuewebhooks.CohortWebhook{}).ValidateCreate(ctx, &cohort); err != nil {
			t.Fatalf("generated Cohort %q is invalid: %v", cohort.Name, err)
		}
		if err := cache.AddOrUpdateCohort(&cohort); err != nil {
			t.Fatal(err)
		}
	}
	addCohort(root)
	for _, team := range teams {
		cohort := getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(team.Name))
		assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "3")
		quotas, err := clusterQueueQuotaObjects(cohort)
		if err != nil {
			t.Fatal(err)
		}
		if _, found := quotas["taugrid-gpu-h200"][nvidiaGPUResourceName]["borrowingLimit"]; found {
			t.Fatal("omitted team borrowing limit must remain unlimited")
		}
		addCohort(cohort)
	}
	for _, workspace := range workspaces {
		queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspace.Name))
		assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "0")
		assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", "8")
		var cq kueue.ClusterQueue
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(queue.Object, &cq); err != nil {
			t.Fatal(err)
		}
		if _, err := (&kueuewebhooks.ClusterQueueWebhook{}).ValidateCreate(ctx, &cq); err != nil {
			t.Fatalf("generated ClusterQueue %q is invalid: %v", cq.Name, err)
		}
		cohort, _, err := unstructured.NestedString(queue.Object, "spec", "cohortName")
		if err != nil || cohort != teamCohortName(workspace.Spec.TeamRef.Name) {
			t.Fatalf("workspace %q cohort = %q, want its team cohort: %v", workspace.Name, cohort, err)
		}
		if cq.Spec.Preemption.ReclaimWithinCohort != kueue.PreemptionPolicyNever ||
			cq.Spec.Preemption.BorrowWithinCohort.Policy != kueue.BorrowWithinCohortPolicyNever {
			t.Fatal("borrowed jobs must not be preempted by another queue")
		}
		if err := cache.AddClusterQueue(ctx, &cq); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := cache.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := snapshot.ClusterQueue(kueue.ClusterQueueReference(workspaceClusterQueueName(workspaces[0].Name)))
	second := snapshot.ClusterQueue(kueue.ClusterQueueReference(workspaceClusterQueueName(workspaces[1].Name)))
	third := snapshot.ClusterQueue(kueue.ClusterQueueReference(workspaceClusterQueueName(extraWorkspace.Name)))
	if first == nil || second == nil || third == nil {
		t.Fatal("shared workspace queues must be in the pinned Kueue scheduling snapshot")
	}
	fr := resources.FlavorResource{Flavor: "taugrid-gpu-h200", Resource: corev1.ResourceName(nvidiaGPUResourceName)}
	usage := workload.Usage{Quota: resources.FlavorResourceQuantities{fr: resources.NewAmount(8)}}
	if first.Available(fr).CmpInt64(8) != 0 || second.Available(fr).CmpInt64(8) != 0 || third.Available(fr).CmpInt64(8) != 0 {
		t.Fatal("each zero-nominal workspace must be able to spill over to the whole pool, not just its team guarantee")
	}
	t.Logf("Before borrowing: either workspace can use %s GPUs", first.Available(fr).String())
	first.AddUsage(usage)
	if second.Available(fr).CmpInt64(0) != 0 || third.Available(fr).CmpInt64(0) != 0 {
		t.Fatal("both same-team and other-team workspaces must wait while the first workspace borrows all supply")
	}
	t.Logf("One team borrows all 8 GPUs: sibling available=%s; no cross-team preemption", second.Available(fr).String())
	first.RemoveUsage(usage)
	if second.Available(fr).CmpInt64(8) != 0 {
		t.Fatal("completed borrowed usage must return capacity to the sibling")
	}
	t.Logf("Borrowed job finishes: sibling available=%s GPUs", second.Available(fr).String())
	second.AddUsage(usage)
	if first.Available(fr).CmpInt64(0) != 0 || third.Available(fr).CmpInt64(0) != 0 {
		t.Fatal("borrowing the whole fixed tree must also work in the other team direction")
	}
	second.RemoveUsage(usage)
	firstUsage := workload.Usage{Quota: resources.FlavorResourceQuantities{fr: resources.NewAmount(3)}}
	first.AddUsage(firstUsage)
	if third.Available(fr).CmpInt64(5) != 0 {
		t.Fatal("a second workspace must share its team entitlement and remaining pool without a fixed split")
	}
	thirdUsage := workload.Usage{Quota: resources.FlavorResourceQuantities{fr: resources.NewAmount(5)}}
	third.AddUsage(thirdUsage)
	if second.Available(fr).CmpInt64(0) != 0 {
		t.Fatal("usage across a team's workspaces must consume one shared physical budget")
	}
	third.RemoveUsage(thirdUsage)
	if second.Available(fr).CmpInt64(5) != 0 {
		t.Fatal("completed usage must return capacity to the other team")
	}
	first.RemoveUsage(firstUsage)
	t.Log("Two workspaces share one team guarantee; combined usage never duplicates physical capacity.")
	cohorts := []*unstructured.Unstructured{root}
	for _, team := range teams {
		cohorts = append(cohorts, getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(team.Name)))
	}
	unit := workload.Usage{Quota: resources.FlavorResourceQuantities{fr: resources.NewAmount(1)}}
	for _, order := range [][3]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		for _, cohort := range cohorts {
			cache.DeleteCohort(kueue.CohortReference(cohort.GetName()))
		}
		var reserved []*kueue.Workload
		for step, index := range order {
			addCohort(cohorts[index])
			snapshot, err := cache.Snapshot(ctx)
			if err != nil {
				t.Fatal(err)
			}
			used := len(reserved)
			for _, workspace := range workspaces {
				queue := snapshot.ClusterQueue(kueue.ClusterQueueReference(workspaceClusterQueueName(workspace.Name)))
				if queue == nil {
					continue
				}
				for queue.Available(fr).CmpInt64(1) >= 0 {
					count := int32(1)
					item := &kueue.Workload{
						ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("reserved-%d", used), Namespace: "test"},
						Spec:       kueue.WorkloadSpec{PodSets: []kueue.PodSet{{Name: "main", Count: count}}},
					}
					workload.SetQuotaReservation(item, &kueue.Admission{
						ClusterQueue: queue.Name,
						PodSetAssignments: []kueue.PodSetAssignment{{
							Name: "main", Count: &count,
							Flavors:       map[corev1.ResourceName]kueue.ResourceFlavorReference{fr.Resource: fr.Flavor},
							ResourceUsage: corev1.ResourceList{fr.Resource: resource.MustParse("1")},
						}},
					}, clock.RealClock{})
					if quantity := workload.NewInfo(item).FlavorResourceUsage()[fr]; quantity.CmpInt64(1) != 0 {
						t.Fatalf("reservation does not consume exactly one GPU: %s", quantity.String())
					}
					if !cache.AddOrUpdateWorkload(logr.Discard(), item) {
						t.Fatal("could not preserve the reservation in the actual Kueue cache")
					}
					reserved = append(reserved, item)
					queue.AddUsage(unit)
					used++
					if used > 8 {
						t.Fatalf("consumer order %v step %d exposed more than the fixed 8 GPUs", order, step)
					}
				}
			}
			if step == len(order)-1 && used != 8 {
				t.Fatalf("consumer order %v exposed %d GPUs after complete ingestion, want 8", order, used)
			}
		}
		for _, item := range reserved {
			if err := cache.DeleteWorkload(logr.Discard(), workload.Key(item)); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Log("All six Cohort consumer orders preserve the fixed budget while workspace reservations interleave.")
}

func TestSharedTeamRejectsLiveQuotaEditsAndRecreation(t *testing.T) {
	ctx := context.Background()
	r, cluster, teams, _ := sharedTeamFixture(t, interceptor.Funcs{})
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	for _, amount := range []string{"2", "4"} {
		team := &tauv1alpha1.TauTeam{}
		if err := r.Get(ctx, client.ObjectKeyFromObject(teams[0]), team); err != nil {
			t.Fatal(err)
		}
		team.Spec.Quota[0].NominalQuota = resource.MustParse(amount)
		if err := r.Update(ctx, team); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil {
			t.Fatal("live quota redistribution was accepted")
		}
		assertPoolQuantity(t, getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName),
			"taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "2")
		assertPoolQuantity(t, getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(team.Name)),
			"taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "3")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(teams[0]), teams[0]); err != nil {
		t.Fatal(err)
	}
	teams[0].Spec.Quota[0].NominalQuota = resource.MustParse("3")
	if err := r.Update(ctx, teams[0]); err != nil {
		t.Fatal(err)
	}
	cohort := getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(teams[1].Name))
	if err := r.Delete(ctx, cohort); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil || !strings.Contains(err.Error(), "operator maintenance") {
		t.Fatalf("missing live Cohort was recreated without maintenance: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cohort), newQueueObject(cohortGVK)); !apierrors.IsNotFound(err) {
		t.Fatalf("missing Cohort was recreated: %v", err)
	}
}

func TestSharedTeamCombinedResourceGroupsRequireAlignment(t *testing.T) {
	ctx := context.Background()
	r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
	root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	root.Object["spec"] = map[string]any{"resourceGroups": quotaResourceGroups([]tauv1alpha1.TauResourceQuota{
		{Flavor: "taugrid-gpu-h200", Resource: "cpu", NominalQuota: resource.MustParse("31500m")},
	}, false)}
	if err := r.Update(ctx, root); err != nil {
		t.Fatal(err)
	}
	teams[1].Spec.Quota[0].Flavor = "taugrid-gpu-a100"
	if err := r.Update(ctx, teams[1]); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil || !strings.Contains(err.Error(), "identical or disjoint") {
		t.Fatalf("incompatible combined resource groups were accepted: %v", err)
	}
	for _, team := range teams {
		if err := r.Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, newQueueObject(cohortGVK)); !apierrors.IsNotFound(err) {
			t.Fatalf("invalid Team Cohort was created: %v", err)
		}
	}
	for _, workspace := range workspaces {
		if err := r.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(workspace.Name)}, newQueueObject(clusterQueueGVK)); !apierrors.IsNotFound(err) {
			t.Fatalf("invalid workspace queue was created: %v", err)
		}
	}
}

func TestSharedTeamZeroSpare(t *testing.T) {
	for _, groups := range []any{nil, []any{}} {
		t.Run(fmt.Sprintf("%T", groups), func(t *testing.T) {
			ctx := context.Background()
			r, cluster, _, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
			root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
			root.Object["spec"] = map[string]any{}
			if groups != nil {
				root.Object["spec"].(map[string]any)["resourceGroups"] = groups
			}
			if err := r.Update(ctx, root); err != nil {
				t.Fatal(err)
			}
			version := root.GetResourceVersion()
			if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
				t.Fatalf("zero-spare parent was rejected: %v", err)
			}
			root = getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
			if root.GetResourceVersion() != version {
				t.Fatal("zero-spare parent was rewritten")
			}
			for _, workspace := range workspaces {
				queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspace.Name))
				assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", "6")
			}
		})
	}
}

func TestSharedTeamDeletionPreservesEstablishedZeroQuotas(t *testing.T) {
	ctx := context.Background()
	r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
	root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	root.Object["spec"] = map[string]any{"resourceGroups": quotaResourceGroups([]tauv1alpha1.TauResourceQuota{
		{Flavor: "taugrid-gpu-h200", Resource: nvidiaGPUResourceName, NominalQuota: resource.MustParse("2")},
	}, false)}
	if err := r.Update(ctx, root); err != nil {
		t.Fatal(err)
	}
	rootVersion := root.GetResourceVersion()
	teams[1].Spec.Quota[0].Flavor = "taugrid-gpu-a100"
	if err := r.Update(ctx, teams[1]); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	survivor := getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(teams[0].Name))
	version := survivor.GetResourceVersion()
	queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspaces[1].Name))
	if err := r.Delete(ctx, queue); err != nil {
		t.Fatal(err)
	}
	workspaces[1].Finalizers = nil
	if err := r.Update(ctx, workspaces[1]); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, workspaces[1]); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, teams[1]); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(teams[1])}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
			t.Fatalf("deleting an unrelated guarantee wedged the surviving team: %v", err)
		}
	}
	survivor = getSharedQueue(t, r.Client, cohortGVK.Kind, survivor.GetName())
	if survivor.GetResourceVersion() != version {
		t.Fatal("surviving Team Cohort was rewritten")
	}
	assertPoolQuantity(t, survivor, "taugrid-gpu-a100", nvidiaGPUResourceName, "nominalQuota", "0")
	root = getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	if root.GetResourceVersion() != rootVersion {
		t.Fatal("deleted guarantee was transferred to the parent")
	}
	queue = getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspaces[0].Name))
	assertPoolQuantity(t, queue, "taugrid-gpu-a100", nvidiaGPUResourceName, "borrowingLimit", "0")
	policy, _, err := unstructured.NestedString(queue.Object, "spec", "stopPolicy")
	if err != nil || policy == "Hold" || policy == "HoldAndDrain" {
		t.Fatalf("surviving workspace was stopped: %q, %v", policy, err)
	}
}

func TestSharedTeamInventoryNeverRebalancesBudgets(t *testing.T) {
	ctx := context.Background()
	r, cluster, _, _ := sharedTeamFixture(t, interceptor.Funcs{})
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	version := root.GetResourceVersion()
	for _, amount := range []string{"16", "7", "0", "8"} {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: "h200"}, &node); err != nil {
			t.Fatal(err)
		}
		node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse(amount)
		if err := r.Status().Update(ctx, &node); err != nil {
			t.Fatal(err)
		}
		err := r.reconcileSharedTeamPool(ctx, cluster)
		if (amount == "7" || amount == "0") != (err != nil) {
			t.Fatalf("inventory=%s: unexpected validation result: %v", amount, err)
		}
		root = getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
		if root.GetResourceVersion() != version {
			t.Fatal("inventory changes wrote to the static spare Cohort")
		}
		assertPoolQuantity(t, root, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "2")
	}
}

func TestSharedTeamCreationRequiresMaintenanceWithExistingQueues(t *testing.T) {
	ctx := context.Background()
	r, cluster, _, _ := sharedTeamFixture(t, interceptor.Funcs{})
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: "h200"}, &node); err != nil {
		t.Fatal(err)
	}
	node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("16")
	if err := r.Status().Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	team := testTeam("new-team", "1")
	team.UID = "new-team"
	team.Finalizers = []string{teamFinalizer}
	team.Spec.Quota[0].BorrowingLimit = nil
	team.Spec.Quota[0].LendingLimit = nil
	if err := r.Create(ctx, team); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil || !strings.Contains(err.Error(), "operator maintenance") {
		t.Fatalf("new team quota was introduced into the live tree: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, newQueueObject(cohortGVK)); !apierrors.IsNotFound(err) {
		t.Fatalf("new Cohort exists despite maintenance requirement: %v", err)
	}
}

func TestSharedTeamStaticRootMustBeProvidedByItsOwner(t *testing.T) {
	for _, change := range []string{"missing", "automatic", "different cluster"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			r, cluster, _, _ := sharedTeamFixture(t, interceptor.Funcs{})
			root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
			if change == "missing" {
				if err := r.Delete(ctx, root); err != nil {
					t.Fatal(err)
				}
			} else {
				labels := root.GetLabels()
				if change == "automatic" {
					labels[labelManagedBy] = labelManagedByValue
				} else {
					labels[labelSharedTeamQuotas] = "different"
				}
				root.SetLabels(labels)
				if err := r.Update(ctx, root); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil {
				t.Fatal("static root ownership contract was not enforced")
			}
			if change == "missing" {
				if err := r.Get(ctx, client.ObjectKeyFromObject(root), newQueueObject(cohortGVK)); !apierrors.IsNotFound(err) {
					t.Fatalf("controller recreated the static spare: %v", err)
				}
			}
		})
	}
}

func TestSharedTeamShortageAndBorrowedReservations(t *testing.T) {
	ctx := context.Background()
	r, cluster, _, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspaces[0].Name))
	reservations := []any{map[string]any{"name": "taugrid-gpu-h200", "resources": []any{
		map[string]any{"name": nvidiaGPUResourceName, "total": "7"},
	}}}
	if err := unstructured.SetNestedSlice(queue.Object, reservations, "status", "flavorsReservation"); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatalf("legitimate usage above a team guarantee was treated as exceeding a hard cap: %v", err)
	}
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: "h200"}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("6")
	if err := r.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil || !strings.Contains(err.Error(), "active reservations exceed") {
		t.Fatalf("expected shortage while preserving admitted borrowers: %v", err)
	}
	queue = getSharedQueue(t, r.Client, clusterQueueGVK.Kind, queue.GetName())
	policy, _, _ := unstructured.NestedString(queue.Object, "spec", "stopPolicy")
	if policy != "Hold" {
		t.Fatalf("shortage must hold new admissions, not evict admitted jobs: %q", policy)
	}
	assertPoolQuantity(t, queue, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", "8")
	for _, amount := range []string{"5", "0"} {
		node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse(amount)
		if err := r.Status().Update(ctx, node); err != nil {
			t.Fatal(err)
		}
		if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil {
			t.Fatal("loss of physical supply must not be interpreted as unlimited capacity")
		}
	}
	node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("8")
	if err := r.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	queue = getSharedQueue(t, r.Client, clusterQueueGVK.Kind, queue.GetName())
	if policy, _, _ := unstructured.NestedString(queue.Object, "spec", "stopPolicy"); policy != "" {
		t.Fatal("controller-owned Hold must clear after capacity recovers")
	}
	if err := unstructured.SetNestedField(queue.Object, "Hold", "spec", "stopPolicy"); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, queue); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	queue = getSharedQueue(t, r.Client, clusterQueueGVK.Kind, queue.GetName())
	if policy, _, _ := unstructured.NestedString(queue.Object, "spec", "stopPolicy"); policy != "Hold" {
		t.Fatal("operator-owned Hold must not be cleared by capacity recovery")
	}
}

func TestSharedTeamOddResidualAndExplicitLimits(t *testing.T) {
	ctx := context.Background()
	r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: "h200"}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("7")
	if err := r.Status().Update(ctx, node); err != nil {
		t.Fatal(err)
	}
	root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	root.Object["spec"] = map[string]any{"resourceGroups": quotaResourceGroups([]tauv1alpha1.TauResourceQuota{
		{Flavor: "taugrid-gpu-h200", Resource: nvidiaGPUResourceName, NominalQuota: resource.MustParse("1")},
	}, false)}
	if err := r.Update(ctx, root); err != nil {
		t.Fatal(err)
	}
	zero := resource.Quantity{}
	teams[0].Spec.Quota[0].BorrowingLimit = &zero
	teams[0].Spec.Quota[0].LendingLimit = &zero
	if err := r.Update(ctx, teams[0]); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	root = getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	assertPoolQuantity(t, root, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "1")
	cohort := getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(teams[0].Name))
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", "0")
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "lendingLimit", "0")
	scheme := runtime.NewScheme()
	if err := kueue.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cache := kueuecache.New(fake.NewClientBuilder().WithScheme(scheme).
		WithIndex(&kueue.LocalQueue{}, kueueindex.QueueClusterQueueKey, kueueindex.IndexQueueClusterQueue).
		WithIndex(&kueue.Workload{}, kueueindex.WorkloadClusterQueueKey, kueueindex.IndexWorkloadClusterQueue).Build())
	cache.AddOrUpdateResourceFlavor(logr.Discard(), &kueue.ResourceFlavor{ObjectMeta: metav1.ObjectMeta{Name: "taugrid-gpu-h200"}})
	for _, name := range []string{sharedTeamCohortName, teamCohortName(teams[0].Name), teamCohortName(teams[1].Name)} {
		var typed kueue.Cohort
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(getSharedQueue(t, r.Client, cohortGVK.Kind, name).Object, &typed); err != nil {
			t.Fatal(err)
		}
		if err := cache.AddOrUpdateCohort(&typed); err != nil {
			t.Fatal(err)
		}
	}
	for _, workspace := range workspaces {
		var typed kueue.ClusterQueue
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspace.Name)).Object, &typed); err != nil {
			t.Fatal(err)
		}
		if err := cache.AddClusterQueue(ctx, &typed); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := cache.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fr := resources.FlavorResource{Flavor: "taugrid-gpu-h200", Resource: corev1.ResourceName(nvidiaGPUResourceName)}
	first := snapshot.ClusterQueue(kueue.ClusterQueueReference(workspaceClusterQueueName(workspaces[0].Name)))
	second := snapshot.ClusterQueue(kueue.ClusterQueueReference(workspaceClusterQueueName(workspaces[1].Name)))
	if first.Available(fr).CmpInt64(3) != 0 || second.Available(fr).CmpInt64(4) != 0 {
		t.Fatal("explicit zero must prevent the first team borrowing and lending, while the other can borrow the one static spare GPU")
	}
}

func TestSharedWorkspaceTeamReassignmentPreservesNamespace(t *testing.T) {
	ctx := context.Background()
	r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
	workspace := workspaces[1]
	namespace := testNamespace(workspace.Name)
	namespace.Annotations[annotationOwnerUID] = string(workspace.UID)
	namespace.Labels[labelTeam] = teams[0].Name
	namespace.Labels["keep"] = "data"
	if err := r.Create(ctx, namespace); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	reconciler := newTestWorkspaceReconciler(r.Client)
	if ready, err := reconciler.reconcileNamespace(ctx, workspace, namespace.Name, true); !ready || err != nil {
		t.Fatalf("same-UID team reassignment failed: %v", err)
	}
	var actual corev1.Namespace
	if err := r.Get(ctx, client.ObjectKeyFromObject(namespace), &actual); err != nil {
		t.Fatal(err)
	}
	if actual.UID != namespace.UID || actual.Labels[labelTeam] != teams[1].Name ||
		actual.Annotations[annotationOwnerUID] != string(workspace.UID) || actual.Labels["keep"] != "data" {
		t.Fatal("team reassignment must preserve the workspace's data namespace and authoritative UID")
	}
	workspace.UID = "other-workspace"
	if _, err := reconciler.reconcileNamespace(ctx, workspace, namespace.Name, true); err == nil {
		t.Fatal("team reassignment must not permit a different workspace UID to take over the namespace")
	}
}

func TestSharedWorkspaceTeamReassignmentUpdatesLocalQueues(t *testing.T) {
	for _, owner := range []string{"same-workspace", "different-uid", "external", "legacy-unbound"} {
		t.Run(owner, func(t *testing.T) {
			ctx := context.Background()
			r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
			workspace := workspaces[1]
			workspace.Spec.Queue = "jobqueue"
			if err := r.Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{workspaceLocalQueueName(workspace), "jobqueue"} {
				queue := newWorkspaceLocalQueue(workspace, workspace.Name, name, workspaceClusterQueueName(workspace.Name), true)
				queue.SetUID(types.UID("local-" + name))
				labels := queue.GetLabels()
				labels[labelTeam], labels["keep"] = teams[0].Name, "value"
				if name == "jobqueue" && owner == "external" {
					labels[labelManagedBy] = "external"
				}
				queue.SetLabels(labels)
				if name == "jobqueue" && owner == "different-uid" {
					setOwnerUIDAnnotation(queue, "different-workspace")
				}
				if name == "jobqueue" && owner == "legacy-unbound" {
					queue.SetAnnotations(nil)
				}
				queue.Object["spec"].(map[string]any)["stopPolicy"] = "Hold"
				if err := r.Create(ctx, queue); err != nil {
					t.Fatal(err)
				}
			}
			reconciler := newTestWorkspaceReconciler(r.Client)
			_, ready, message, err := reconciler.reconcileQueue(ctx, workspace, workspace.Name)
			if owner == "different-uid" {
				if ready {
					t.Fatal("LocalQueue team metadata must not change for another workspace UID")
				}
			} else if !ready || err != nil {
				t.Fatalf("reconcile LocalQueue metadata: %s: %v", message, err)
			}
			for _, name := range []string{workspaceLocalQueueName(workspace), "jobqueue"} {
				queue := newQueueObject(localQueueGVK)
				if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: workspace.Name}, queue); err != nil {
					t.Fatal(err)
				}
				expectedTeam := teams[1].Name
				if name == "jobqueue" && owner != "same-workspace" {
					expectedTeam = teams[0].Name
				}
				policy, _, _ := unstructured.NestedString(queue.Object, "spec", "stopPolicy")
				if queue.GetLabels()[labelTeam] != expectedTeam || queue.GetLabels()["keep"] != "value" ||
					queue.GetUID() != types.UID("local-"+name) || policy != "Hold" {
					t.Fatalf("LocalQueue %q did not preserve ownership/policy while reconciling its team: %#v", name, queue.Object)
				}
			}
		})
	}
}

func TestNodeClassificationByExactHardwareLabels(t *testing.T) {
	rules := []tauv1alpha1.TauNodeLabelRule{
		{Match: tauv1alpha1.TauNodeMatch{VMSizes: []string{"legacy-size"}}, Labels: map[string]string{labelkeys.LabelGPUClass: "legacy"}},
		{Match: tauv1alpha1.TauNodeMatch{Labels: map[string]string{"nvidia.com/gpu.product": "distinct-product", corev1.LabelArchStable: "arm64"}},
			Labels: map[string]string{labelkeys.LabelGPUClass: "distinct"}},
	}
	if err := validateNodeLabelRules(rules); err != nil {
		t.Fatal(err)
	}
	if err := validateNodeLabelRuleConflicts(rules); err != nil {
		t.Fatal(err)
	}
	for _, instanceType := range []string{"", "distinct-size"} {
		node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Labels: map[string]string{
			"nvidia.com/gpu.product": "distinct-product", corev1.LabelArchStable: "arm64", corev1.LabelInstanceTypeStable: instanceType,
		}}}
		desired, matched, err := desiredNodeLabels(node, rules)
		if err != nil || !matched || desired[labelkeys.LabelGPUClass] != "distinct" {
			t.Fatalf("hardware evidence must classify independently of missing instance type: %v %v", desired, err)
		}
		node.Labels[corev1.LabelArchStable] = "amd64"
		if _, matched, err := desiredNodeLabels(node, rules); matched || err != nil {
			t.Fatal("partial hardware evidence must not classify a node")
		}
		node.Labels[corev1.LabelArchStable] = "arm64"
		node.Labels[corev1.LabelInstanceTypeStable] = "legacy-size"
		if _, _, err := desiredNodeLabels(node, rules); err == nil {
			t.Fatal("actual conflicting hardware and instance-type matches must fail closed")
		}
	}
	emptyMatch := []tauv1alpha1.TauNodeLabelRule{{
		Match:  tauv1alpha1.TauNodeMatch{Labels: map[string]string{"hardware.example.com/verified": ""}},
		Labels: map[string]string{labelkeys.LabelGPUClass: "distinct"},
	}}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "missing-evidence", Labels: map[string]string{}}}
	if _, matched, err := desiredNodeLabels(node, emptyMatch); matched || err != nil {
		t.Fatal("an absent label must not satisfy a present-but-empty exact selector")
	}
	node.Labels["hardware.example.com/verified"] = ""
	if _, matched, err := desiredNodeLabels(node, emptyMatch); !matched || err != nil {
		t.Fatal("a present empty label must satisfy an exact empty-value selector")
	}
}

func TestSharedTeamRejectsConflictsAndUnclassifiedCapacity(t *testing.T) {
	for _, name := range []string{"unclassified", "external budget", "duplicate workspace", "wrong UID", "both modes", "workspace split"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, cluster, _, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
			switch name {
			case "unclassified":
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "unclassified", Labels: map[string]string{corev1.LabelOSStable: "linux"}},
					Status: corev1.NodeStatus{Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
						Allocatable: corev1.ResourceList{corev1.ResourceName(nvidiaGPUResourceName): resource.MustParse("1")}}}
				if err := r.Create(ctx, node); err != nil {
					t.Fatal(err)
				}
			case "external budget":
				external := desiredTeamCohort(testTeam("external", "1"), []tauv1alpha1.TauResourceQuota{
					{Flavor: "taugrid-gpu-h200", Resource: nvidiaGPUResourceName, NominalQuota: resource.MustParse("1")},
				})
				if err := r.Create(ctx, external); err != nil {
					t.Fatal(err)
				}
			case "duplicate workspace", "wrong UID":
				queue := desiredWorkspaceClusterQueue(workspaces[0])
				if name == "duplicate workspace" {
					queue.Object["spec"].(map[string]any)["resourceGroups"] = quotaResourceGroups([]tauv1alpha1.TauResourceQuota{
						{Flavor: "taugrid-gpu-h200", Resource: nvidiaGPUResourceName, NominalQuota: resource.MustParse("1")},
					}, true)
				} else {
					queue.SetAnnotations(map[string]string{annotationOwnerUID: "foreign"})
				}
				if err := r.Create(ctx, queue); err != nil {
					t.Fatal(err)
				}
			case "both modes":
				cluster.Spec.Queues.CapacityTeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "exclusive"}
			case "workspace split":
				workspaces[0].Spec.Quota = []tauv1alpha1.TauResourceQuota{{Flavor: "taugrid-gpu-h200", Resource: nvidiaGPUResourceName}}
				if err := r.Update(ctx, workspaces[0]); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil {
				t.Fatal("unsafe shared capacity configuration was accepted")
			}
		})
	}
}

func TestSharedTeamExternalBudgetOverlap(t *testing.T) {
	for _, name := range []string{"CPU-only node", "absent empty selector", "GPU alias", "present empty selector"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, cluster, _, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
			if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
				t.Fatal(err)
			}
			selector := map[string]any{"hardware.example.com/reserved": ""}
			switch name {
			case "CPU-only node":
				selector = map[string]any{"pool": "cpu"}
				if err := r.Create(ctx, &corev1.Node{
					ObjectMeta: metav1.ObjectMeta{Name: "outside-gpu-pool", Labels: map[string]string{
						corev1.LabelOSStable: "linux", "pool": "cpu",
					}},
					Status: corev1.NodeStatus{
						Conditions:  []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
						Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("4")},
					},
				}); err != nil {
					t.Fatal(err)
				}
			case "GPU alias":
				selector = map[string]any{labelkeys.LabelGPUClass: "h200"}
			case "present empty selector":
				var node corev1.Node
				if err := r.Get(ctx, client.ObjectKey{Name: "h200"}, &node); err != nil {
					t.Fatal(err)
				}
				node.Labels["hardware.example.com/reserved"] = ""
				if err := r.Update(ctx, &node); err != nil {
					t.Fatal(err)
				}
			}
			flavor := newQueueObject(resourceFlavorGVK)
			flavor.SetName("external-cpu")
			flavor.Object["spec"] = map[string]any{"nodeLabels": selector}
			if err := r.Create(ctx, flavor); err != nil {
				t.Fatal(err)
			}
			if err := r.Create(ctx, desiredTeamCohort(testTeam("external", "1"), []tauv1alpha1.TauResourceQuota{{
				Flavor: flavor.GetName(), Resource: "cpu", NominalQuota: resource.MustParse("1"),
			}})); err != nil {
				t.Fatal(err)
			}
			err := r.reconcileSharedTeamPool(ctx, cluster)
			if name == "GPU alias" || name == "present empty selector" {
				if err == nil || !strings.Contains(err.Error(), "overlaps") {
					t.Fatalf("overlapping physical budget was accepted: %v", err)
				}
				queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspaces[0].Name))
				if policy, _, _ := unstructured.NestedString(queue.Object, "spec", "stopPolicy"); policy != "Hold" {
					t.Fatalf("overlapping budget did not hold admissions: %q", policy)
				}
			} else if err != nil {
				t.Fatalf("disjoint external supply blocked shared GPU capacity: %v", err)
			}
		})
	}
}

func TestSharedTeamDeletionMembership(t *testing.T) {
	for _, name := range []string{"join", "leave", "retain"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
			workspace := workspaces[1]
			if name == "join" {
				workspace.Spec.TeamRef.Name = teams[0].Name
				if err := r.Update(ctx, workspace); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
				t.Fatal(err)
			}
			if err := r.Delete(ctx, teams[1]); err != nil {
				t.Fatal(err)
			}
			if name == "join" {
				workspace.Spec.TeamRef.Name = teams[1].Name
			} else if name == "leave" {
				workspace.Spec.TeamRef.Name = teams[0].Name
			}
			if err := r.Update(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			reconciler := newTestWorkspaceReconciler(r.Client)
			_, err := reconciler.reconcileWorkspaceClusterQueue(ctx, workspace)
			if name == "join" {
				if err == nil || !strings.Contains(err.Error(), "cannot join deleting team") {
					t.Fatalf("workspace joined a deleting team: %v", err)
				}
			} else if err != nil {
				t.Fatalf("existing workspace could not %s its deleting team: %v", name, err)
			}
			queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspace.Name))
			cohort, _, _ := unstructured.NestedString(queue.Object, "spec", "cohortName")
			expected := teams[0].Name
			if name == "retain" {
				expected = teams[1].Name
			}
			if cohort != teamCohortName(expected) {
				t.Fatalf("workspace cohort=%q, want %q", cohort, teamCohortName(expected))
			}
			root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
			assertPoolQuantity(t, root, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "2")
		})
	}
}

func TestSharedTeamFinalizerRetryDoesNotRecreateCohort(t *testing.T) {
	ctx := context.Background()
	failFinalizerUpdate := false
	r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.UpdateOption) error {
			if team, ok := object.(*tauv1alpha1.TauTeam); ok && failFinalizerUpdate && len(team.Finalizers) == 0 {
				failFinalizerUpdate = false
				return fmt.Errorf("finalizer update unavailable")
			}
			return c.Update(ctx, object, opts...)
		},
	})
	workspaces[1].Finalizers = nil
	if err := r.Update(ctx, workspaces[1]); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, workspaces[1]); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	cohort := getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(teams[1].Name))
	cohort.SetUID("original-cohort")
	if err := r.Update(ctx, cohort); err != nil {
		t.Fatal(err)
	}
	teams[1].Status.CohortUID = string(cohort.GetUID())
	if err := r.Status().Update(ctx, teams[1]); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, teams[1]); err != nil {
		t.Fatal(err)
	}
	failFinalizerUpdate = true
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(teams[1])}
	if _, err := r.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "finalizer update unavailable") {
		t.Fatalf("did not exercise a failed finalizer update: %v", err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cohort), newQueueObject(cohortGVK)); !apierrors.IsNotFound(err) {
		t.Fatalf("cohort was not deleted before the failed finalizer update: %v", err)
	}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(teams[0])}); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(cohort), newQueueObject(cohortGVK)); !apierrors.IsNotFound(err) {
		t.Fatalf("sibling reconciliation recreated a terminating team's Cohort: %v", err)
	}
	root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	assertPoolQuantity(t, root, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "2")
	if _, err := r.Reconcile(ctx, request); err != nil {
		t.Fatalf("team finalization did not recover: %v", err)
	}
	if err := r.Get(ctx, request.NamespacedName, &tauv1alpha1.TauTeam{}); !apierrors.IsNotFound(err) {
		t.Fatalf("terminating team remains after the successful finalizer retry: %v", err)
	}
}

func TestSharedTeamHoldRecoveryPreservesOperatorPolicy(t *testing.T) {
	for _, policy := range []string{"HoldAndDrain", "None", "Hold"} {
		t.Run(policy, func(t *testing.T) {
			ctx := context.Background()
			r, cluster, _, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
			if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
				t.Fatal(err)
			}

			if err := r.holdSharedTeamQueues(ctx); err != nil {
				t.Fatal(err)
			}
			queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspaces[0].Name))
			if err := unstructured.SetNestedField(queue.Object, policy, "spec", "stopPolicy"); err != nil {
				t.Fatal(err)
			}
			if policy == "Hold" {
				annotations := queue.GetAnnotations()
				delete(annotations, annotationSharedQuotaHold)
				queue.SetAnnotations(annotations)
			}
			if err := r.Update(ctx, queue); err != nil {
				t.Fatal(err)
			}
			if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
				t.Fatal(err)
			}
			queue = getSharedQueue(t, r.Client, clusterQueueGVK.Kind, queue.GetName())
			if actual, _, _ := unstructured.NestedString(queue.Object, "spec", "stopPolicy"); actual != policy {
				t.Fatalf("recovery cleared operator policy %q: got %q", policy, actual)
			}
			if queue.GetAnnotations()[annotationSharedQuotaHold] != "" {
				t.Fatal("recovered operator policy retains controller hold ownership")
			}
		})
	}
}

func TestSharedDeletingWorkspaceRecoversHoldWithPendingWork(t *testing.T) {
	ctx := context.Background()
	r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
	workspace := workspaces[0]
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	namespace := testNamespace(workspace.Name)
	namespace.Annotations[annotationOwnerUID] = string(workspace.UID)
	namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
	queueName := workspaceClusterQueueName(workspace.Name)
	pending := newQueueObject(workloadGVK)
	pending.SetName("pending")
	pending.SetNamespace(namespace.Name)
	pending.Object["spec"] = map[string]any{"queueName": workspace.Spec.Queue}
	for _, object := range []client.Object{namespace, testLocalQueue(namespace.Name, workspace.Spec.Queue, queueName), pending} {
		if err := r.Create(ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: "h200"}, &node); err != nil {
		t.Fatal(err)
	}
	node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("7")
	if err := r.Status().Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err == nil {
		t.Fatal("shortage did not hold queues")
	}
	if err := r.Delete(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	cleanup := &TauWorkspaceReconciler{Client: r.Client}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}
	if _, err := cleanup.Reconcile(ctx, request); err == nil || !strings.Contains(err.Error(), "waiting for Workload") {
		t.Fatalf("deletion did not retain pending work: %v", err)
	}
	queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, queueName)
	before, _, err := unstructured.NestedMap(queue.Object, "spec")
	if err != nil {
		t.Fatal(err)
	}
	capacity := node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)]
	t.Logf("Shortage: capacity=%s, deleting Workspace stopPolicy=%v; pending Workload retained",
		capacity.String(), before["stopPolicy"])
	delete(before, "stopPolicy")
	node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("8")
	if err := r.Status().Update(ctx, &node); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	queue = getSharedQueue(t, r.Client, clusterQueueGVK.Kind, queueName)
	if policy, _, err := unstructured.NestedString(queue.Object, "spec", "stopPolicy"); err != nil || policy == "Hold" {
		t.Fatalf("deleting workspace remained on Hold after recovery: %q, %v", policy, err)
	}
	after, _, err := unstructured.NestedMap(queue.Object, "spec")
	if err != nil || !reflect.DeepEqual(before, after) || queue.GetAnnotations()[annotationSharedQuotaHold] != "" {
		t.Fatalf("recovery changed the queue contract or retained its Hold marker: %v", err)
	}
	capacity = node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)]
	t.Logf("Recovery: capacity=%s, deleting Workspace stopPolicy=%v; queue routing and quota unchanged",
		capacity.String(), after["stopPolicy"])
	assertPoolQuantity(t, getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(teams[0].Name)),
		"taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "3")
	if err := unstructured.SetNestedSlice(pending.Object, []any{map[string]any{"type": "Finished", "status": "True"}}, "status", "conditions"); err != nil {
		t.Fatal(err)
	}
	if err := r.Update(ctx, pending); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanup.Reconcile(ctx, request); err != nil {
		t.Fatalf("workspace could not finish deletion after work completed: %v", err)
	}
	if err := r.Get(ctx, request.NamespacedName, &tauv1alpha1.TauWorkspace{}); !apierrors.IsNotFound(err) {
		t.Fatalf("workspace finalizer remains: %v", err)
	}
	t.Log("Work completed: Workspace cleanup and finalizer removal finished.")
}

func TestSharedWorkspaceDeletionAfterListDoesNotRecreateQueue(t *testing.T) {
	ctx := context.Background()
	armed := false
	var target *tauv1alpha1.TauWorkspace
	r, cluster, _, workspaces := sharedTeamFixture(t, interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, objects client.ObjectList, opts ...client.ListOption) error {
			if err := c.List(ctx, objects, opts...); err != nil {
				return err
			}
			if _, ok := objects.(*tauv1alpha1.TauWorkspaceList); ok && armed {
				armed = false
				if err := c.Delete(ctx, target); err != nil {
					return err
				}
				_, err := (&TauWorkspaceReconciler{Client: c}).Reconcile(ctx,
					ctrl.Request{NamespacedName: client.ObjectKeyFromObject(target)})
				return err
			}
			return nil
		},
	})
	target = workspaces[0]
	target.Annotations = map[string]string{annotationOwnerUID: string(target.UID)}
	if err := r.Update(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	armed = true
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(target.Name)}, newQueueObject(clusterQueueGVK)); !apierrors.IsNotFound(err) {
		t.Fatalf("cleanup's removed queue was recreated from the stale Workspace list: %v", err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatalf("orphaned queue stopped surviving workspaces: %v", err)
	}
}

func TestSharedWorkspaceCleanupRefreshesIdentityUnderQuotaLock(t *testing.T) {
	ctx := context.Background()
	cleanupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	initialRead := make(chan struct{})
	reads := 0
	r, cluster, _, workspaces := sharedTeamFixture(t, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, object, opts...)
			if _, ok := object.(*tauv1alpha1.TauWorkspace); ok && ctx == cleanupCtx && err == nil {
				reads++
				if reads == 1 {
					close(initialRead)
				}
			}
			return err
		},
	})
	workspace := workspaces[0]
	workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
	if err := r.Update(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	if err := r.Delete(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	unlock := lockTeamQuota()
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	done := make(chan error, 1)
	go func() {
		_, err := (&TauWorkspaceReconciler{Client: r.Client}).Reconcile(cleanupCtx,
			ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)})
		done <- err
	}()
	select {
	case <-initialRead:
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not read its initial Workspace snapshot")
	}
	select {
	case err := <-done:
		t.Fatalf("cleanup bypassed the shared quota lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	old := &tauv1alpha1.TauWorkspace{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(workspace), old); err != nil {
		t.Fatal(err)
	}
	old.Finalizers = nil
	if err := r.Update(ctx, old); err != nil {
		t.Fatal(err)
	}
	replacement := workspace.DeepCopy()
	replacement.ResourceVersion = ""
	replacement.UID = "replacement-workspace"
	replacement.Annotations[annotationOwnerUID] = string(replacement.UID)
	if err := r.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	unlock()
	locked = false
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("cleanup did not reject its stale Workspace UID: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cleanup did not resume after the quota lock was released")
	}
	if err := r.Get(ctx, client.ObjectKeyFromObject(replacement), replacement); err != nil {
		t.Fatal(err)
	}
	if len(replacement.Finalizers) == 0 {
		t.Fatal("cleanup removed the replacement Workspace's finalizer")
	}
	getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspace.Name))
}

func TestSharedDeletingWorkspaceHoldRecoveryPreservesQueue(t *testing.T) {
	for _, policy := range []string{"Hold", "HoldAndDrain", "None"} {
		for _, owned := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/controller-owned=%v", policy, owned), func(t *testing.T) {
				ctx := context.Background()
				r, cluster, teams, workspaces := sharedTeamFixture(t, interceptor.Funcs{})
				if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
					t.Fatal(err)
				}
				queue := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, workspaceClusterQueueName(workspaces[0].Name))
				queue.SetUID("existing-queue")
				annotations := queue.GetAnnotations()
				annotations["keep"] = "operator-data"
				if owned {
					annotations[annotationSharedQuotaHold] = "true"
				}
				queue.SetAnnotations(annotations)
				if err := unstructured.SetNestedField(queue.Object, policy, "spec", "stopPolicy"); err != nil {
					t.Fatal(err)
				}
				if err := r.Update(ctx, queue); err != nil {
					t.Fatal(err)
				}
				before, _, err := unstructured.NestedMap(queue.Object, "spec")
				if err != nil {
					t.Fatal(err)
				}
				if owned && policy == "Hold" {
					delete(before, "stopPolicy")
				}
				workspaces[0].Spec.TeamRef.Name = teams[1].Name
				if err := r.Update(ctx, workspaces[0]); err != nil {
					t.Fatal(err)
				}
				if err := r.Delete(ctx, workspaces[0]); err != nil {
					t.Fatal(err)
				}
				if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
					t.Fatal(err)
				}
				after := getSharedQueue(t, r.Client, clusterQueueGVK.Kind, queue.GetName())
				spec, _, err := unstructured.NestedMap(after.Object, "spec")
				if err != nil || !reflect.DeepEqual(spec, before) || after.GetUID() != queue.GetUID() ||
					after.GetAnnotations()["keep"] != "operator-data" {
					t.Fatalf("deleting queue was recreated, retargeted or its operator policy changed: %v", err)
				}
				if err := r.Delete(ctx, after); err != nil {
					t.Fatal(err)
				}
				if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
					t.Fatal(err)
				}
				if err := r.Get(ctx, client.ObjectKeyFromObject(after), newQueueObject(clusterQueueGVK)); !apierrors.IsNotFound(err) {
					t.Fatalf("missing deleting queue was recreated: %v", err)
				}
			})
		}
	}
}

func getSharedQueue(t *testing.T, c client.Client, kind, name string) *unstructured.Unstructured {
	t.Helper()
	gvk := cohortGVK
	if kind == clusterQueueGVK.Kind {
		gvk = clusterQueueGVK
	}
	object := newQueueObject(gvk)
	if err := c.Get(context.Background(), client.ObjectKey{Name: name}, object); err != nil {
		t.Fatal(err)
	}
	return object
}
