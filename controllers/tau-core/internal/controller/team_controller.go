// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type TauTeamReconciler struct {
	client.Client
	SystemNamespace string
}

// +kubebuilder:rbac:groups=tau.azure.com,resources=teams,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=tau.azure.com,resources=teams/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=tau.azure.com,resources=workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=cohorts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=resourceflavors,verbs=get

func (r *TauTeamReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var team tauv1alpha1.TauTeam
	if err := r.Get(ctx, req.NamespacedName, &team); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if team.Namespace != systemNamespace(r.SystemNamespace) {
		return ctrl.Result{}, nil
	}
	if !team.DeletionTimestamp.IsZero() {
		return r.finalizeTeam(ctx, &team)
	}
	if !controllerutil.ContainsFinalizer(&team, teamFinalizer) {
		controllerutil.AddFinalizer(&team, teamFinalizer)
		if err := r.Update(ctx, &team); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if err := r.validateTeamCapacity(ctx, &team); err != nil {
		return r.reportTeamStatus(ctx, &team, false, "CapacityExceeded", err.Error())
	}
	sharedQuota, err := r.sharedTeamQuota(ctx, &team)
	if err != nil {
		return r.reportTeamStatus(ctx, &team, false, "QuotaInvalid", err.Error())
	}
	if err := validateQuotaFlavors(ctx, r.Client, team.Spec.Quota); err != nil {
		return r.reportTeamStatus(ctx, &team, false, "FlavorNotReady", err.Error())
	}
	if err := reconcileManagedUnstructured(ctx, r.Client, desiredTeamCohort(&team, sharedQuota), labelTeam, team.Name); err != nil {
		return r.reportTeamStatus(ctx, &team, false, "CohortReconcileFailed", err.Error())
	}
	return r.reportTeamStatus(ctx, &team, true, "QuotaReady", "team quota hierarchy is reconciled")
}

func (r *TauTeamReconciler) validateTeamCapacity(ctx context.Context, team *tauv1alpha1.TauTeam) error {
	var cluster tauv1alpha1.TauCluster
	if err := r.Get(ctx, client.ObjectKey{Name: tauv1alpha1.TauClusterSingletonName}, &cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	capacity := make(map[string]resource.Quantity, len(cluster.Status.DiscoveredCapacity))
	for _, entry := range cluster.Status.DiscoveredCapacity {
		capacity[entry.Resource+"\x00"+entry.Flavor] = entry.Capacity.DeepCopy()
	}
	var teams tauv1alpha1.TauTeamList
	if err := r.List(ctx, &teams, client.InNamespace(team.Namespace)); err != nil {
		return err
	}
	allocated := map[string]resource.Quantity{}
	for i := range teams.Items {
		for _, quota := range teams.Items[i].Spec.Quota {
			key := quotaKey(quota)
			total := allocated[key]
			total.Add(quota.NominalQuota)
			allocated[key] = total
		}
	}
	for key, total := range allocated {
		available, discovered := capacity[key]
		if discovered && total.Cmp(available) > 0 {
			return fmt.Errorf("team allocations %s exceed discovered capacity %s for %q", total.String(), available.String(), key)
		}
	}
	return nil
}

func (r *TauTeamReconciler) sharedTeamQuota(ctx context.Context, team *tauv1alpha1.TauTeam) ([]tauv1alpha1.TauResourceQuota, error) {
	remaining := make(map[string]tauv1alpha1.TauResourceQuota, len(team.Spec.Quota))
	for _, quota := range team.Spec.Quota {
		if quota.BorrowingLimit != nil && quota.BorrowingLimit.Sign() != 0 {
			return nil, fmt.Errorf("team root quota for flavor %q resource %q cannot set borrowingLimit", quota.Flavor, quota.Resource)
		}
		if quota.LendingLimit != nil && quota.LendingLimit.Sign() != 0 {
			return nil, fmt.Errorf("team root quota for flavor %q resource %q cannot set lendingLimit", quota.Flavor, quota.Resource)
		}
		remaining[quotaKey(quota)] = tauv1alpha1.TauResourceQuota{
			Flavor:       quota.Flavor,
			Resource:     quota.Resource,
			NominalQuota: quota.NominalQuota.DeepCopy(),
		}
	}
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := r.List(ctx, &workspaces, client.InNamespace(team.Namespace)); err != nil {
		return nil, err
	}
	for i := range workspaces.Items {
		workspace := &workspaces.Items[i]
		if workspace.Spec.TeamRef == nil || workspace.Spec.TeamRef.Name != team.Name {
			continue
		}
		for _, quota := range workspace.Spec.Quota {
			key := quotaKey(quota)
			available, ok := remaining[key]
			if !ok {
				return nil, fmt.Errorf("workspace %q requests flavor %q resource %q outside team quota", workspace.Name, quota.Flavor, quota.Resource)
			}
			available.NominalQuota.Sub(quota.NominalQuota)
			if available.NominalQuota.Sign() < 0 {
				return nil, fmt.Errorf("workspace guarantees exceed team quota for flavor %q resource %q", quota.Flavor, quota.Resource)
			}
			remaining[key] = available
		}
	}
	out := make([]tauv1alpha1.TauResourceQuota, 0, len(remaining))
	for _, quota := range remaining {
		out = append(out, quota)
	}
	return out, nil
}

func (r *TauTeamReconciler) finalizeTeam(ctx context.Context, team *tauv1alpha1.TauTeam) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(team, teamFinalizer) {
		return ctrl.Result{}, nil
	}
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := r.List(ctx, &workspaces, client.InNamespace(team.Namespace)); err != nil {
		return ctrl.Result{}, err
	}
	for i := range workspaces.Items {
		workspace := &workspaces.Items[i]
		if workspace.Spec.TeamRef != nil && workspace.Spec.TeamRef.Name == team.Name {
			return r.reportTeamStatus(ctx, team, false, tauv1alpha1.ConditionDeletionBlocked,
				fmt.Sprintf("workspace %q still references this team", workspace.Name))
		}
	}
	cohort := newQueueObject(cohortGVK)
	cohort.SetName(teamCohortName(team.Name))
	if err := r.Get(ctx, client.ObjectKeyFromObject(cohort), cohort); err == nil {
		if cohort.GetLabels()[labelManagedBy] != labelManagedByValue || cohort.GetLabels()[labelTeam] != team.Name {
			return ctrl.Result{}, fmt.Errorf("refusing to delete Cohort %q: ownership does not match team %q", cohort.GetName(), team.Name)
		}
		if ownerUID := cohort.GetAnnotations()[annotationOwnerUID]; ownerUID != "" && ownerUID != string(team.UID) {
			return ctrl.Result{}, fmt.Errorf("refusing to delete Cohort %q: owner UID is %q, not %q", cohort.GetName(), ownerUID, team.UID)
		}
		if cohort.GetAnnotations()[annotationOwnerUID] == "" && cohort.GetUID() != "" && team.UID != "" {
			return ctrl.Result{}, fmt.Errorf("refusing to delete Cohort %q without authoritative owner UID", cohort.GetName())
		}
		if expectedUID := team.Status.CohortUID; expectedUID != "" && string(cohort.GetUID()) != expectedUID {
			return ctrl.Result{}, fmt.Errorf("refusing to delete Cohort %q: UID changed from %q to %q", cohort.GetName(), expectedUID, cohort.GetUID())
		}
		if err := r.Delete(ctx, cohort); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
	} else if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	controllerutil.RemoveFinalizer(team, teamFinalizer)
	return ctrl.Result{}, r.Update(ctx, team)
}

func (r *TauTeamReconciler) reportTeamStatus(
	ctx context.Context,
	team *tauv1alpha1.TauTeam,
	ready bool,
	reason, message string,
) (ctrl.Result, error) {
	conditions := mergeConditions(team.Status.Conditions, []metav1.Condition{
		boolCondition(tauv1alpha1.ConditionQuotaReady, ready, reason, message, team.Generation),
	})
	phase := tauv1alpha1.TeamPhaseReady
	requeue := 5 * time.Minute
	if !ready {
		phase = tauv1alpha1.TeamPhaseDegraded
		requeue = notReadyRequeue
	}
	cohortUID := team.Status.CohortUID
	if ready {
		cohort := newQueueObject(cohortGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: teamCohortName(team.Name)}, cohort); err != nil {
			return ctrl.Result{}, err
		}
		cohortUID = string(cohort.GetUID())
	}
	desired := tauv1alpha1.TauTeamStatus{
		Phase:              phase,
		ObservedGeneration: team.Generation,
		Cohort:             teamCohortName(team.Name),
		CohortUID:          cohortUID,
		Conditions:         conditions,
	}
	if !apiequality.Semantic.DeepEqual(team.Status, desired) {
		team.Status = desired
		if err := r.Status().Update(ctx, team); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: requeue}, nil
}

func (r *TauTeamReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tauv1alpha1.TauTeam{}).
		Watches(&tauv1alpha1.TauCluster{}, handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
			if obj.GetName() != tauv1alpha1.TauClusterSingletonName {
				return nil
			}
			var teams tauv1alpha1.TauTeamList
			if err := r.List(ctx, &teams, client.InNamespace(systemNamespace(r.SystemNamespace))); err != nil {
				return nil
			}
			requests := make([]reconcile.Request, 0, len(teams.Items))
			for i := range teams.Items {
				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(&teams.Items[i]),
				})
			}
			return requests
		})).
		Watches(&tauv1alpha1.TauWorkspace{}, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
			workspace, ok := obj.(*tauv1alpha1.TauWorkspace)
			if !ok || workspace.Spec.TeamRef == nil {
				return nil
			}
			return []reconcile.Request{{NamespacedName: client.ObjectKey{
				Name:      workspace.Spec.TeamRef.Name,
				Namespace: workspace.Namespace,
			}}}
		})).
		Complete(r)
}
