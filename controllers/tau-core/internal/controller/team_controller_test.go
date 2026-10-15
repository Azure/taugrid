// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestTeamRejectsWorkspaceGuaranteesAboveAllocation(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	team := testTeam("vision", "8")
	workspace := testWorkspace("training")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "12", "0", "0")}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(flavor, team, workspace).
		WithStatusSubresource(&tauv1alpha1.TauTeam{}).
		Build()
	reconciler := &TauTeamReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: team.Name, Namespace: team.Namespace}}
	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("team reconcile iteration %d: %v", i, err)
		}
	}
	var got tauv1alpha1.TauTeam
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("Get team: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.TeamPhaseDegraded {
		t.Fatalf("team phase = %q, want Degraded", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQuotaReady, metav1.ConditionFalse)
}

func TestTeamReductionBlocksBelowBorrowedReservations(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	team := testTeam("vision", "8")
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "12", "0")}
	queue := testWorkspaceClusterQueue(workspace)
	if err := unstructured.SetNestedSlice(queue.Object, []any{
		map[string]any{
			"name": "taugrid-gpu-h200",
			"resources": []any{
				map[string]any{"name": nvidiaGPUResourceName, "total": "12"},
			},
		},
	}, "status", "flavorsReservation"); err != nil {
		t.Fatalf("set reservation status: %v", err)
	}
	cohort := desiredTeamCohort(team, []tauv1alpha1.TauResourceQuota{
		testGPUQuota("taugrid-gpu-h200", "12", "0", "0"),
	})
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, workspace, queue, cohort).
		Build()
	reconciler := &TauTeamReconciler{Client: c}

	err := reconciler.reconcileTeamCohort(ctx, team, []tauv1alpha1.TauResourceQuota{
		testGPUQuota("taugrid-gpu-h200", "4", "0", "0"),
	})
	if err == nil || !strings.Contains(err.Error(), "below active reservations 12") {
		t.Fatalf("reservation validation error = %v", err)
	}
	gotCohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: cohort.GetName()}, gotCohort); err != nil {
		t.Fatalf("Get Cohort: %v", err)
	}
	if got := quotaFromResourceGroups(t, gotCohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "12" {
		t.Fatalf("shared Cohort quota after blocked Team reduction = %q, want 12", got)
	}
}

func TestTeamCapacityRetainsAppliedAllocationDuringReduction(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Status: tauv1alpha1.TauClusterStatus{
			DiscoveredCapacity: []tauv1alpha1.TauResourceCapacityStatus{{
				Flavor:   "taugrid-gpu-h200",
				Resource: nvidiaGPUResourceName,
				Capacity: resource.MustParse("20"),
			}},
		},
	}
	vision := testTeam("vision", "8")
	vision.UID = types.UID("vision-team-uid")
	language := testTeam("language", "8")
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: vision.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "12", "0")}
	queue := testWorkspaceClusterQueue(workspace)
	cohort := desiredTeamCohort(vision, []tauv1alpha1.TauResourceQuota{
		testGPUQuota("taugrid-gpu-h200", "12", "0", "0"),
	})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, vision, language, workspace, queue, cohort).
		Build()
	reconciler := &TauTeamReconciler{Client: c}

	err := reconciler.validateTeamCapacity(ctx, language)
	if err == nil || !strings.Contains(err.Error(), "team allocations 24 exceed discovered capacity 20") {
		t.Fatalf("capacity validation error = %v", err)
	}
	if err := reconciler.validateTeamCapacity(ctx, vision); err != nil {
		t.Fatalf("reducing team capacity validation: %v", err)
	}
}

func TestTeamCapacityAllowsRequestedRebalanceToConverge(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Status: tauv1alpha1.TauClusterStatus{
			DiscoveredCapacity: []tauv1alpha1.TauResourceCapacityStatus{{
				Flavor:   "taugrid-gpu-h200",
				Resource: nvidiaGPUResourceName,
				Capacity: resource.MustParse("8"),
			}},
		},
	}
	vision := testTeam("vision", "4")
	vision.UID = types.UID("vision-team-uid")
	language := testTeam("language", "4")
	language.UID = types.UID("language-team-uid")
	visionCohort := desiredTeamCohort(vision, []tauv1alpha1.TauResourceQuota{
		testGPUQuota("taugrid-gpu-h200", "8", "0", "0"),
	})
	languageCohort := desiredTeamCohort(language, []tauv1alpha1.TauResourceQuota{
		testGPUQuota("taugrid-gpu-h200", "8", "0", "0"),
	})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster, vision, language, visionCohort, languageCohort).
		Build()
	reconciler := &TauTeamReconciler{Client: c}

	if err := reconciler.validateTeamCapacity(ctx, vision); err != nil {
		t.Fatalf("first reducing team capacity validation: %v", err)
	}
	if err := reconciler.reconcileTeamCohort(ctx, vision, vision.Spec.Quota); err != nil {
		t.Fatalf("first reducing team Cohort reconcile: %v", err)
	}
	if err := reconciler.validateTeamCapacity(ctx, language); err != nil {
		t.Fatalf("second reducing team capacity validation: %v", err)
	}
	if err := reconciler.reconcileTeamCohort(ctx, language, language.Spec.Quota); err != nil {
		t.Fatalf("second reducing team Cohort reconcile: %v", err)
	}
	for _, team := range []*tauv1alpha1.TauTeam{vision, language} {
		gotCohort := newQueueObject(cohortGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, gotCohort); err != nil {
			t.Fatalf("Get %s Cohort: %v", team.Name, err)
		}
		if got := quotaFromResourceGroups(t, gotCohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "4" {
			t.Fatalf("%s Cohort quota after capacity scale-down = %q, want 4", team.Name, got)
		}
	}
}

func TestTeamCohortRejectsStaleWorkspaceAccountingSnapshot(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	team := testTeam("vision", "16")
	team.UID = types.UID("team-uid")
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "12", "0", "0")}
	queue := testWorkspaceClusterQueue(workspace)
	cohort := desiredTeamCohort(team, []tauv1alpha1.TauResourceQuota{
		testGPUQuota("taugrid-gpu-h200", "4", "0", "0"),
	})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, workspace, queue, cohort).
		Build()
	reconciler := &TauTeamReconciler{Client: c}
	staleSharedQuota := []tauv1alpha1.TauResourceQuota{
		testGPUQuota("taugrid-gpu-h200", "12", "0", "0"),
	}

	err := reconciler.reconcileTeamCohort(ctx, team, staleSharedQuota)
	if err == nil || !strings.Contains(err.Error(), "stale Cohort accounting snapshot") {
		t.Fatalf("Cohort reconcile error = %v, want stale accounting refusal", err)
	}
	gotCohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: cohort.GetName()}, gotCohort); err != nil {
		t.Fatalf("Get Cohort: %v", err)
	}
	if got := quotaFromResourceGroups(t, gotCohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "4" {
		t.Fatalf("Cohort quota after stale reconcile = %q, want 4", got)
	}
}

func TestTeamReconcileRefreshesAllocationAfterWaitingForQuotaTransfer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	team := testTeam("vision", "16")
	team.UID = types.UID("team-uid")
	team.Finalizers = []string{teamFinalizer}
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	queue := testWorkspaceClusterQueue(workspace)
	cohort := desiredTeamCohort(team, []tauv1alpha1.TauResourceQuota{
		testGPUQuota("taugrid-gpu-h200", "12", "0", "0"),
	})

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(flavor, team, workspace, queue, cohort).
		WithStatusSubresource(&tauv1alpha1.TauTeam{}).
		Build()
	reconciler := &TauTeamReconciler{Client: c}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(team)}
	unlock := lockTeamQuota(teamQuotaLockKey(team.Namespace, team.Name))
	done := make(chan error, 1)
	go func() {
		_, err := reconciler.Reconcile(ctx, req)
		done <- err
	}()
	select {
	case err := <-done:
		unlock()
		t.Fatalf("Team reconcile completed before workspace transfer lock released: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	var updatedTeam tauv1alpha1.TauTeam
	if err := c.Get(ctx, client.ObjectKeyFromObject(team), &updatedTeam); err != nil {
		unlock()
		t.Fatalf("Get Team: %v", err)
	}
	updatedTeam.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")}
	if err := c.Update(ctx, &updatedTeam); err != nil {
		unlock()
		t.Fatalf("Update Team: %v", err)
	}
	updatedCohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: cohort.GetName()}, updatedCohort); err != nil {
		unlock()
		t.Fatalf("Get Cohort: %v", err)
	}
	setClusterQueueNominalQuota(t, updatedCohort, "4")
	if err := c.Update(ctx, updatedCohort); err != nil {
		unlock()
		t.Fatalf("Update Cohort: %v", err)
	}
	unlock()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Team reconcile after workspace transfer: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Team reconcile did not resume after workspace transfer")
	}
	gotCohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: cohort.GetName()}, gotCohort); err != nil {
		t.Fatalf("Get final Cohort: %v", err)
	}
	if got := quotaFromResourceGroups(t, gotCohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "4" {
		t.Fatalf("Cohort quota after interleaved Team reconcile = %q, want 4", got)
	}
}

func TestQuotaResourceGroupsKeepFlavorUniqueAcrossResources(t *testing.T) {
	cpuBorrowing := resource.MustParse("2")
	cpuLending := resource.MustParse("1")
	quotas := []tauv1alpha1.TauResourceQuota{
		testGPUQuota("shared-flavor", "8", "4", "0"),
		{
			Flavor:         "shared-flavor",
			Resource:       string(corev1.ResourceCPU),
			NominalQuota:   resource.MustParse("32"),
			BorrowingLimit: &cpuBorrowing,
			LendingLimit:   &cpuLending,
		},
	}
	groups := quotaResourceGroups(quotas, true)
	if len(groups) != 1 {
		t.Fatalf("resourceGroups = %#v, want one group for shared flavor", groups)
	}
	group := groups[0].(map[string]any)
	covered := group["coveredResources"].([]any)
	if got := fmt.Sprint(covered); got != "[cpu nvidia.com/gpu]" {
		t.Fatalf("coveredResources = %s", got)
	}
	flavors := group["flavors"].([]any)
	if len(flavors) != 1 {
		t.Fatalf("flavors = %#v, want shared flavor exactly once", flavors)
	}
	flavor := flavors[0].(map[string]any)
	if flavor["name"] != "shared-flavor" {
		t.Fatalf("flavor name = %v", flavor["name"])
	}
	resources := flavor["resources"].([]any)
	if len(resources) != len(covered) {
		t.Fatalf("flavor resources = %#v, coveredResources = %#v", resources, covered)
	}
	for index, resourceName := range covered {
		resourceQuota := resources[index].(map[string]any)
		if resourceQuota["name"] != resourceName {
			t.Fatalf("resource[%d] = %v, covered resource = %v", index, resourceQuota["name"], resourceName)
		}
	}
}

func TestQuotaValuesRejectPartiallyOverlappingFlavorResources(t *testing.T) {
	quotas := []tauv1alpha1.TauResourceQuota{
		testGPUQuota("combined-flavor", "8", "0", "0"),
		{
			Flavor:       "combined-flavor",
			Resource:     string(corev1.ResourceCPU),
			NominalQuota: resource.MustParse("32"),
		},
		testGPUQuota("gpu-only-flavor", "4", "0", "0"),
	}

	err := validateQuotaValues(quotas)
	if err == nil || !strings.Contains(err.Error(), "must cover identical or disjoint resource sets") {
		t.Fatalf("quota validation error = %v", err)
	}
}

func TestTeamDeletionWaitsForAppliedWorkspaceCohort(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")
	team.Finalizers = []string{teamFinalizer}
	now := metav1.Now()
	team.DeletionTimestamp = &now

	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	queue := testWorkspaceClusterQueue(workspace)
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "missing-team"}

	cohort := desiredTeamCohort(team, team.Spec.Quota)
	cohort.SetUID(types.UID("cohort-uid"))
	team.Status.CohortUID = string(cohort.GetUID())

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(team, workspace, queue, cohort).
		WithStatusSubresource(&tauv1alpha1.TauTeam{}).
		Build()
	reconciler := &TauTeamReconciler{Client: c}
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(team)}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("Reconcile deleting Team: %v", err)
	}
	var remaining tauv1alpha1.TauTeam
	if err := c.Get(ctx, req.NamespacedName, &remaining); err != nil {
		t.Fatalf("Team disappeared while ClusterQueue still used Cohort: %v", err)
	}
	if !controllerutil.ContainsFinalizer(&remaining, teamFinalizer) {
		t.Fatal("Team finalizer was removed while an applied ClusterQueue still used its Cohort")
	}
	deletionBlocked := findCondition(remaining.Status.Conditions, tauv1alpha1.ConditionQuotaReady)
	if deletionBlocked == nil ||
		deletionBlocked.Reason != tauv1alpha1.ConditionDeletionBlocked ||
		!strings.Contains(deletionBlocked.Message, workspace.Name) {
		t.Fatalf("QuotaReady = %#v, want applied-Cohort deletion block", deletionBlocked)
	}
	remainingCohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: cohort.GetName()}, remainingCohort); err != nil {
		t.Fatalf("Team Cohort was deleted while still applied: %v", err)
	}
}

type blockingWorkspaceSnapshotReader struct {
	client.Reader
	captured chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (r *blockingWorkspaceSnapshotReader) List(
	ctx context.Context,
	list client.ObjectList,
	opts ...client.ListOption,
) error {
	if err := r.Reader.List(ctx, list, opts...); err != nil {
		return err
	}
	if _, ok := list.(*tauv1alpha1.TauWorkspaceList); ok {
		r.once.Do(func() {
			close(r.captured)
			select {
			case <-r.release:
			case <-ctx.Done():
			}
		})
	}
	return nil
}

func TestTeamRejectsAggregateAllocationsAboveDiscoveredCapacity(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	cluster := &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName},
		Status: tauv1alpha1.TauClusterStatus{
			DiscoveredCapacity: []tauv1alpha1.TauResourceCapacityStatus{{
				Flavor:   "taugrid-gpu-h200",
				Resource: nvidiaGPUResourceName,
				Capacity: resource.MustParse("12"),
			}},
		},
	}
	vision := testTeam("vision", "8")
	language := testTeam("language", "8")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(flavor, cluster, vision, language).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}, &tauv1alpha1.TauTeam{}).
		Build()
	reconciler := &TauTeamReconciler{Client: c}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: vision.Name, Namespace: vision.Namespace}}
	for i := 0; i < 2; i++ {
		if _, err := reconciler.Reconcile(ctx, req); err != nil {
			t.Fatalf("team reconcile iteration %d: %v", i, err)
		}
	}

	var got tauv1alpha1.TauTeam
	if err := c.Get(ctx, req.NamespacedName, &got); err != nil {
		t.Fatalf("Get team: %v", err)
	}
	if got.Status.Phase != tauv1alpha1.TeamPhaseDegraded {
		t.Fatalf("team phase = %q, want Degraded", got.Status.Phase)
	}
	assertCondition(t, got.Status.Conditions, tauv1alpha1.ConditionQuotaReady, metav1.ConditionFalse)
}

func TestAppliedQueueRemainsChargedToOldTeamDuringTeamMigration(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	oldTeam := testTeam("vision", "16")
	oldTeam.UID = types.UID("old-team-uid")
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "missing-team"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	queue := testWorkspaceClusterQueue(workspace)
	if err := unstructured.SetNestedField(queue.Object, teamCohortName(oldTeam.Name), "spec", "cohortName"); err != nil {
		t.Fatalf("set old Cohort: %v", err)
	}
	setClusterQueueNominalQuota(t, queue, "8")

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(oldTeam, workspace, queue).
		Build()
	shared, err := (&TauTeamReconciler{Client: c}).sharedTeamQuota(ctx, oldTeam)
	if err != nil {
		t.Fatal(err)
	}
	if got := shared[0].NominalQuota.String(); got != "8" {
		t.Fatalf("old team shared quota during migration = %q, want 8", got)
	}
}

func setClusterQueueNominalQuota(t *testing.T, queue *unstructured.Unstructured, value string) {
	setClusterQueueQuotaField(t, queue, "nominalQuota", value)
}

type staleTypedCacheClient struct {
	client.Client
	Reader client.Reader
}

func (c *staleTypedCacheClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	switch obj.(type) {
	case *tauv1alpha1.TauTeam, *tauv1alpha1.TauWorkspace:
		return c.Reader.Get(ctx, key, obj, opts...)
	default:
		return c.Client.Get(ctx, key, obj, opts...)
	}
}

func (c *staleTypedCacheClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	switch list.(type) {
	case *tauv1alpha1.TauTeamList, *tauv1alpha1.TauWorkspaceList:
		return c.Reader.List(ctx, list, opts...)
	default:
		return c.Client.List(ctx, list, opts...)
	}
}

func setClusterQueueQuotaField(
	t *testing.T,
	queue *unstructured.Unstructured,
	field, value string,
) {
	t.Helper()
	groups, found, err := unstructured.NestedSlice(queue.Object, "spec", "resourceGroups")
	if err != nil || !found {
		t.Fatalf("read ClusterQueue resourceGroups: found=%v err=%v", found, err)
	}
	group := groups[0].(map[string]any)
	flavors := group["flavors"].([]any)
	flavor := flavors[0].(map[string]any)
	resources := flavor["resources"].([]any)
	resourceQuota := resources[0].(map[string]any)
	resourceQuota[field] = value
	if err := unstructured.SetNestedSlice(queue.Object, groups, "spec", "resourceGroups"); err != nil {
		t.Fatalf("write ClusterQueue resourceGroups: %v", err)
	}
}

func TestManagedQuotaObjectsRejectUnannotatedLegacyOwnership(t *testing.T) {
	scheme := testScheme(t)
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")
	desired := desiredTeamCohort(team, team.Spec.Quota)
	existing := desired.DeepCopy()
	existing.SetAnnotations(nil)
	existing.SetUID(types.UID("legacy-object-uid"))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	err := reconcileManagedUnstructuredWithReader(
		context.Background(),
		c,
		c,
		desired,
		labelTeam,
		team.Name,
	)
	if err == nil || !strings.Contains(err.Error(), "legacy ownership metadata") {
		t.Fatalf("reconcile error = %v, want legacy ownership refusal", err)
	}
}

func testTeam(name, quota string) *tauv1alpha1.TauTeam {
	return &tauv1alpha1.TauTeam{
		TypeMeta: metav1.TypeMeta{
			APIVersion: tauv1alpha1.GroupVersion.String(),
			Kind:       tauv1alpha1.KindTauTeam,
		},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: tauv1alpha1.SystemNamespace},
		Spec: tauv1alpha1.TauTeamSpec{
			Quota: []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", quota, "0", "0")},
		},
	}
}

func testGPUQuota(flavor, nominal, borrowing, lending string) tauv1alpha1.TauResourceQuota {
	borrowingQuantity := resource.MustParse(borrowing)
	lendingQuantity := resource.MustParse(lending)
	return tauv1alpha1.TauResourceQuota{
		Flavor:         flavor,
		Resource:       nvidiaGPUResourceName,
		NominalQuota:   resource.MustParse(nominal),
		BorrowingLimit: &borrowingQuantity,
		LendingLimit:   &lendingQuantity,
	}
}

func testWorkspaceClusterQueue(workspace *tauv1alpha1.TauWorkspace) *unstructured.Unstructured {
	queue := newQueueObject(clusterQueueGVK)
	queue.SetName(workspaceClusterQueueName(workspace.Name))
	queue.Object["spec"] = map[string]any{
		"cohortName":     teamCohortName(workspace.Spec.TeamRef.Name),
		"resourceGroups": quotaResourceGroups(workspace.Spec.Quota, true),
	}
	labels := workspaceLabels(workspace.Name)
	labels[labelTeam] = workspace.Spec.TeamRef.Name
	queue.SetLabels(labels)
	setOwnerUIDAnnotation(queue, workspace.UID)
	return queue
}

func quotaFromResourceGroups(
	t *testing.T,
	obj *unstructured.Unstructured,
	flavorName, resourceName, field string,
) string {
	t.Helper()
	groups, found, err := unstructured.NestedSlice(obj.Object, "spec", "resourceGroups")
	if err != nil || !found {
		t.Fatalf("%s %q resourceGroups: found=%v err=%v", obj.GetKind(), obj.GetName(), found, err)
	}
	for _, rawGroup := range groups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			continue
		}
		flavors, _, _ := unstructured.NestedSlice(group, "flavors")
		for _, rawFlavor := range flavors {
			flavor, ok := rawFlavor.(map[string]any)
			if !ok || flavor["name"] != flavorName {
				continue
			}
			resources, _, _ := unstructured.NestedSlice(flavor, "resources")
			for _, rawResource := range resources {
				resourceQuota, ok := rawResource.(map[string]any)
				if ok && resourceQuota["name"] == resourceName {
					return resourceQuota[field].(string)
				}
			}
		}
	}
	return ""
}
