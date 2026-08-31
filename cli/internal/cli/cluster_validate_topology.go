// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/kueueapi"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	runtopology "github.com/Azure/taugrid/core/topology"
)

func newClusterValidateTopologyCmd() *cobra.Command {
	var (
		kubeContext string
		profileName string
	)
	cmd := &cobra.Command{
		Use:   "topology",
		Short: "Verify cluster topology matches Kueue ResourceFlavor expectations",
		Long: `Verify that the live cluster topology matches Kueue ResourceFlavor expectations.

For each GPU ResourceFlavor referenced by the ClusterQueue, this command checks:
  - nodes matching the flavor's nodeLabels exist
  - matched GPU nodes expose the ClusterQueue's configured GPU resource
  - GPU nodes have topology.kubernetes.io/zone populated
  - IB-capable SKUs (H200, A100) have Ready nodes

ResourceFlavors without GPU capacity in the resolved ClusterQueue contract are
reported and skipped.

Use --profile to validate one ready TauCluster workload profile. Without it,
Tau validates every ready profile in the current TauCluster profile-set revision.`,
		Example: `  tau cluster validate topology
  tau cluster validate topology --profile research-training`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			r := kube.New(kubeContext)
			client, err := newClusterProfileClient(kubeContext)
			if err != nil {
				return err
			}
			return validateReadyWorkloadProfilesTopology(
				cmd.Context(),
				cmd.OutOrStdout(),
				r,
				profile.NewClusterProvider(client),
				profileName,
				r.Context,
			)
		},
	}
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVar(&profileName, "profile", "", "validate one ready TauCluster workload profile (default: all ready profiles)")
	return cmd
}

type checkStatus string

const (
	checkOK    checkStatus = "ok"
	checkWarn  checkStatus = "warn"
	checkError checkStatus = "error"
)

type topologyCheckResult struct {
	label   string
	status  checkStatus
	message string
}

func writeResults(w io.Writer, results []topologyCheckResult, passed, warnings, errors *int) {
	for _, r := range results {
		fmt.Fprintf(w, "  %-5s %s: %s\n", r.status, r.label, r.message)
		switch r.status {
		case checkOK:
			*passed++
		case checkWarn:
			*warnings++
		case checkError:
			*errors++
		}
	}
}

func writeSummary(w io.Writer, passed, warnings, errors int) error {
	fmt.Fprintf(w, "\nsummary: %d passed, %d warnings, %d errors\n", passed, warnings, errors)
	if errors > 0 {
		return fmt.Errorf("topology validation failed: %d error(s)", errors)
	}
	return nil
}

func validateClusterTopology(ctx context.Context, w io.Writer, r validateNodesRunner, ctxName, cqName string) error {
	cq, err := fetchClusterQueue(ctx, r, cqName)
	if err != nil {
		return fmt.Errorf("fetch clusterqueue %s: %w", cqName, err)
	}

	fmt.Fprintf(w, "cluster: %s\n", dash(ctxName))
	fmt.Fprintf(w, "clusterqueue: %s\n\n", cqName)

	contracts := clusterQueueFlavorContracts(cq)
	if len(contracts) == 0 {
		fmt.Fprintf(w, "warning: clusterqueue %s has no resource flavors\n", cqName)
		return fmt.Errorf("clusterqueue %s has no resource flavors", cqName)
	}

	var passed, warnings, errors int
	for _, contract := range contracts {
		results := validateFlavor(ctx, r, contract)
		fmt.Fprintf(w, "resourceflavor %s:\n", contract.name)
		writeResults(w, results, &passed, &warnings, &errors)
		fmt.Fprintln(w)
	}

	return writeSummary(w, passed, warnings, errors)
}

func validateReadyWorkloadProfilesTopology(
	ctx context.Context,
	w io.Writer,
	r validateNodesRunner,
	provider *profile.Provider,
	profileName, ctxName string,
) error {
	set, err := provider.ProfileSet(ctx)
	if err != nil {
		return err
	}
	var candidates []profile.ResolvedWorkloadProfile
	for _, candidate := range set.Profiles {
		if profileName == "" || candidate.Name == strings.TrimSpace(profileName) {
			candidates = append(candidates, candidate)
		}
	}
	if len(candidates) == 0 {
		_, err := provider.Select(ctx, profile.SelectionRequest{Name: profileName})
		return err
	}
	for _, candidate := range candidates {
		request := representativeProfileSelectionRequest(candidate)
		selection, err := provider.Select(ctx, request)
		if err != nil {
			return err
		}
		if selection.Generation != set.Generation || selection.ProfileSetHash != set.ProfileSetHash {
			return fmt.Errorf(
				"TauCluster workload profile revision changed during topology validation (generation/hash %d/%s -> %d/%s); retry",
				set.Generation,
				set.ProfileSetHash,
				selection.Generation,
				selection.ProfileSetHash,
			)
		}
		if err := validateResolvedWorkloadProfileTopology(ctx, w, r, selection, ctxName); err != nil {
			return err
		}
	}
	return nil
}

func representativeProfileSelectionRequest(candidate profile.ResolvedWorkloadProfile) profile.SelectionRequest {
	first := func(values []string) string {
		if len(values) == 0 {
			return ""
		}
		return values[0]
	}
	return profile.SelectionRequest{
		Name:      candidate.Name,
		Namespace: first(candidate.Applicability.Namespaces),
		Team:      first(candidate.Applicability.Teams),
		Lane:      first(candidate.Applicability.Lanes),
	}
}

func validateResolvedWorkloadProfileTopology(
	ctx context.Context,
	w io.Writer,
	r validateNodesRunner,
	selection profile.Selection,
	ctxName string,
) error {
	p := selection.Profile
	fmt.Fprintf(w, "cluster: %s\n", dash(ctxName))
	fmt.Fprintf(w, "profile: %s\n", p.Name)
	fmt.Fprintf(w, "taucluster generation: %d\n", selection.Generation)
	fmt.Fprintf(w, "profile set hash: %s\n\n", selection.ProfileSetHash)

	var passed, warnings, errors int
	var results []topologyCheckResult
	for _, check := range workloadProfileObjectChecks(p) {
		out, err := r.Raw(ctx, check.args, nil)
		if err != nil || strings.TrimSpace(out) == "" {
			detail := "not found"
			if err != nil {
				detail = err.Error()
			}
			results = append(results, topologyCheckResult{check.label, checkError, detail})
		} else {
			results = append(results, topologyCheckResult{check.label, checkOK, "exists"})
		}
	}

	wantedFlavors := make(map[string]struct{}, len(p.ResourceFlavors))
	for _, name := range p.ResourceFlavors {
		wantedFlavors[name] = struct{}{}
	}
	foundFlavors := make(map[string]struct{}, len(wantedFlavors))
	var contracts []topologyFlavorContract
	for _, clusterQueue := range p.ClusterQueues {
		cq, err := fetchClusterQueue(ctx, r, clusterQueue)
		if err != nil {
			results = append(results, topologyCheckResult{
				"gpu-contract",
				checkError,
				fmt.Sprintf("cannot fetch clusterqueue %s contract: %v", clusterQueue, err),
			})
			continue
		}
		for _, contract := range clusterQueueFlavorContracts(cq) {
			if _, wanted := wantedFlavors[contract.name]; !wanted {
				continue
			}
			foundFlavors[contract.name] = struct{}{}
			if len(contract.gpuResources) == 0 {
				continue
			}
			contracts = append(contracts, contract)
		}
	}
	for flavor := range wantedFlavors {
		if _, found := foundFlavors[flavor]; !found {
			results = append(results, topologyCheckResult{
				"gpu-contract",
				checkError,
				fmt.Sprintf("resourceflavor %s is not referenced by resolved clusterqueues %s", flavor, strings.Join(p.ClusterQueues, ", ")),
			})
		}
	}
	if len(wantedFlavors) == 0 {
		results = append(results, topologyCheckResult{"gpu-contract", checkError, "profile has no resolved ResourceFlavor"})
	} else if p.GPUsPerWorker > 0 && len(contracts) == 0 {
		results = append(results, topologyCheckResult{
			"gpu-contract",
			checkError,
			"profile has no GPU-capable ResourceFlavor in its resolved ClusterQueues",
		})
	}

	fmt.Fprintf(w, "profile %s:\n", p.Name)
	writeResults(w, results, &passed, &warnings, &errors)
	for _, contract := range contracts {
		fmt.Fprintf(w, "\nresourceflavor %s:\n", contract.name)
		writeResults(w, validateFlavor(ctx, r, contract), &passed, &warnings, &errors)
	}
	return writeSummary(w, passed, warnings, errors)
}

type objectCheck struct {
	label string
	args  []string
}

func workloadProfileObjectChecks(p profile.ResolvedWorkloadProfile) []objectCheck {
	var checks []objectCheck
	for _, queue := range p.LocalQueues {
		checks = append(checks, objectCheck{
			"localqueue " + queue.Namespace + "/" + queue.Name,
			[]string{"-n", queue.Namespace, "get", "localqueue.kueue.x-k8s.io", queue.Name, "-o", "name"},
		})
	}
	for _, name := range p.ClusterQueues {
		checks = append(checks, objectCheck{"clusterqueue " + name, []string{"get", "clusterqueue.kueue.x-k8s.io", name, "-o", "name"}})
	}
	for _, name := range p.Topologies {
		checks = append(checks, objectCheck{"topology " + name, []string{"get", "topology.kueue.x-k8s.io", name, "-o", "name"}})
	}
	for _, name := range p.WorkloadPriorityClasses {
		checks = append(checks, objectCheck{"workloadpriorityclass " + name, []string{"get", "workloadpriorityclass.kueue.x-k8s.io", name, "-o", "name"}})
	}
	for _, name := range p.PodPriorityClasses {
		checks = append(checks, objectCheck{"priorityclass " + name, []string{"get", "priorityclass.scheduling.k8s.io", name, "-o", "name"}})
	}
	sort.Slice(checks, func(i, j int) bool { return checks[i].label < checks[j].label })
	return checks
}

func validateFlavor(ctx context.Context, r validateNodesRunner, contract topologyFlavorContract) []topologyCheckResult {
	rf, err := fetchResourceFlavor(ctx, r, contract.name)
	if err != nil {
		return []topologyCheckResult{{label: "fetch", status: checkError, message: fmt.Sprintf("cannot fetch resourceflavor: %v", err)}}
	}

	if len(contract.gpuResources) == 0 {
		return []topologyCheckResult{{label: "gpu-contract", status: checkOK, message: "no GPU capacity in ClusterQueue contract; GPU topology checks skipped"}}
	}

	nodes, err := fetchNodesByLabels(ctx, r, rf.Spec.NodeLabels)
	if err != nil {
		return []topologyCheckResult{{label: "node-match", status: checkError, message: fmt.Sprintf("cannot list nodes: %v", err)}}
	}

	var results []topologyCheckResult

	var draDevices map[string]int
	if contract.hasGPUResource(kueueapi.GPUResource) {
		draDevices, err = fetchNodesWithResourceSlices(ctx, r)
		if err != nil {
			return []topologyCheckResult{{label: "gpu-resourceslices", status: checkError, message: fmt.Sprintf("cannot list ResourceSlices: %v", err)}}
		}
	}

	selectorDesc := formatNodeSelector(rf.Spec.NodeLabels)
	if len(rf.Spec.NodeLabels) == 0 {
		selectorDesc = "<all nodes>"
	}
	if len(nodes) == 0 {
		results = append(results, topologyCheckResult{"node-match", checkError,
			fmt.Sprintf("0 nodes match %s — check instance-type label or node pool availability", selectorDesc)})
		return results
	}
	gpuNodes := selectFlavorGPUNodes(nodes, rf, contract, draDevices)
	if len(gpuNodes) == 0 {
		results = append(results, topologyCheckResult{"node-match", checkError,
			fmt.Sprintf("0 GPU-capable nodes among %d node(s) matching %s — check ResourceFlavor labels/taints and GPU registration", len(nodes), selectorDesc)})
		return results
	}
	if len(gpuNodes) == len(nodes) {
		results = append(results, topologyCheckResult{"node-match", checkOK,
			fmt.Sprintf("%d GPU-capable node(s) match %s", len(gpuNodes), selectorDesc)})
	} else {
		results = append(results, topologyCheckResult{"node-match", checkOK,
			fmt.Sprintf("%d GPU-capable node(s) selected from %d node(s) matching %s", len(gpuNodes), len(nodes), selectorDesc)})
	}

	zoneReady, readyCount, gpuTotal := 0, 0, len(gpuNodes)
	for _, n := range gpuNodes {
		if n.Metadata.Labels["topology.kubernetes.io/zone"] != "" {
			zoneReady++
		}
		if nodeIsReady(n) {
			readyCount++
		}
	}

	for _, resourceName := range contract.gpuResources {
		results = append(results, validateGPUResource(resourceName, gpuNodes, draDevices))
	}

	if zoneReady == gpuTotal {
		results = append(results, topologyCheckResult{"topology-zone", checkOK,
			fmt.Sprintf("%d/%d nodes have topology.kubernetes.io/zone", zoneReady, gpuTotal)})
	} else {
		results = append(results, topologyCheckResult{"topology-zone", checkWarn,
			fmt.Sprintf("%d/%d nodes have topology.kubernetes.io/zone — Kueue Topology scheduling requires this label", zoneReady, gpuTotal)})
	}

	instanceType := rf.Spec.NodeLabels["node.kubernetes.io/instance-type"]
	if sku, ok := ibCapableSKUs[strings.ToLower(instanceType)]; ok {
		if readyCount == gpuTotal {
			results = append(results, topologyCheckResult{"ib-capable", checkOK,
				fmt.Sprintf("%s SKU, %d/%d matched nodes are Ready", sku, readyCount, gpuTotal)})
		} else {
			results = append(results, topologyCheckResult{"ib-capable", checkWarn,
				fmt.Sprintf("%s SKU, %d/%d matched nodes are Ready — IB fabric requires all nodes healthy", sku, readyCount, gpuTotal)})
		}
	}

	return results
}

type topologyNodeDoc struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool               `json:"unschedulable"`
		Taints        []topologyTaintDoc `json:"taints"`
	} `json:"spec"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type topologyNodeListDoc struct {
	Items []topologyNodeDoc `json:"items"`
}

type topologyTaintDoc struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

type topologyFlavorContract struct {
	name         string
	gpuResources []string
}

func (c topologyFlavorContract) hasGPUResource(name string) bool {
	for _, resourceName := range c.gpuResources {
		if resourceName == name {
			return true
		}
	}
	return false
}

type topologyClusterQueueDoc struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		// Kueue v1beta1 uses cohort; v1beta2 uses cohortName.
		Cohort         string `json:"cohort"`
		CohortName     string `json:"cohortName"`
		ResourceGroups []struct {
			Flavors []struct {
				Name      string `json:"name"`
				Resources []struct {
					Name           string             `json:"name"`
					NominalQuota   kueueapi.Quantity  `json:"nominalQuota"`
					BorrowingLimit *kueueapi.Quantity `json:"borrowingLimit"`
				} `json:"resources"`
			} `json:"flavors"`
		} `json:"resourceGroups"`
	} `json:"spec"`
}

func (cq topologyClusterQueueDoc) hasCohort() bool {
	return cq.Spec.Cohort != "" || cq.Spec.CohortName != ""
}

func clusterQueueFlavorContracts(cq topologyClusterQueueDoc) []topologyFlavorContract {
	capacities := make(map[string]map[string]bool)
	hasCohort := cq.hasCohort()
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if f.Name == "" {
				continue
			}
			if capacities[f.Name] == nil {
				capacities[f.Name] = make(map[string]bool)
			}
			for _, resource := range f.Resources {
				if isRenderedGPUResource(resource.Name) {
					capacities[f.Name][resource.Name] = capacities[f.Name][resource.Name] || quotaHasCapacity(resource.NominalQuota, resource.BorrowingLimit, hasCohort)
				}
			}
		}
	}
	contracts := make([]topologyFlavorContract, 0, len(capacities))
	for name, resources := range capacities {
		contract := topologyFlavorContract{name: name}
		for resourceName, hasCapacity := range resources {
			if hasCapacity {
				contract.gpuResources = append(contract.gpuResources, resourceName)
			}
		}
		sort.Strings(contract.gpuResources)
		contracts = append(contracts, contract)
	}
	sort.Slice(contracts, func(i, j int) bool { return contracts[i].name < contracts[j].name })
	return contracts
}

func quotaHasCapacity(nominal kueueapi.Quantity, borrowingLimit *kueueapi.Quantity, hasCohort bool) bool {
	if nominal.Int64() > 0 {
		return true
	}
	if borrowingLimit != nil {
		return borrowingLimit.Int64() > 0
	}
	// In a cohort, an omitted borrowingLimit means unlimited borrowing. Without
	// a cohort, Kueue requires borrowingLimit to be omitted and no borrowing is possible.
	return hasCohort
}

type topologyResourceFlavorDoc struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		NodeLabels map[string]string  `json:"nodeLabels"`
		NodeTaints []topologyTaintDoc `json:"nodeTaints"`
	} `json:"spec"`
}

func fetchJSON[T any](ctx context.Context, r validateNodesRunner, args []string) (T, error) {
	var zero T
	out, err := r.Raw(ctx, args, nil)
	if err != nil {
		return zero, err
	}
	var doc T
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return zero, fmt.Errorf("parse json: %w", err)
	}
	return doc, nil
}

func fetchClusterQueue(ctx context.Context, r validateNodesRunner, name string) (topologyClusterQueueDoc, error) {
	return fetchJSON[topologyClusterQueueDoc](ctx, r, []string{"get", "clusterqueue.kueue.x-k8s.io", name, "-o", "json"})
}

func fetchResourceFlavor(ctx context.Context, r validateNodesRunner, name string) (topologyResourceFlavorDoc, error) {
	return fetchJSON[topologyResourceFlavorDoc](ctx, r, []string{"get", "resourceflavor.kueue.x-k8s.io", name, "-o", "json"})
}

func fetchNodesByLabels(ctx context.Context, r validateNodesRunner, labels map[string]string) ([]topologyNodeDoc, error) {
	selector := formatNodeSelector(labels)
	args := []string{"get", "nodes"}
	if len(labels) > 0 {
		args = append(args, "-l", selector)
	}
	args = append(args, "-o", "json")
	doc, err := fetchJSON[topologyNodeListDoc](ctx, r, args)
	if err != nil {
		return nil, err
	}
	return doc.Items, nil
}

func gpuAllocatable(n topologyNodeDoc) int {
	return gpuResourceAllocatable(n, kueueapi.GPUResourceDevicePlugin)
}

func gpuResourceAllocatable(n topologyNodeDoc, resourceName string) int {
	count, err := strconv.Atoi(n.Status.Allocatable[resourceName])
	if err != nil {
		return 0
	}
	return count
}

func selectFlavorGPUNodes(nodes []topologyNodeDoc, rf topologyResourceFlavorDoc, contract topologyFlavorContract, draDevices map[string]int) []topologyNodeDoc {
	selectorIsGPU := flavorSelectsGPU(rf.Spec.NodeLabels)
	var selected []topologyNodeDoc
	for _, node := range nodes {
		registered := false
		for _, resourceName := range contract.gpuResources {
			switch resourceName {
			case kueueapi.GPUResource:
				_, registered = draDevices[node.Metadata.Name]
			default:
				registered = gpuResourceAllocatable(node, resourceName) > 0
			}
			if registered {
				break
			}
		}
		if registered || selectorIsGPU || nodeMatchesFlavorTaints(node, rf.Spec.NodeTaints) || looksLikeGPUNode(node.Metadata.Labels, nodeInstanceType(node.Metadata.Labels)) {
			selected = append(selected, node)
		}
	}
	return selected
}

func flavorSelectsGPU(labels map[string]string) bool {
	if looksLikeGPUNode(labels, nodeInstanceType(labels)) {
		return true
	}
	for _, key := range []string{runtopology.ManagedGPUSeriesLabel, "nvidia.com/gpu.product"} {
		if labels[key] != "" {
			return true
		}
	}
	return false
}

func nodeMatchesFlavorTaints(node topologyNodeDoc, required []topologyTaintDoc) bool {
	if len(required) == 0 {
		return false
	}
	for _, want := range required {
		found := false
		for _, got := range node.Spec.Taints {
			if got.Key == want.Key && got.Value == want.Value && (want.Effect == "" || got.Effect == want.Effect) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func validateGPUResource(resourceName string, nodes []topologyNodeDoc, draDevices map[string]int) topologyCheckResult {
	ready := 0
	for _, node := range nodes {
		if resourceName == kueueapi.GPUResource {
			if draDevices[node.Metadata.Name] > 0 {
				ready++
			}
		} else if gpuResourceAllocatable(node, resourceName) > 0 {
			ready++
		}
	}
	label := "gpu-allocatable"
	mechanism := fmt.Sprintf("report %s > 0", resourceName)
	missing := "device plugin may be starting on remaining nodes"
	if resourceName == kueueapi.GPUResource {
		label = "gpu-resourceslices"
		mechanism = fmt.Sprintf("publish %s ResourceSlice devices", resourceName)
		missing = "DRA driver may be starting on remaining nodes"
	}
	message := fmt.Sprintf("%d/%d nodes %s", ready, len(nodes), mechanism)
	switch {
	case ready == len(nodes):
		return topologyCheckResult{label, checkOK, message}
	case ready > 0:
		return topologyCheckResult{label, checkWarn, message + " — " + missing}
	default:
		return topologyCheckResult{label, checkError, message + " — GPU resource is not registered"}
	}
}

func nodeIsReady(n topologyNodeDoc) bool {
	if n.Spec.Unschedulable {
		return false
	}
	for _, c := range n.Status.Conditions {
		if c.Type == "Ready" {
			return strings.EqualFold(c.Status, "True")
		}
	}
	return false
}

var ibCapableSKUs = map[string]string{
	"standard_nd96isr_h200_v5":  "H200",
	"standard_nd96isr_h100_v5":  "H100",
	"standard_nd96amsr_a100_v4": "A100",
	"standard_nd96isr_gb200_v6": "GB200",
}
