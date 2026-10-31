// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuecache "sigs.k8s.io/kueue/pkg/cache/scheduler"
	kueueindex "sigs.k8s.io/kueue/pkg/controller/core/indexer"
	"sigs.k8s.io/kueue/pkg/resources"
	"sigs.k8s.io/kueue/pkg/workload"
)

func sharedTeamFixture(t *testing.T, intercept interceptor.Funcs) (*TauTeamReconciler, *tauv1alpha1.TauCluster, []*tauv1alpha1.TauTeam, []*tauv1alpha1.TauWorkspace) {
	t.Helper()
	cluster, _, workspaces, nodes := capacityPoolFixture()
	cluster.UID = "shared-cluster"
	cluster.Spec.Queues.CapacityTeamRef = nil
	cluster.Spec.Queues.ShareTeamQuotas = true
	teams := []*tauv1alpha1.TauTeam{}
	objects := []client.Object{cluster}
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

func TestSharedTeamResidualAndBorrowingContract(t *testing.T) {
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
	t.Logf("GPU supply=8; team guarantees=3+3; parent residual=%s; workspace nominal=0",
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
}

func TestSharedTeamOrderedQuotaTransfers(t *testing.T) {
	ctx := context.Background()
	r, cluster, teams, _ := sharedTeamFixture(t, interceptor.Funcs{})
	if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
		t.Fatal(err)
	}
	var snapshots []string
	base := r.Client
	r.Client = interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if err := c.Update(ctx, obj, opts...); err != nil {
				return err
			}
			cohorts := &unstructured.UnstructuredList{}
			cohorts.SetGroupVersionKind(cohortGVK.GroupVersion().WithKind("CohortList"))
			if err := c.List(ctx, cohorts); err != nil {
				return err
			}
			total := resource.Quantity{}
			for i := range cohorts.Items {
				quota, err := clusterQueueNominalQuota(&cohorts.Items[i])
				if err != nil {
					return err
				}
				total.Add(quota[nvidiaGPUResourceName+"\x00taugrid-gpu-h200"])
			}
			if total.Cmp(resource.MustParse("8")) > 0 {
				t.Fatalf("transient duplicate physical budget after updating %s: %s", obj.GetName(), total.String())
			}
			if obj.GetObjectKind().GroupVersionKind().Kind == cohortGVK.Kind {
				snapshots = append(snapshots, obj.GetName())
			}
			return nil
		},
	})
	for _, amount := range []string{"4", "2"} {
		team := &tauv1alpha1.TauTeam{}
		if err := base.Get(ctx, client.ObjectKeyFromObject(teams[0]), team); err != nil {
			t.Fatal(err)
		}
		team.Spec.Quota[0].NominalQuota = resource.MustParse(amount)
		if err := base.Update(ctx, team); err != nil {
			t.Fatal(err)
		}
		snapshots = nil
		if err := r.reconcileSharedTeamPool(ctx, cluster); err != nil {
			t.Fatal(err)
		}
		if len(snapshots) < 2 {
			t.Fatalf("missing ordered transfers: %v", snapshots)
		}
		if amount == "4" && snapshots[0] != sharedTeamCohortName ||
			amount == "2" && snapshots[len(snapshots)-1] != sharedTeamCohortName {
			t.Fatalf("incorrect residual transfer order: %v", snapshots)
		}
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
	r, cluster, teams, _ := sharedTeamFixture(t, interceptor.Funcs{})
	node := &corev1.Node{}
	if err := r.Get(ctx, client.ObjectKey{Name: "h200"}, node); err != nil {
		t.Fatal(err)
	}
	node.Status.Allocatable[corev1.ResourceName(nvidiaGPUResourceName)] = resource.MustParse("7")
	if err := r.Status().Update(ctx, node); err != nil {
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
	root := getSharedQueue(t, r.Client, cohortGVK.Kind, sharedTeamCohortName)
	assertPoolQuantity(t, root, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota", "1")
	cohort := getSharedQueue(t, r.Client, cohortGVK.Kind, teamCohortName(teams[0].Name))
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit", "0")
	assertPoolQuantity(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "lendingLimit", "0")
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
