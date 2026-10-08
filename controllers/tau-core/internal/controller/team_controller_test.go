// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTeamAndWorkspacesReconcileQuotaHierarchy(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")

	team := testTeam("vision", "16")
	training := testWorkspace("training")
	training.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	training.Spec.Queue = "default"
	training.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "2", "0")}
	evaluation := testWorkspace("evaluation")
	evaluation.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	evaluation.Spec.Queue = "default"
	evaluation.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "6", "0", "0")}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(flavor, team, training, evaluation).
		WithStatusSubresource(&tauv1alpha1.TauTeam{}, &tauv1alpha1.TauWorkspace{}).
		Build()
	teamReconciler := &TauTeamReconciler{Client: c}
	teamRequest := ctrl.Request{NamespacedName: types.NamespacedName{
		Name: team.Name, Namespace: tauv1alpha1.SystemNamespace,
	}}
	for i := 0; i < 2; i++ {
		if _, err := teamReconciler.Reconcile(ctx, teamRequest); err != nil {
			t.Fatalf("team reconcile iteration %d: %v", i, err)
		}
	}

	cohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, cohort); err != nil {
		t.Fatalf("Get team Cohort: %v", err)
	}
	if got := quotaFromResourceGroups(t, cohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "6" {
		t.Fatalf("team shared nominal quota = %q, want 6", got)
	}

	for _, workspaceName := range []string{training.Name, evaluation.Name} {
		reconciler := newTestWorkspaceReconciler(c)
		req := ctrl.Request{NamespacedName: types.NamespacedName{
			Name: workspaceName, Namespace: tauv1alpha1.SystemNamespace,
		}}
		for i := 0; i < 2; i++ {
			if _, err := reconciler.Reconcile(ctx, req); err != nil {
				t.Fatalf("workspace %s reconcile iteration %d: %v", workspaceName, i, err)
			}
		}
		var workspace tauv1alpha1.TauWorkspace
		if err := c.Get(ctx, req.NamespacedName, &workspace); err != nil {
			t.Fatalf("Get workspace %s: %v", workspaceName, err)
		}
		if workspace.Status.Phase != tauv1alpha1.WorkspacePhaseReady {
			t.Fatalf("workspace %s phase = %q, conditions=%#v", workspaceName, workspace.Status.Phase, workspace.Status.Conditions)
		}
		wantClusterQueue := workspaceClusterQueueName(workspaceName)
		if workspace.Status.Queue.ClusterQueue != wantClusterQueue {
			t.Fatalf("workspace %s ClusterQueue = %q, want %q", workspaceName, workspace.Status.Queue.ClusterQueue, wantClusterQueue)
		}
		queue := newQueueObject(clusterQueueGVK)
		if err := c.Get(ctx, client.ObjectKey{Name: wantClusterQueue}, queue); err != nil {
			t.Fatalf("Get workspace %s ClusterQueue: %v", workspaceName, err)
		}
		if got, _, _ := unstructured.NestedString(queue.Object, "spec", "cohortName"); got != teamCohortName(team.Name) {
			t.Fatalf("workspace %s cohortName = %q", workspaceName, got)
		}
	}

	trainingQueue := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: workspaceClusterQueueName(training.Name)}, trainingQueue); err != nil {
		t.Fatalf("Get training ClusterQueue: %v", err)
	}
	if got := quotaFromResourceGroups(t, trainingQueue, "taugrid-gpu-h200", nvidiaGPUResourceName, "borrowingLimit"); got != "2" {
		t.Fatalf("training borrowingLimit = %q, want 2", got)
	}
	if got := quotaFromResourceGroups(t, trainingQueue, "taugrid-gpu-h200", nvidiaGPUResourceName, "lendingLimit"); got != "0" {
		t.Fatalf("training lendingLimit = %q, want 0", got)
	}
}

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

func TestWorkspaceQuotaReductionBlocksBelowActiveReservation(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")}
	existing := desiredWorkspaceClusterQueue(workspace)
	if err := unstructured.SetNestedSlice(existing.Object, []any{
		map[string]any{
			"name": "taugrid-gpu-h200",
			"resources": []any{
				map[string]any{"name": nvidiaGPUResourceName, "total": "6"},
			},
		},
	}, "status", "flavorsReservation"); err != nil {
		t.Fatalf("set reservation status: %v", err)
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()

	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	err := reconcileManagedUnstructured(
		ctx,
		c,
		desiredWorkspaceClusterQueue(workspace),
		labelWorkspace,
		workspace.Name,
	)
	if err == nil || !strings.Contains(err.Error(), "below active reservation") {
		t.Fatalf("reconcile error = %v, want active reservation refusal", err)
	}
	got := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: existing.GetName()}, got); err != nil {
		t.Fatalf("Get ClusterQueue: %v", err)
	}
	if nominal := quotaFromResourceGroups(t, got, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); nominal != "8" {
		t.Fatalf("ClusterQueue nominal quota = %q, want unchanged 8", nominal)
	}
}

func TestBlockedWorkspaceQuotaReductionDoesNotReleaseTeamQuota(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	team := testTeam("vision", "16")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	existing := desiredWorkspaceClusterQueue(workspace)
	setClusterQueueNominalQuota(t, existing, "8")
	if err := unstructured.SetNestedSlice(existing.Object, []any{
		map[string]any{
			"name": "taugrid-gpu-h200",
			"resources": []any{
				map[string]any{"name": nvidiaGPUResourceName, "total": "6"},
			},
		},
	}, "status", "flavorsReservation"); err != nil {
		t.Fatalf("set reservation status: %v", err)
	}
	cohortQuota := []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")}
	cohort := desiredTeamCohort(team, cohortQuota)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(flavor, team, workspace, existing, cohort).
		Build()
	reconciler := &TauTeamReconciler{Client: c}
	shared, err := reconciler.sharedTeamQuota(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	if got := shared[0].NominalQuota.String(); got != "8" {
		t.Fatalf("shared team quota before blocked reduction = %q, want 8", got)
	}
	_, err = newTestWorkspaceReconciler(c).reconcileWorkspaceClusterQueue(ctx, workspace)
	if err == nil || !strings.Contains(err.Error(), "below active reservation") {
		t.Fatalf("reconcile error = %v, want active reservation refusal", err)
	}
	gotCohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: cohort.GetName()}, gotCohort); err != nil {
		t.Fatalf("Get Cohort: %v", err)
	}
	if got := quotaFromResourceGroups(t, gotCohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "8" {
		t.Fatalf("shared Cohort quota after blocked reduction = %q, want 8", got)
	}
	shared, err = reconciler.sharedTeamQuota(ctx, team)
	if err != nil {
		t.Fatal(err)
	}
	if got := shared[0].NominalQuota.String(); got != "8" {
		t.Fatalf("shared team quota after blocked reduction = %q, want 8", got)
	}
}

func TestSuccessfulWorkspaceQuotaReductionReleasesTeamQuotaAfterQueueUpdate(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	team := testTeam("vision", "16")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseReady
	team.Status.ObservedGeneration = team.Generation
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	existing := desiredWorkspaceClusterQueue(workspace)
	setClusterQueueNominalQuota(t, existing, "8")
	cohort := desiredTeamCohort(
		team,
		[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "8", "0", "0")},
	)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(flavor, team, workspace, existing, cohort).
		Build()
	if _, err := newTestWorkspaceReconciler(c).reconcileWorkspaceClusterQueue(ctx, workspace); err != nil {
		t.Fatal(err)
	}

	gotQueue := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: existing.GetName()}, gotQueue); err != nil {
		t.Fatalf("Get ClusterQueue: %v", err)
	}
	if got := quotaFromResourceGroups(t, gotQueue, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "4" {
		t.Fatalf("workspace quota after reduction = %q, want 4", got)
	}
	gotCohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: cohort.GetName()}, gotCohort); err != nil {
		t.Fatalf("Get Cohort: %v", err)
	}
	if got := quotaFromResourceGroups(t, gotCohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "12" {
		t.Fatalf("shared Cohort quota after successful reduction = %q, want 12", got)
	}
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
	queue := desiredWorkspaceClusterQueue(workspace)
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

func TestUnreadyTeamAllowsWorkspaceQuotaReductionToConverge(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	flavor := newQueueObject(resourceFlavorGVK)
	flavor.SetName("taugrid-gpu-h200")
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")
	team.Status.Phase = tauv1alpha1.TeamPhaseDegraded
	team.Status.ObservedGeneration = team.Generation
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: team.Name}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	queue := desiredWorkspaceClusterQueue(workspace)
	setClusterQueueNominalQuota(t, queue, "12")
	cohort := desiredTeamCohort(
		team,
		[]tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "0", "0", "0")},
	)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(flavor, team, workspace, queue, cohort).
		Build()
	if _, err := newTestWorkspaceReconciler(c).reconcileWorkspaceClusterQueue(ctx, workspace); err != nil {
		t.Fatal(err)
	}
	gotQueue := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: queue.GetName()}, gotQueue); err != nil {
		t.Fatalf("Get ClusterQueue: %v", err)
	}
	if got := quotaFromResourceGroups(t, gotQueue, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "4" {
		t.Fatalf("workspace quota after convergence = %q, want 4", got)
	}
	gotCohort := newQueueObject(cohortGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: cohort.GetName()}, gotCohort); err != nil {
		t.Fatalf("Get Cohort: %v", err)
	}
	if got := quotaFromResourceGroups(t, gotCohort, "taugrid-gpu-h200", nvidiaGPUResourceName, "nominalQuota"); got != "4" {
		t.Fatalf("shared quota after convergence = %q, want 4", got)
	}
}

func setClusterQueueNominalQuota(t *testing.T, queue *unstructured.Unstructured, value string) {
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
	resourceQuota["nominalQuota"] = value
	if err := unstructured.SetNestedSlice(queue.Object, groups, "spec", "resourceGroups"); err != nil {
		t.Fatalf("write ClusterQueue resourceGroups: %v", err)
	}
}

func TestWorkspaceClusterQueueRejectsDifferentOwnerUID(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme(t)
	workspace := testWorkspace("training")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	workspace.UID = types.UID("old-workspace-uid")
	existing := desiredWorkspaceClusterQueue(workspace)
	workspace.UID = types.UID("new-workspace-uid")

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	err := reconcileManagedUnstructured(
		ctx,
		c,
		desiredWorkspaceClusterQueue(workspace),
		labelWorkspace,
		workspace.Name,
	)
	if err == nil || !strings.Contains(err.Error(), "different owner UID") {
		t.Fatalf("reconcile error = %v, want owner UID refusal", err)
	}
}

func TestManagedQuotaObjectsRejectUnannotatedLegacyOwnership(t *testing.T) {
	scheme := testScheme(t)
	workspace := testWorkspace("training")
	workspace.UID = types.UID("workspace-uid")
	workspace.Spec.TeamRef = &tauv1alpha1.TauClusterObjectReference{Name: "vision"}
	workspace.Spec.Quota = []tauv1alpha1.TauResourceQuota{testGPUQuota("taugrid-gpu-h200", "4", "0", "0")}
	team := testTeam("vision", "8")
	team.UID = types.UID("team-uid")

	tests := []struct {
		name       string
		desired    *unstructured.Unstructured
		ownerLabel string
		ownerName  string
	}{
		{
			name:       "ClusterQueue",
			desired:    desiredWorkspaceClusterQueue(workspace),
			ownerLabel: labelWorkspace,
			ownerName:  workspace.Name,
		},
		{
			name:       "Cohort",
			desired:    desiredTeamCohort(team, team.Spec.Quota),
			ownerLabel: labelTeam,
			ownerName:  team.Name,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := tt.desired.DeepCopy()
			existing.SetAnnotations(nil)
			existing.SetUID(types.UID("legacy-object-uid"))
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
			err := reconcileManagedUnstructured(context.Background(), c, tt.desired, tt.ownerLabel, tt.ownerName)
			if err == nil || !strings.Contains(err.Error(), "legacy ownership metadata") {
				t.Fatalf("reconcile error = %v, want legacy ownership refusal", err)
			}
		})
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
