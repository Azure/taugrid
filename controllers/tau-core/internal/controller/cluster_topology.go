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

func (r *TauClusterReconciler) reconcileSiteTopology(
	ctx context.Context,
	cluster *tauv1alpha1.TauCluster,
	mutate bool,
) (topologyReconcileState, error) {
	generation := cluster.Generation
	if err := validateSites(cluster.Spec.Sites); err != nil {
		message := err.Error()
		return topologyReconcileState{
			queuesCondition:      condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionFalse, "InvalidSites", message, generation),
			driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionUnknown, "InvalidSites", message, generation),
			ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no topology ownership conflict was evaluated", generation),
			reconciliationFailed: true,
		}, nil
	}

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

	nodeStatus, nodeDrift, err := r.reconcileSiteNodeLabels(ctx, cluster.Spec.Sites, mutate)
	if err != nil {
		message := err.Error()
		return topologyReconcileState{
			status:               nodeStatus,
			queuesCondition:      condition(tauv1alpha1.ConditionQueuesReady, metav1.ConditionFalse, "TopologyNodeLabelFailed", message, generation),
			driftCondition:       condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionTrue, "TopologyNodeLabelDrift", message, generation),
			ownershipCondition:   condition(tauv1alpha1.ConditionOwnershipConflict, metav1.ConditionFalse, "NoConflictObserved", "no topology ownership conflict was found", generation),
			reconciliationFailed: true,
		}, err
	}

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
		return readyTopologyState(generation, nodeStatus, nodeDrift, topology), nil
	}
	return readyTopologyState(generation, nodeStatus, nodeDrift, topology), nil
}

func validateSites(sites []tauv1alpha1.TauSiteSpec) error {
	seen := map[string]struct{}{}
	for i, site := range sites {
		if problems := validation.IsDNS1123Label(site.Name); len(problems) > 0 {
			return fmt.Errorf("sites[%d].name %q: %s", i, site.Name, problems[0])
		}
		if _, ok := seen[site.Name]; ok {
			return fmt.Errorf("sites[%d].name %q is duplicated", i, site.Name)
		}
		seen[site.Name] = struct{}{}
		if problems := validation.IsValidLabelValue(site.Region); len(problems) > 0 || strings.TrimSpace(site.Region) == "" {
			return fmt.Errorf("sites[%d].region %q is not a valid topology label value", i, site.Region)
		}
		for key, value := range site.NodeSelector {
			if problems := validation.IsQualifiedName(key); len(problems) > 0 {
				return fmt.Errorf("sites[%d].nodeSelector[%q]: %s", i, key, problems[0])
			}
			if problems := validation.IsValidLabelValue(value); len(problems) > 0 {
				return fmt.Errorf("sites[%d].nodeSelector[%q]: %s", i, key, problems[0])
			}
		}
		switch site.Provider {
		case tauv1alpha1.SiteProviderMicrosoft:
			if site.Infiniband != nil && !*site.Infiniband {
				return fmt.Errorf("sites[%d] %q cannot disable InfiniBand for Microsoft capacity under the launch topology contract", i, site.Name)
			}
		case tauv1alpha1.SiteProviderFlex:
			if site.Infiniband == nil {
				return fmt.Errorf("sites[%d] %q must explicitly set infiniband for Flex capacity", i, site.Name)
			}
		default:
			return fmt.Errorf("sites[%d].provider %q is unsupported", i, site.Provider)
		}
	}
	return nil
}

func (r *TauClusterReconciler) reconcileSiteNodeLabels(
	ctx context.Context,
	sites []tauv1alpha1.TauSiteSpec,
	mutate bool,
) (tauv1alpha1.TauClusterSectionStatus, bool, error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return tauv1alpha1.TauClusterSectionStatus{}, false, fmt.Errorf("list nodes for site topology: %w", err)
	}

	status := tauv1alpha1.TauClusterSectionStatus{}
	drift := false
	var reconcileErr error
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Name < nodes.Items[j].Name })
	for i := range nodes.Items {
		node := &nodes.Items[i]
		hasManagedLabels := node.Labels[labelkeys.LabelNetworkDomain] != "" || node.Labels[labelkeys.LabelInfiniband] != ""
		if node.Labels[labelkeys.LabelGPUClass] == "" && !hasManagedLabels {
			continue
		}
		site, matched, err := matchingSite(node, sites)
		if err != nil {
			return status, true, err
		}
		if !matched {
			if !hasManagedLabels {
				continue
			}
			status.Observed++
			status.Drifted++
			drift = true
			if !mutate {
				continue
			}
			before := node.DeepCopy()
			delete(node.Labels, labelkeys.LabelNetworkDomain)
			delete(node.Labels, labelkeys.LabelInfiniband)
			if err := r.Patch(ctx, node, client.MergeFrom(before)); err != nil {
				reconcileErr = errors.Join(reconcileErr, fmt.Errorf("remove node %q stale site topology labels: %w", node.Name, err))
				continue
			}
			status.Drifted--
			status.Ready++
			continue
		}
		status.Observed++
		desired := siteNodeLabels(node, site)
		if nodeHasLabels(node, desired) {
			status.Ready++
			continue
		}
		status.Drifted++
		drift = true
		if !mutate {
			continue
		}
		before := node.DeepCopy()
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		for key, value := range desired {
			node.Labels[key] = value
		}
		if err := r.Patch(ctx, node, client.MergeFrom(before)); err != nil {
			reconcileErr = errors.Join(reconcileErr, fmt.Errorf("patch node %q site topology labels: %w", node.Name, err))
			continue
		}
		status.Drifted--
		status.Ready++
	}
	return status, drift && status.Drifted > 0, reconcileErr
}

func matchingSite(node *corev1.Node, sites []tauv1alpha1.TauSiteSpec) (tauv1alpha1.TauSiteSpec, bool, error) {
	var matched *tauv1alpha1.TauSiteSpec
	for i := range sites {
		site := &sites[i]
		if node.Labels[labelRegion] != site.Region || !matchesLabels(node.Labels, site.NodeSelector) {
			continue
		}
		switch site.Provider {
		case tauv1alpha1.SiteProviderFlex:
			if node.Labels[labelFlexSite] != site.Name {
				continue
			}
		case tauv1alpha1.SiteProviderMicrosoft:
			if node.Labels[labelFlexSite] != "" {
				continue
			}
		}
		if matched != nil {
			return tauv1alpha1.TauSiteSpec{}, false, fmt.Errorf("node %q matches multiple TauGrid sites %q and %q", node.Name, matched.Name, site.Name)
		}
		copy := *site
		matched = &copy
	}
	if matched == nil {
		return tauv1alpha1.TauSiteSpec{}, false, nil
	}
	return *matched, true, nil
}

func matchesLabels(labels, selector map[string]string) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func siteNodeLabels(node *corev1.Node, site tauv1alpha1.TauSiteSpec) map[string]string {
	infiniband := site.Provider == tauv1alpha1.SiteProviderMicrosoft || (site.Infiniband != nil && *site.Infiniband)
	domain := networkDomainLabel("microsoft", site.Region)
	if site.Provider == tauv1alpha1.SiteProviderFlex {
		domain = networkDomainLabel("flex", site.Name)
		if !infiniband {
			sum := sha256.Sum256([]byte(node.Name))
			domain = "isolated-" + hex.EncodeToString(sum[:8])
		}
	}
	return map[string]string{
		labelkeys.LabelNetworkDomain: domain,
		labelkeys.LabelInfiniband:    fmt.Sprintf("%t", infiniband),
	}
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
			map[string]any{"nodeLabel": labelRegion},
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
	driftMessage := "TauGrid site labels and Kueue topology are reconciled"
	if nodeDrift {
		driftStatus = metav1.ConditionTrue
		driftReason = "TopologyNodeLabelDrift"
		driftMessage = "TauGrid site node labels need reconciliation"
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
