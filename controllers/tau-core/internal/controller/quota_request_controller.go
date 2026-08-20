// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type TauQuotaRequestReconciler struct {
	client.Client
	SystemNamespace string
}

// +kubebuilder:rbac:groups=tau.azure.com,resources=quotarequests,verbs=get;list;watch
// +kubebuilder:rbac:groups=tau.azure.com,resources=quotarequests/status,verbs=get;update;patch

func (r *TauQuotaRequestReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("tauquotarequest", req.NamespacedName.String())
	var request tauv1alpha1.TauQuotaRequest
	if err := r.Get(ctx, req.NamespacedName, &request); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if request.Namespace != systemNamespace(r.SystemNamespace) {
		logger.Info("ignoring quota request outside system namespace", "systemNamespace", systemNamespace(r.SystemNamespace))
		return ctrl.Result{}, nil
	}

	phase := tauv1alpha1.QuotaRequestPhasePendingApproval
	decision := "waiting for platform approval"
	conditions := []metav1.Condition{
		condition("Approved", metav1.ConditionFalse, "PendingApproval", decision, request.Generation),
	}
	if request.Annotations[annotationRejected] == "true" {
		phase = tauv1alpha1.QuotaRequestPhaseRejected
		decision = "request rejected by platform"
		conditions = []metav1.Condition{condition("Approved", metav1.ConditionFalse, "Rejected", decision, request.Generation)}
	} else if request.Annotations[annotationApproved] == "true" {
		phase = tauv1alpha1.QuotaRequestPhaseApproved
		decision = "approved in ReportOnly mode; waiting for GitOps quota update"
		conditions = []metav1.Condition{
			condition("Approved", metav1.ConditionTrue, "Approved", "request approved by platform", request.Generation),
			condition("QuotaMutated", metav1.ConditionFalse, "ReportOnly", decision, request.Generation),
		}
	}

	desired := tauv1alpha1.TauQuotaRequestStatus{
		Phase:              phase,
		ObservedGeneration: request.Generation,
		Decision:           decision,
		ApprovedBy:         request.Annotations[annotationReviewedBy],
		Conditions:         mergeConditions(request.Status.Conditions, conditions),
	}
	if equalQuotaRequestStatus(request.Status, desired) {
		return ctrl.Result{}, nil
	}
	request.Status = desired
	if err := r.Status().Update(ctx, &request); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func (r *TauQuotaRequestReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tauv1alpha1.TauQuotaRequest{}).
		Complete(r)
}
