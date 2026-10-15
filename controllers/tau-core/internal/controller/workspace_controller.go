// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	notReadyRequeue = 30 * time.Second
	readyRequeue    = 5 * time.Minute
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
			if err := r.migrateLegacyWorkspaceOwnership(ctx, &workspace); err != nil {
				return ctrl.Result{}, err
			}
			if err := r.cleanupWorkspaceAccess(ctx, &workspace); err != nil {
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
	if err := r.migrateLegacyWorkspaceOwnership(ctx, &workspace); err != nil {
		return ctrl.Result{}, err
	}

	if strings.TrimSpace(workspace.Spec.Queue) == "" {
		resolved, err := r.getDefaultWorkspaceQueue(ctx)
		if err != nil {
			return r.reportUnresolvedQueue(ctx, &workspace, err.Error())
		}
		workspace.Spec.Queue = resolved
	}
	return r.syncWorkspace(ctx, &workspace)
}

func (r *TauWorkspaceReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *TauWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tauv1alpha1.TauWorkspace{}).
		Complete(r)
}
