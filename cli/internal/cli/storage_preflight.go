package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/manifest"
	"github.com/Azure/taugrid/core/resourceprofile"
	runtopology "github.com/Azure/taugrid/core/topology"
)

type storagePVCRef struct {
	Field string
	Name  string
}

type pvcDoc struct {
	Spec struct {
		VolumeName string `json:"volumeName"`
	} `json:"spec"`
	Status struct {
		Phase string `json:"phase"`
	} `json:"status"`
}

type pvDoc struct {
	Spec struct {
		CSI *struct {
			Driver string `json:"driver"`
		} `json:"csi"`
	} `json:"spec"`
}

type storageNodeListDoc struct {
	Items []storageNodeDoc `json:"items"`
}

type storageNodeDoc struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Unschedulable bool `json:"unschedulable"`
	} `json:"spec"`
	Status struct {
		Allocatable map[string]string `json:"allocatable"`
		Conditions  []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
	} `json:"status"`
}

type csiNodeListDoc struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Drivers []struct {
				Name string `json:"name"`
			} `json:"drivers"`
		} `json:"spec"`
	} `json:"items"`
}

func explicitManifestPVCRefs(m *manifest.Manifest) []storagePVCRef {
	refs := []storagePVCRef{}
	if pvc := strings.TrimSpace(m.Storage.DataPVC); pvc != "" {
		refs = append(refs, storagePVCRef{Field: "storage.data_pvc", Name: pvc})
	}
	for i, mount := range m.Storage.Mounts {
		if pvc := strings.TrimSpace(mount.PVC); pvc != "" {
			refs = append(refs, storagePVCRef{
				Field: fmt.Sprintf("storage.mounts[%d].pvc", i),
				Name:  pvc,
			})
		}
	}
	return refs
}

func storagePreflightNodeSelector(topologyProfile *profile.Profile, topologyHolder jobrender.Options, explicit map[string]string) (map[string]string, error) {
	var p profile.Profile
	if topologyProfile != nil {
		p = *topologyProfile
	}
	plan, err := runtopology.Build(p, topologyOptionsFromSubmit(topologyHolder))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for k, v := range plan.NodeSelector {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	for k, v := range explicit {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out, nil
}

func validateStorageNodeCompatibility(
	ctx context.Context,
	r kubeRawRunner,
	namespace string,
	m *manifest.Manifest,
	nodeSelector map[string]string,
) error {
	refs := explicitManifestPVCRefs(m)
	if len(refs) == 0 {
		return nil
	}
	requireGPU := m.Compute.GPUs > 0 && !m.IsEval()
	nodes, err := listReadySchedulableNodes(ctx, r, nodeSelector, requireGPU)
	if err != nil {
		return err
	}
	if len(nodes) == 0 {
		return fmt.Errorf("storage PVC compatibility: no Ready schedulable nodes match %s; adjust --node-selector/--gpu-class/--preset before submitting", formatNodeSelector(nodeSelector))
	}
	csiDrivers, err := listCSINodeDrivers(ctx, r)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		driver, err := csiDriverForPVC(ctx, r, namespace, ref)
		if err != nil {
			return err
		}
		if driver == "" {
			continue
		}
		if err := validateDriverOnSelectedNodes(ref, driver, nodes, csiDrivers, nodeSelector); err != nil {
			return err
		}
	}
	return nil
}

func csiDriverForPVC(ctx context.Context, r kubeRawRunner, namespace string, ref storagePVCRef) (string, error) {
	pvcNamespace, err := requireWorkloadNamespace(namespace)
	if err != nil {
		return "", err
	}
	rawPVC, err := r.Raw(ctx, []string{"-n", pvcNamespace, "get", "pvc", ref.Name, "-o", "json"}, nil)
	if err != nil {
		return "", fmt.Errorf(
			"%s %q: platform-managed PVC is unavailable in namespace %q; pre-provision and bind it before submission: %w",
			ref.Field,
			ref.Name,
			pvcNamespace,
			err,
		)
	}
	var pvc pvcDoc
	if err := json.Unmarshal([]byte(rawPVC), &pvc); err != nil {
		return "", fmt.Errorf("%s %q: parse PVC json: %w", ref.Field, ref.Name, err)
	}
	if pvc.Spec.VolumeName == "" {
		phase := strings.TrimSpace(pvc.Status.Phase)
		if phase == "" {
			phase = "unknown"
		}
		return "", fmt.Errorf(
			"%s %q: platform-managed PVC is not Bound in namespace %q (phase=%s); wait for the platform storage lifecycle before submission",
			ref.Field,
			ref.Name,
			pvcNamespace,
			phase,
		)
	}
	rawPV, err := r.Raw(ctx, []string{"get", "pv", pvc.Spec.VolumeName, "-o", "json"}, nil)
	if err != nil {
		return "", fmt.Errorf("%s %q: failed to read bound PV %q: %w", ref.Field, ref.Name, pvc.Spec.VolumeName, err)
	}
	var pv pvDoc
	if err := json.Unmarshal([]byte(rawPV), &pv); err != nil {
		return "", fmt.Errorf("%s %q: parse PV json: %w", ref.Field, ref.Name, err)
	}
	if pv.Spec.CSI == nil {
		return "", nil
	}
	return strings.TrimSpace(pv.Spec.CSI.Driver), nil
}

func listReadySchedulableNodes(ctx context.Context, r kubeRawRunner, selector map[string]string, requireGPU bool) ([]storageNodeDoc, error) {
	raw, err := r.Raw(ctx, []string{"get", "nodes", "-o", "json"}, nil)
	if err != nil {
		return nil, fmt.Errorf("storage PVC compatibility: failed to list nodes: %w", err)
	}
	var doc storageNodeListDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("storage PVC compatibility: parse nodes json: %w", err)
	}
	nodes := []storageNodeDoc{}
	for _, node := range doc.Items {
		if !nodeReadyForScheduling(node) {
			continue
		}
		if !nodeMatchesSelector(node, selector) {
			continue
		}
		if requireGPU && !nodeLooksGPUCapable(node) {
			continue
		}
		nodes = append(nodes, node)
	}
	sort.Slice(nodes, func(i, j int) bool {
		return nodes[i].Metadata.Name < nodes[j].Metadata.Name
	})
	return nodes, nil
}

func listCSINodeDrivers(ctx context.Context, r kubeRawRunner) (map[string]map[string]bool, error) {
	raw, err := r.Raw(ctx, []string{"get", "csinode", "-o", "json"}, nil)
	if err != nil {
		return nil, fmt.Errorf("storage PVC compatibility: failed to list CSINode registrations: %w", err)
	}
	var doc csiNodeListDoc
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		return nil, fmt.Errorf("storage PVC compatibility: parse CSINode json: %w", err)
	}
	out := map[string]map[string]bool{}
	for _, item := range doc.Items {
		drivers := map[string]bool{}
		for _, driver := range item.Spec.Drivers {
			if driver.Name != "" {
				drivers[driver.Name] = true
			}
		}
		out[item.Metadata.Name] = drivers
	}
	return out, nil
}

func validateDriverOnSelectedNodes(ref storagePVCRef, driver string, nodes []storageNodeDoc, csiDrivers map[string]map[string]bool, selector map[string]string) error {
	missing := []string{}
	compatible := []string{}
	registered := []string{}
	for nodeName, drivers := range csiDrivers {
		if drivers[driver] {
			registered = append(registered, nodeName)
		}
	}
	sort.Strings(registered)

	for _, node := range nodes {
		nodeName := node.Metadata.Name
		if csiDrivers[nodeName][driver] {
			compatible = append(compatible, nodeName)
		} else {
			missing = append(missing, nodeName)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	detail := fmt.Sprintf(
		"%s %q is backed by CSI driver %q, but node selector %s still matches node(s) without that driver: %s",
		ref.Field,
		ref.Name,
		driver,
		formatNodeSelector(selector),
		strings.Join(missing, ", "),
	)
	if len(compatible) > 0 {
		detail += fmt.Sprintf("; compatible matching node(s): %s", strings.Join(compatible, ", "))
	} else if len(registered) > 0 {
		detail += fmt.Sprintf("; driver is registered on non-matching node(s): %s", strings.Join(registered, ", "))
	} else {
		detail += "; driver is not registered on any node"
	}
	return fmt.Errorf("%s. Pick a PVC-compatible --node-selector/--gpu-class/--preset, install the CSI driver on the selected nodes, or choose a different data PVC", detail)
}

func nodeReadyForScheduling(node storageNodeDoc) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, cond := range node.Status.Conditions {
		if cond.Type == "Ready" {
			return strings.EqualFold(cond.Status, "True")
		}
	}
	return false
}

func nodeMatchesSelector(node storageNodeDoc, selector map[string]string) bool {
	for k, v := range selector {
		if node.Metadata.Labels[k] != v {
			return false
		}
	}
	return true
}

func nodeLooksGPUCapable(node storageNodeDoc) bool {
	labels := node.Metadata.Labels
	if strings.EqualFold(labels["nvidia.com/gpu.present"], "true") {
		return true
	}
	if strings.TrimSpace(labels["nvidia.com/gpu.count"]) != "" {
		return true
	}
	if _, ok := node.Status.Allocatable["nvidia.com/gpu"]; ok {
		return true
	}
	if strings.TrimSpace(labels["kubernetes.azure.com/accelerator"]) != "" ||
		strings.TrimSpace(labels["accelerator"]) != "" ||
		strings.TrimSpace(labels["gpu-sku"]) != "" ||
		strings.TrimSpace(labels[runtopology.NodeLabelGPUClass]) != "" {
		return true
	}
	return false
}

func formatNodeSelector(selector map[string]string) string {
	if len(selector) == 0 {
		return "<none>"
	}
	parts := make([]string, 0, len(selector))
	for k, v := range selector {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
