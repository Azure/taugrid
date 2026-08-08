// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strings"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	azureVMSizeLabel = "node.kubernetes.io/instance-type"
	nodeResyncPeriod = 10 * time.Minute
)

type TauClusterReconciler struct {
	client.Client
}

type nodeReconcileState struct {
	status               tauv1alpha1.TauClusterSectionStatus
	nodesCondition       metav1.Condition
	driftCondition       metav1.Condition
	ownershipCondition   metav1.Condition
	reconciliationFailed bool
}

// +kubebuilder:rbac:groups=tau.azure.com,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=tau.azure.com,resources=clusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;patch

func (r *TauClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var cluster tauv1alpha1.TauCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	mode := cluster.Spec.ManagementMode
	if mode == "" {
		mode = tauv1alpha1.ClusterManagementModeObserve
	}

	var (
		nodeState nodeReconcileState
		nodeErr   error
	)
	if cluster.Name == tauv1alpha1.TauClusterSingletonName {
		nodeState, nodeErr = r.reconcileNodeLabels(ctx, &cluster, mode == tauv1alpha1.ClusterManagementModeReconcile)
	}

	desired := tauClusterStatus(&cluster, mode, nodeState)
	result := ctrl.Result{RequeueAfter: nodeResyncPeriod}
	if equalTauClusterStatus(cluster.Status, desired) {
		return result, nodeErr
	}

	cluster.Status = desired
	if err := r.Status().Update(ctx, &cluster); err != nil {
		return ctrl.Result{}, errors.Join(nodeErr, err)
	}
	return result, nodeErr
}

func (r *TauClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tauv1alpha1.TauCluster{}).
		Watches(
			&corev1.Node{},
			handler.EnqueueRequestsFromMapFunc(enqueueTauClusterForNode),
			builder.WithPredicates(nodeLabelChangePredicate()),
		).
		Complete(r)
}

func enqueueTauClusterForNode(context.Context, client.Object) []reconcile.Request {
	return []reconcile.Request{{
		NamespacedName: types.NamespacedName{Name: tauv1alpha1.TauClusterSingletonName},
	}}
}

func nodeLabelChangePredicate() predicate.Predicate {
	return predicate.Funcs{
		CreateFunc: func(event.CreateEvent) bool {
			return true
		},
		DeleteFunc: func(event.DeleteEvent) bool {
			return true
		},
		UpdateFunc: func(update event.UpdateEvent) bool {
			return !maps.Equal(update.ObjectOld.GetLabels(), update.ObjectNew.GetLabels())
		},
		GenericFunc: func(event.GenericEvent) bool {
			return false
		},
	}
}

func (r *TauClusterReconciler) reconcileNodeLabels(
	ctx context.Context,
	cluster *tauv1alpha1.TauCluster,
	mutate bool,
) (nodeReconcileState, error) {
	generation := cluster.Generation
	rules := cluster.Spec.Nodes.LabelRules
	if len(rules) == 0 {
		return nodeReconcileState{
			nodesCondition:     condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionTrue, "NoNodeLabelRules", "no node topology labels are declared", generation),
			driftCondition:     condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionFalse, "NoNodeLabelDrift", "no node topology labels are declared", generation),
			ownershipCondition: condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no node topology label conflicts were found", generation),
		}, nil
	}

	if err := validateNodeLabelRules(rules); err != nil {
		message := err.Error()
		return nodeReconcileState{
			nodesCondition:       condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionFalse, "InvalidNodeLabelRules", message, generation),
			driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, "InvalidNodeLabelRules", message, generation),
			ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no node topology label conflicts were evaluated", generation),
			reconciliationFailed: true,
		}, nil
	}
	if err := validateNodeLabelRuleConflicts(rules); err != nil {
		message := "conflicting node label rules: " + err.Error()
		return nodeReconcileState{
			nodesCondition:       condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionFalse, "ConflictingNodeLabelRules", message, generation),
			driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "NodeLabelRuleConflict", message, generation),
			ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionTrue, "ConflictingNodeLabelRules", message, generation),
			reconciliationFailed: true,
		}, nil
	}

	var nodes corev1.NodeList
	listOptions := []client.ListOption{}
	if len(cluster.Spec.Nodes.Selector) > 0 {
		listOptions = append(listOptions, client.MatchingLabels(cluster.Spec.Nodes.Selector))
	}
	if err := r.List(ctx, &nodes, listOptions...); err != nil {
		message := fmt.Sprintf("list nodes: %v", err)
		return nodeReconcileState{
			nodesCondition:       condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionFalse, "NodeDiscoveryFailed", message, generation),
			driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, "NodeDiscoveryFailed", message, generation),
			ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no node topology label conflicts were evaluated", generation),
			reconciliationFailed: true,
		}, err
	}

	state := nodeReconcileState{}
	var reconcileErr error
	conflicts := make([]string, 0)
	for i := range nodes.Items {
		node := &nodes.Items[i]
		desiredLabels, matched, err := desiredNodeLabels(node, rules)
		if !matched {
			continue
		}
		state.status.Observed++
		if err != nil {
			state.status.Drifted++
			conflicts = append(conflicts, err.Error())
			continue
		}
		if nodeHasLabels(node, desiredLabels) {
			state.status.Ready++
			continue
		}
		if !mutate {
			state.status.Drifted++
			continue
		}

		before := node.DeepCopy()
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		for key, value := range desiredLabels {
			node.Labels[key] = value
		}
		if err := r.Patch(ctx, node, client.MergeFrom(before)); err != nil {
			state.status.Drifted++
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("patch node %q topology labels: %w", node.Name, err))
			continue
		}
		state.status.Ready++
	}

	if len(conflicts) > 0 {
		sort.Strings(conflicts)
		message := "conflicting node label rules: " + conflicts[0]
		state.nodesCondition = condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionFalse, "ConflictingNodeLabelRules", message, generation)
		state.driftCondition = condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "NodeLabelRuleConflict", message, generation)
		state.ownershipCondition = condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionTrue, "ConflictingNodeLabelRules", message, generation)
		state.reconciliationFailed = true
		return state, reconcileErr
	}

	state.ownershipCondition = condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no node topology label conflicts were found", generation)
	switch {
	case reconcileErr != nil:
		message := reconcileErr.Error()
		state.nodesCondition = condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionFalse, "NodeLabelReconcileFailed", message, generation)
		state.driftCondition = condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "NodeLabelDrift", message, generation)
		state.reconciliationFailed = true
	case state.status.Observed == 0:
		state.nodesCondition = condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionTrue, "NoMatchingNodes", "no nodes currently match the configured VM-size rules; no node labels need reconciliation", generation)
		state.driftCondition = condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionFalse, "NoNodeLabelDrift", "no matching nodes were discovered", generation)
	case state.status.Drifted > 0:
		message := fmt.Sprintf("%d of %d matching nodes need topology label reconciliation", state.status.Drifted, state.status.Observed)
		state.nodesCondition = condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionFalse, "NodeLabelDrift", message, generation)
		state.driftCondition = condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "NodeLabelDrift", message, generation)
	default:
		message := fmt.Sprintf("%d matching nodes have the configured topology labels", state.status.Ready)
		state.nodesCondition = condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionTrue, "NodeLabelsReady", message, generation)
		state.driftCondition = condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionFalse, "NoNodeLabelDrift", message, generation)
	}
	return state, reconcileErr
}

func validateNodeLabelRules(rules []tauv1alpha1.TauNodeLabelRule) error {
	for i, rule := range rules {
		if len(rule.Labels) == 0 {
			return fmt.Errorf("nodes.labelRules[%d].labels must not be empty", i)
		}
		keys := make([]string, 0, len(rule.Labels))
		for key := range rule.Labels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if problems := k8svalidation.IsQualifiedName(key); len(problems) > 0 {
				return fmt.Errorf("nodes.labelRules[%d].labels[%q]: %s", i, key, problems[0])
			}
			if strings.HasPrefix(key, "kubernetes.io/") || strings.HasPrefix(key, "node.kubernetes.io/") {
				return fmt.Errorf("nodes.labelRules[%d].labels[%q]: reserved Kubernetes label prefixes cannot be managed by TauCluster", i, key)
			}
			if problems := k8svalidation.IsValidLabelValue(rule.Labels[key]); len(problems) > 0 {
				return fmt.Errorf("nodes.labelRules[%d].labels[%q]: %s", i, key, problems[0])
			}
		}
	}
	return nil
}

func validateNodeLabelRuleConflicts(rules []tauv1alpha1.TauNodeLabelRule) error {
	for i := range rules {
		for j := i + 1; j < len(rules); j++ {
			if !vmSizeRulesOverlap(rules[i].Match.VMSizes, rules[j].Match.VMSizes) {
				continue
			}
			keys := make([]string, 0, len(rules[i].Labels))
			for key := range rules[i].Labels {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			for _, key := range keys {
				if right, ok := rules[j].Labels[key]; ok && right != rules[i].Labels[key] {
					return fmt.Errorf("rules %d and %d assign different values for %q (%q and %q)", i, j, key, rules[i].Labels[key], right)
				}
			}
		}
	}
	return nil
}

func vmSizeRulesOverlap(left, right []string) bool {
	if len(left) == 0 || len(right) == 0 {
		return true
	}
	for _, vmSize := range left {
		if slices.Contains(right, vmSize) {
			return true
		}
	}
	return false
}

func desiredNodeLabels(node *corev1.Node, rules []tauv1alpha1.TauNodeLabelRule) (map[string]string, bool, error) {
	vmSize := node.Labels[azureVMSizeLabel]
	desired := map[string]string{}
	matched := false
	for i, rule := range rules {
		if len(rule.Match.VMSizes) > 0 && !slices.Contains(rule.Match.VMSizes, vmSize) {
			continue
		}
		matched = true
		for key, value := range rule.Labels {
			if previous, ok := desired[key]; ok && previous != value {
				return nil, true, fmt.Errorf("node %q matches rules with different values for %q (%q and %q, including rule %d)", node.Name, key, previous, value, i)
			}
			desired[key] = value
		}
	}
	return desired, matched, nil
}

func nodeHasLabels(node *corev1.Node, desired map[string]string) bool {
	for key, value := range desired {
		if node.Labels[key] != value {
			return false
		}
	}
	return true
}

func tauClusterStatus(
	cluster *tauv1alpha1.TauCluster,
	mode string,
	nodes nodeReconcileState,
) tauv1alpha1.TauClusterStatus {
	generation := cluster.Generation
	if cluster.Name != tauv1alpha1.TauClusterSingletonName {
		conditions := []metav1.Condition{
			condition(tauv1alpha1.ConditionNodesReady, metav1.ConditionUnknown, "ObservationSkipped", "node discovery requires the TauCluster singleton named cluster", generation),
			condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionUnknown, "ObservationPending", "queue observation is not enabled", generation),
			condition(tauv1alpha1.ConditionWorkspacesReady, metav1.ConditionUnknown, "ObservationPending", "workspace aggregation is not enabled", generation),
			condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, "ObservationSkipped", "node discovery requires the TauCluster singleton named cluster", generation),
			condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no resource ownership conflict was observed", generation),
			condition(tauv1alpha1.ConditionDeletionBlocked, metav1.ConditionFalse, "NoDeletionBlock", "no managed resource blocks deletion", generation),
			condition(tauv1alpha1.ConditionObserveOnly, metav1.ConditionTrue, "InvalidSingletonName", "cluster resources are not mutated", generation),
			condition(tauv1alpha1.ConditionReconcilePaused, metav1.ConditionTrue, "InvalidSingletonName", "TauCluster must be named cluster", generation),
			condition(tauv1alpha1.ConditionReady, metav1.ConditionFalse, "InvalidSingletonName", "TauCluster must be named cluster", generation),
		}
		return tauv1alpha1.TauClusterStatus{
			Phase:              tauv1alpha1.ClusterPhaseDegraded,
			ObservedGeneration: generation,
			DesiredStateHash:   clusterSpecHash(cluster.Spec),
			ManagedResources:   []tauv1alpha1.TauManagedResourceStatus{},
			Conditions:         mergeConditions(cluster.Status.Conditions, conditions),
		}
	}

	conditions := []metav1.Condition{
		nodes.nodesCondition,
		condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionUnknown, "ObservationPending", "queue observation is not enabled", generation),
		condition(tauv1alpha1.ConditionWorkspacesReady, metav1.ConditionUnknown, "ObservationPending", "workspace aggregation is not enabled", generation),
		nodes.driftCondition,
		nodes.ownershipCondition,
		condition(tauv1alpha1.ConditionDeletionBlocked, metav1.ConditionFalse, "NoDeletionBlock", "node labels are retained when TauCluster is deleted", generation),
	}
	phase := tauv1alpha1.ClusterPhasePending
	if nodes.reconciliationFailed {
		phase = tauv1alpha1.ClusterPhaseDegraded
	}

	if mode == tauv1alpha1.ClusterManagementModeReconcile {
		// Queue and workspace aggregation are deliberately out of scope, so an
		// otherwise healthy singleton must still be able to report Ready. Node
		// reconciliation is the only thing this object owns today.
		nodesReady := nodes.nodesCondition.Status == metav1.ConditionTrue
		if !nodes.reconciliationFailed && nodesReady {
			phase = tauv1alpha1.ClusterPhaseReady
		}
		readyStatus := metav1.ConditionFalse
		readyReason := "PartialReconciliation"
		readyMessage := "node topology labels are not fully reconciled; queue ownership remains external"
		if phase == tauv1alpha1.ClusterPhaseReady {
			readyStatus = metav1.ConditionTrue
			readyReason = "NodeReconciliationReady"
			readyMessage = "node topology labels are reconciled; queue ownership remains external"
		}
		conditions = append(conditions,
			condition(tauv1alpha1.ConditionObserveOnly, metav1.ConditionFalse, "ReconcileMode", "node topology labels are reconciled", generation),
			condition(tauv1alpha1.ConditionReconcilePaused, metav1.ConditionFalse, "NodeReconciliationActive", "node topology label reconciliation is active; queue ownership remains external", generation),
			condition(tauv1alpha1.ConditionReady, readyStatus, readyReason, readyMessage, generation),
		)
	} else {
		conditions = append(conditions,
			condition(tauv1alpha1.ConditionObserveOnly, metav1.ConditionTrue, "ObserveMode", "node topology label drift is observed but not mutated", generation),
			condition(tauv1alpha1.ConditionReconcilePaused, metav1.ConditionFalse, "ObserveMode", "reconciliation is not requested", generation),
			condition(tauv1alpha1.ConditionReady, metav1.ConditionFalse, "ObservationPending", "queue observation is not enabled", generation),
		)
	}

	return tauv1alpha1.TauClusterStatus{
		Phase:              phase,
		ObservedGeneration: generation,
		DesiredStateHash:   clusterSpecHash(cluster.Spec),
		Nodes:              nodes.status,
		ManagedResources:   []tauv1alpha1.TauManagedResourceStatus{},
		Conditions:         mergeConditions(cluster.Status.Conditions, conditions),
	}
}

func clusterSpecHash(spec tauv1alpha1.TauClusterSpec) string {
	spec = normalizeClusterSpec(spec)
	data, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func normalizeClusterSpec(spec tauv1alpha1.TauClusterSpec) tauv1alpha1.TauClusterSpec {
	if spec.ManagementMode == "" {
		spec.ManagementMode = tauv1alpha1.ClusterManagementModeObserve
	}
	if spec.DeletionPolicy == "" {
		spec.DeletionPolicy = tauv1alpha1.ClusterDeletionPolicyRetain
	}
	if spec.Queues.Ownership == "" {
		spec.Queues.Ownership = tauv1alpha1.ClusterOwnershipExternal
	}
	if spec.WorkspaceDefaults.DefaultQueue == "" {
		spec.WorkspaceDefaults.DefaultQueue = "jobqueue"
	}
	return spec
}
