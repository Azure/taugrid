// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// notReadyRequeue bounds recovery time while workspace dependencies are
	// unavailable.
	notReadyRequeue = 30 * time.Second
	// readyRequeue lets the controller repair a deleted workspace LocalQueue
	// without introducing a hard startup dependency on the Kueue CRDs.
	readyRequeue = 5 * time.Minute
)

type TauWorkspaceReconciler struct {
	client.Client
	APIReader       client.Reader
	SystemNamespace string
}

// +kubebuilder:rbac:groups=tau.azure.com,resources=workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=tau.azure.com,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=tau.azure.com,resources=workspaces,verbs=update;patch
// +kubebuilder:rbac:groups=tau.azure.com,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=localqueues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues,verbs=get
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=get;list;watch

func (r *TauWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("tauworkspace", req.NamespacedName.String())

	var workspace tauv1alpha1.TauWorkspace
	if err := r.Get(ctx, req.NamespacedName, &workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if workspace.Namespace != systemNamespace(r.SystemNamespace) {
		logger.Info("ignoring workspace outside system namespace", "systemNamespace", systemNamespace(r.SystemNamespace))
		return ctrl.Result{}, nil
	}
	if !workspace.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&workspace, workspaceFinalizer) {
			if err := r.cleanupWorkspaceAccess(ctx, workspace.Name); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&workspace, workspaceFinalizer)
			if err := r.Update(ctx, &workspace); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&workspace, workspaceFinalizer) {
		controllerutil.AddFinalizer(&workspace, workspaceFinalizer)
		if err := r.Update(ctx, &workspace); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	primaryWorkspace, err := r.resolvePrimaryWorkspace(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve v0 primary workspace: %w", err)
	}
	if primaryWorkspace != workspace.Name {
		if err := r.cleanupWorkspaceAccess(ctx, workspace.Name); err != nil {
			return ctrl.Result{}, err
		}
		message := fmt.Sprintf(
			"TauGrid v0 activates one workspace; %q is primary and %q is blocked",
			primaryWorkspace,
			workspace.Name,
		)
		conditions := []metav1.Condition{
			condition(tauv1alpha1.ConditionRBACReady, metav1.ConditionFalse, reasonAdditionalWorkspaceBlocked, message, workspace.Generation),
			condition(tauv1alpha1.ConditionQueueReady, metav1.ConditionFalse, reasonAdditionalWorkspaceBlocked, message, workspace.Generation),
			condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionFalse, "NoDrift", "No owned resource drift detected", workspace.Generation),
		}
		desiredStatus := tauv1alpha1.TauWorkspaceStatus{
			Phase:              workspacePhase(conditions),
			ObservedGeneration: workspace.Generation,
			Target: tauv1alpha1.WorkspaceTargetStatus{
				ResolvedNamespace: resolvedNamespace(&workspace),
			},
			Conditions: mergeConditions(workspace.Status.Conditions, conditions),
		}
		if !equalWorkspaceStatus(workspace.Status, desiredStatus) {
			workspace.Status = desiredStatus
			if err := r.Status().Update(ctx, &workspace); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
	}
	if workspace.Annotations[annotationV0Primary] != "true" {
		before := workspace.DeepCopy()
		if workspace.Annotations == nil {
			workspace.Annotations = map[string]string{}
		}
		workspace.Annotations[annotationV0Primary] = "true"
		if err := r.Patch(ctx, &workspace, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, fmt.Errorf("persist v0 primary workspace marker: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	// spec.queue is optional so a platform can declare the queue name once on
	// the TauCluster singleton. Resolve it in memory before anything reads it;
	// status.queue.localQueue then reports the effective queue to the CLI.
	if strings.TrimSpace(workspace.Spec.Queue) == "" {
		resolved, err := r.getDefaultWorkspaceQueue(ctx)
		if err != nil {
			return r.reportUnresolvedQueue(ctx, &workspace, err.Error())
		}
		workspace.Spec.Queue = resolved
	}
	return r.syncWorkspace(ctx, &workspace)
}

func (r *TauWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tauv1alpha1.TauWorkspace{}).
		Complete(r)
}
