// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"fmt"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	localQueueGVK            = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "LocalQueue"}
	clusterQueueGVK          = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "ClusterQueue"}
	admissionCheckGVK        = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "AdmissionCheck"}
	resourceFlavorGVK        = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "ResourceFlavor"}
	topologyGVK              = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "Topology"}
	workloadPriorityClassGVK = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "WorkloadPriorityClass"}
)

// getDefaultWorkspaceQueue reads the cluster-wide workspace queue default from the
// TauCluster singleton. It is the same name the TauGrid distribution gives its
// baseline ClusterQueue, so a workspace that omits spec.queue still lands on a
// reviewed queue instead of guessing.
func (r *TauWorkspaceReconciler) getDefaultWorkspaceQueue(ctx context.Context) (string, error) {
	var cluster tauv1alpha1.TauCluster
	if err := r.Get(ctx, client.ObjectKey{Name: tauv1alpha1.TauClusterSingletonName}, &cluster); err != nil {
		return "", fmt.Errorf("spec.queue is empty and the TauCluster default is unavailable: %w", err)
	}
	queue := strings.TrimSpace(cluster.Spec.WorkspaceDefaults.DefaultQueue)
	if queue == "" {
		return "", fmt.Errorf("spec.queue is empty and TauCluster %q declares no workspaceDefaults.defaultQueue", cluster.Name)
	}
	return queue, nil
}

// reportUnresolvedQueue keeps an unresolvable workspace visible and Degraded
// rather than reconciling namespace, RBAC, or identity against a guessed queue.
func (r *TauWorkspaceReconciler) reportUnresolvedQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, message string) (ctrl.Result, error) {
	conditions := []metav1.Condition{
		boolCondition(tauv1alpha1.ConditionRBACReady, false, "QueueUnresolved", message, workspace.Generation),
		boolCondition(tauv1alpha1.ConditionQueueReady, false, "QueueUnresolved", message, workspace.Generation),
		condition(tauv1alpha1.ConditionWorkloadIdentityReady, metav1.ConditionUnknown, "QueueUnresolved", message, workspace.Generation),
		boolCondition(tauv1alpha1.ConditionDriftDetected, false, "NoDrift", message, workspace.Generation),
	}
	desired := tauv1alpha1.TauWorkspaceStatus{
		Phase:              tauv1alpha1.WorkspacePhaseDegraded,
		ObservedGeneration: workspace.Generation,
		Target:             tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: resolvedNamespace(workspace)},
		Conditions:         mergeConditions(workspace.Status.Conditions, conditions),
	}
	if !equalWorkspaceStatus(workspace.Status, desired) {
		workspace.Status = desired
		if err := r.Status().Update(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
}

// reconcileQueue accepts an existing platform-owned LocalQueue or creates the
// workspace LocalQueue when a same-named ClusterQueue exists. The latter is the
// portable TauGrid bootstrap contract: Helm owns the ClusterQueue, while this
// controller owns state that depends on a future workspace namespace.
func (r *TauWorkspaceReconciler) reconcileQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (tauv1alpha1.WorkspaceQueueStatus, bool, string) {
	localQueue := newQueueObject(localQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: workspace.Spec.Queue, Namespace: targetNamespace}, localQueue); err != nil {
		if !apierrors.IsNotFound(err) {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}, false, err.Error()
		}
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: workspace.Spec.Queue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: workspace.Spec.Queue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", workspace.Spec.Queue, err)
		}
		localQueue = newWorkspaceLocalQueue(workspace, targetNamespace)
		if err := r.Create(ctx, localQueue); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: workspace.Spec.Queue}, false,
					"workspace LocalQueue changed concurrently; retrying"
			}
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: workspace.Spec.Queue}, false,
				fmt.Sprintf("failed to reconcile workspace LocalQueue: %v", err)
		}
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: workspace.Spec.Queue}, true,
			"workspace LocalQueue is reconciled"
	}
	clusterQueueName, _, _ := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	labels := localQueue.GetLabels()
	if labels[labelManagedBy] == labelManagedByValue && labels[labelWorkspace] != workspace.Name {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("LocalQueue %q is owned by TauWorkspace %q", workspace.Spec.Queue, labels[labelWorkspace])
	}
	if ownedByWorkspace(labels, workspace.Name) {
		desiredClusterQueue := workspace.Spec.Queue
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: desiredClusterQueue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", desiredClusterQueue, err)
		}
		if clusterQueueName != desiredClusterQueue {
			if err := unstructured.SetNestedField(localQueue.Object, desiredClusterQueue, "spec", "clusterQueue"); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to restore workspace LocalQueue: %v", err)
			}
			if err := r.Update(ctx, localQueue); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to restore workspace LocalQueue: %v", err)
			}
			clusterQueueName = desiredClusterQueue
		}
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, true,
			"workspace LocalQueue is reconciled"
	}
	if strings.TrimSpace(clusterQueueName) == "" {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}, false,
			fmt.Sprintf("LocalQueue %q does not reference a ClusterQueue", workspace.Spec.Queue)
	}
	clusterQueue := newQueueObject(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: clusterQueueName}, clusterQueue); err != nil {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("backing ClusterQueue %q is not ready: %v", clusterQueueName, err)
	}
	return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, true,
		"workspace queue and backing ClusterQueue are readable"
}

func newQueueObject(gvk schema.GroupVersionKind) *unstructured.Unstructured {
	queue := &unstructured.Unstructured{}
	queue.SetGroupVersionKind(gvk)
	return queue
}

func newWorkspaceLocalQueue(workspace *tauv1alpha1.TauWorkspace, targetNamespace string) *unstructured.Unstructured {
	queue := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": localQueueGVK.GroupVersion().String(),
		"kind":       localQueueGVK.Kind,
		"metadata": map[string]any{
			"name":      workspace.Spec.Queue,
			"namespace": targetNamespace,
		},
		"spec": map[string]any{"clusterQueue": workspace.Spec.Queue},
	}}
	queue.SetGroupVersionKind(localQueueGVK)
	queue.SetLabels(workspaceLabels(workspace.Name))
	return queue
}
