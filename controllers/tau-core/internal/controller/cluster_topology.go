// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func (r *TauClusterReconciler) reconcileGPUNodeTopology(
	ctx context.Context,
	cluster *tauv1alpha1.TauCluster,
	mutate bool,
) (topologyReconcileState, error) {
	generation := cluster.Generation
	topology := newQueueObject(topologyGVK)
	err := r.Get(ctx, client.ObjectKey{Name: tauGPUNodeTopologyName}, topology)
	topologyMissing := apierrors.IsNotFound(err)
	if err != nil && !topologyMissing {
		return topologyReconcileState{}, fmt.Errorf("get Topology %q: %w", tauGPUNodeTopologyName, err)
	}
	if !topologyMissing {
		if topology.GetLabels()[labelManagedBy] != labelManagedByValue {
			message := fmt.Sprintf("Topology %s exists but is not owned by %s", tauGPUNodeTopologyName, labelManagedByValue)
			return topologyReconcileState{
				status:               tauv1alpha1.TauClusterSectionStatus{Observed: 1, Drifted: 1},
				queuesCondition:      condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionFalse, "TopologyOwnershipConflict", message, generation),
				driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "TopologyDrift", message, generation),
				ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionTrue, "TopologyOwnershipConflict", message, generation),
				reconciliationFailed: true,
			}, nil
		}

		desired := desiredTauGPUTopology()
		currentSpec, _, _ := unstructured.NestedMap(topology.Object, "spec")
		desiredSpec, _, _ := unstructured.NestedMap(desired.Object, "spec")
		if !reflect.DeepEqual(currentSpec, desiredSpec) {
			message := fmt.Sprintf("Topology %s has drifted from the TauGrid topology contract", tauGPUNodeTopologyName)
			return topologyReconcileState{
				status:               tauv1alpha1.TauClusterSectionStatus{Observed: 1, Drifted: 1},
				queuesCondition:      condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionFalse, "ImmutableTopologyDrift", message, generation),
				driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "ImmutableTopologyDrift", message, generation),
				ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "the TauGrid-owned topology has immutable spec drift", generation),
				reconciliationFailed: true,
				managedResources:     managedTopologyStatus(topology),
			}, nil
		}
	}

	nodeStatus, nodeDrift, nodeErr := r.reconcileNodeTopologyLabels(ctx, mutate)

	if topologyMissing {
		if !mutate {
			return topologyReconcileState{
				status:             tauv1alpha1.TauClusterSectionStatus{Observed: 1, Drifted: 1},
				queuesCondition:    condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionFalse, "TopologyMissing", fmt.Sprintf("Topology %s does not exist", tauGPUNodeTopologyName), generation),
				driftCondition:     condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "TopologyDrift", fmt.Sprintf("Topology %s needs reconciliation", tauGPUNodeTopologyName), generation),
				ownershipCondition: condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no topology ownership conflict was found", generation),
			}, nil
		}
		topology = desiredTauGPUTopology()
		if err := r.Create(ctx, topology); err != nil {
			return topologyReconcileState{
				status:               tauv1alpha1.TauClusterSectionStatus{Observed: 1, Drifted: 1},
				queuesCondition:      condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionFalse, "TopologyCreateFailed", err.Error(), generation),
				driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "TopologyDrift", err.Error(), generation),
				ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no topology ownership conflict was found", generation),
				reconciliationFailed: true,
			}, fmt.Errorf("create Topology %q: %w", tauGPUNodeTopologyName, err)
		}
	}
	if nodeErr != nil {
		message := nodeErr.Error()
		return topologyReconcileState{
			status:               nodeStatus,
			queuesCondition:      condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionFalse, "TopologyNodeLabelFailed", message, generation),
			driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "TopologyNodeLabelDrift", message, generation),
			ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no topology ownership conflict was found", generation),
			reconciliationFailed: true,
			managedResources:     managedTopologyStatus(topology),
		}, nodeErr
	}
	return readyTopologyState(generation, nodeStatus, nodeDrift, topology), nil
}

type nodeTopologyPlan struct {
	node    *corev1.Node
	desired map[string]string
	invalid bool
}

func (r *TauClusterReconciler) reconcileNodeTopologyLabels(
	ctx context.Context,
	mutate bool,
) (tauv1alpha1.TauClusterSectionStatus, bool, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return tauv1alpha1.TauClusterSectionStatus{}, false, fmt.Errorf("list nodes for TauGrid topology: %w", err)
	}

	status := tauv1alpha1.TauClusterSectionStatus{}
	plans := make([]nodeTopologyPlan, 0)
	var reconcileErr error
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	for i := range nodes.Items {
		node := &nodes.Items[i]
		status.Observed++
		desired, err := desiredNodeTopologyLabels(node)
		if err != nil {
			status.Drifted++
			reconcileErr = errors.Join(reconcileErr, err)
			if desired != nil && !nodeHasLabels(node, desired) {
				plans = append(plans, nodeTopologyPlan{node: node, desired: desired, invalid: true})
			}
			continue
		}
		if nodeHasLabels(node, desired) {
			status.Ready++
			continue
		}
		status.Drifted++
		plans = append(plans, nodeTopologyPlan{node: node, desired: desired})
	}
	if !mutate {
		return status, status.Drifted > 0, reconcileErr
	}

	for _, plan := range plans {
		before := plan.node.DeepCopy()
		if plan.node.Labels == nil {
			plan.node.Labels = map[string]string{}
		}
		for key, value := range plan.desired {
			plan.node.Labels[key] = value
		}
		if err := r.Patch(ctx, plan.node, client.MergeFrom(before)); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("patch node %q topology labels: %w", plan.node.Name, err))
			continue
		}
		if plan.invalid {
			continue
		}
		status.Drifted--
		status.Ready++
	}
	return status, status.Drifted > 0, reconcileErr
}

func desiredNodeTopologyLabels(node *corev1.Node) (map[string]string, error) {
	region := node.Labels[labelRegion]
	if region == "" {
		region = node.Labels[labelAKSRegion]
	}
	if region == "" {
		region = isolatedTopologyLabel("unplaced", node.Name)
	} else if problems := validation.IsValidLabelValue(region); len(problems) > 0 {
		region = isolatedTopologyLabel("unplaced", node.Name)
		return isolatedNodeTopologyLabels(node, region), fmt.Errorf("node %q has invalid region label", node.Name)
	}

	baseline := isolatedNodeTopologyLabels(node, region)
	site := baseline[labelkeys.LabelSite]
	domain := baseline[labelkeys.LabelNetworkDomain]
	infiniband := false

	if !isAzureNode(node) {
		if sourceSite := node.Labels[labelFlexSite]; sourceSite != "" {
			if problems := validation.IsDNS1123Label(sourceSite); len(problems) > 0 {
				return baseline, fmt.Errorf("node %q has invalid %s label", node.Name, labelFlexSite)
			}
			site = networkDomainLabel("site", sourceSite)
		}
		return map[string]string{
			labelkeys.LabelSite:          site,
			labelkeys.LabelRegion:        region,
			labelkeys.LabelNetworkDomain: domain,
			labelkeys.LabelInfiniband:    "false",
		}, nil
	}

	if isExternalAzureNode(node) {
		sourceSite := node.Labels[labelFlexSite]
		if problems := validation.IsDNS1123Label(sourceSite); len(problems) > 0 {
			return baseline, fmt.Errorf("Azure Flex node %q has no valid %s label", node.Name, labelFlexSite)
		}
		site = networkDomainLabel("azure-site", sourceSite)
		switch strings.ToLower(node.Labels[labelAKSInfiniband]) {
		case "true":
			infiniband = true
			domain = networkDomainLabel("azure-site-ib", sourceSite)
		case "false":
		default:
			return baseline, fmt.Errorf("Azure Flex node %q must set %s to true or false", node.Name, labelAKSInfiniband)
		}
	} else {
		site = networkDomainLabel("azure", region)
		if node.Labels[labelkeys.LabelGPUClass] != "" {
			infiniband = true
			domain = networkDomainLabel("azure-ib", region)
		}
	}
	return map[string]string{
		labelkeys.LabelSite:          site,
		labelkeys.LabelRegion:        region,
		labelkeys.LabelNetworkDomain: domain,
		labelkeys.LabelInfiniband:    fmt.Sprintf("%t", infiniband),
	}, nil
}

func isolatedNodeTopologyLabels(node *corev1.Node, region string) map[string]string {
	return map[string]string{
		labelkeys.LabelSite:          isolatedTopologyLabel("isolated-site", node.Name),
		labelkeys.LabelRegion:        region,
		labelkeys.LabelNetworkDomain: isolatedTopologyLabel("isolated-domain", node.Name),
		labelkeys.LabelInfiniband:    "false",
	}
}

func isAzureNode(node *corev1.Node) bool {
	if cloud := node.Labels[labelAKSCloud]; cloud != "" {
		return strings.EqualFold(cloud, "azure")
	}
	return strings.HasPrefix(strings.ToLower(node.Spec.ProviderID), "azure://") ||
		node.Labels[labelAzureManagedCluster] != ""
}

func isExternalAzureNode(node *corev1.Node) bool {
	return strings.EqualFold(node.Labels[labelAzureManaged], "false") ||
		strings.EqualFold(node.Labels[labelStretchManaged], "true")
}

func hasManagedTopologyLabels(node *corev1.Node) bool {
	return node.Labels[labelkeys.LabelSite] != "" ||
		node.Labels[labelkeys.LabelRegion] != "" ||
		node.Labels[labelkeys.LabelNetworkDomain] != "" ||
		node.Labels[labelkeys.LabelInfiniband] != ""
}

func isolatedTopologyLabel(prefix, nodeName string) string {
	sum := sha256.Sum256([]byte(nodeName))
	return prefix + "-" + hex.EncodeToString(sum[:8])
}

func networkDomainLabel(prefix, identity string) string {
	value := prefix + "-" + identity
	if len(value) <= validation.LabelValueMaxLength {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	suffix := hex.EncodeToString(sum[:6])
	baseLength := validation.LabelValueMaxLength - len(suffix) - 1
	base := strings.TrimRight(value[:baseLength], "-_.")
	return base + "-" + suffix
}

func desiredTauGPUTopology() *unstructured.Unstructured {
	topology := newQueueObject(topologyGVK)
	topology.SetName(tauGPUNodeTopologyName)
	topology.SetLabels(map[string]string{
		labelManagedBy: labelManagedByValue,
	})
	topology.Object["spec"] = map[string]any{
		"levels": []any{
			map[string]any{"nodeLabel": labelkeys.LabelSite},
			map[string]any{"nodeLabel": labelkeys.LabelNetworkDomain},
			map[string]any{"nodeLabel": labelHostname},
		},
	}
	return topology
}

func readyTopologyState(
	generation int64,
	nodeStatus tauv1alpha1.TauClusterSectionStatus,
	nodeDrift bool,
	topology *unstructured.Unstructured,
) topologyReconcileState {
	status := tauv1alpha1.TauClusterSectionStatus{Observed: 1, Ready: 1}
	status.Observed += nodeStatus.Observed
	status.Ready += nodeStatus.Ready
	status.Drifted += nodeStatus.Drifted
	driftStatus := metav1.ConditionFalse
	driftReason := "NoTopologyDrift"
	driftMessage := "Node topology labels and Kueue topology are reconciled"
	if nodeDrift {
		driftStatus = metav1.ConditionTrue
		driftReason = "TopologyNodeLabelDrift"
		driftMessage = "Node topology labels need reconciliation"
	}
	return topologyReconcileState{
		status:             status,
		queuesCondition:    condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionTrue, "TopologyReady", "TauGrid GPU Topology is reconciled", generation),
		driftCondition:     condition(tauv1alpha1.ConditionDriftDetected, driftStatus, driftReason, driftMessage, generation),
		ownershipCondition: condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "TauGrid owns the GPU Topology", generation),
		managedResources:   managedTopologyStatus(topology),
	}
}

func managedTopologyStatus(topology *unstructured.Unstructured) []tauv1alpha1.TauManagedResourceStatus {
	return []tauv1alpha1.TauManagedResourceStatus{{
		APIVersion: topologyGVK.GroupVersion().String(),
		Kind:       topologyGVK.Kind,
		Name:       topology.GetName(),
		UID:        string(topology.GetUID()),
		Ownership:  tauv1alpha1.ClusterOwnershipManage,
	}}
}
