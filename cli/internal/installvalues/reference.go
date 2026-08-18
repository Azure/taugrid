// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package installvalues

import (
	"fmt"
	"strings"

	"github.com/Azure/taugrid/core/workloadmeta"
)

type fieldInfo struct {
	Type        string
	Default     string
	Description string
}

var catalog = []struct {
	Path string
	fieldInfo
}{
	{"components.kueue.enabled", fieldInfo{"bool", "true", "Install the Kueue job scheduler"}},
	{"components.kuberayOperator.enabled", fieldInfo{"bool", "true", "Install the KubeRay operator"}},
	{"components.tauCoreController.enabled", fieldInfo{"bool", "true", "Install the Tau core controller"}},
	{"components.taugridCore.enabled", fieldInfo{"bool", "true", "Install the taugrid-core services chart"}},
	{"components.gpuMonitoring.enabled", fieldInfo{"bool", "unset (follows tauCoreController)", "Install GPU node health monitoring; set explicitly only to override the controller linkage"}},

	{"baselineQueue.enabled", fieldInfo{"bool", "true", "Create the baseline ClusterQueue, LocalQueue, ResourceFlavor, and Topology"}},
	{"baselineQueue.name", fieldInfo{"string", "jobqueue", "LocalQueue name (must be a valid DNS label)"}},
	{"baselineQueue.namespaceSelector", fieldInfo{"object", "{matchExpressions: [{key: " + workloadmeta.LabelWorkspace + ", operator: Exists}]}", "Which namespaces receive the LocalQueue"}},
	{"baselineQueue.topology.enabled", fieldInfo{"bool", "true", "Create a Topology object for hostname-level scheduling"}},
	{"baselineQueue.topology.name", fieldInfo{"string", "default-node-topology", "Topology object name"}},
	{"baselineQueue.flavor.name", fieldInfo{"string", "taugrid-default-cpu", "CPU/memory ResourceFlavor name"}},
	{"baselineQueue.flavor.nodeLabels", fieldInfo{"map", "{kubernetes.io/os: linux}", "CPU/memory flavor node selector labels"}},
	{"baselineQueue.flavor.tolerations", fieldInfo{"list", "[]", "CPU/memory flavor tolerations; keep GPU taint tolerations out of this flavor"}},
	{"baselineQueue.resources", fieldInfo{"list", "cpu:100000, memory:100Ti", "CPU/memory quotas bounding Kueue admission"}},
	{"baselineQueue.gpu.enabled", fieldInfo{"bool", "true", "Add GPU resources and flavors to the node-resource group"}},
	{"baselineQueue.gpu.coveredResources", fieldInfo{"list", "nvidia.com/gpu", "GPU resource names covered by the node-resource group"}},
	{"baselineQueue.gpu.flavors", fieldInfo{"list", "taugrid-default-gpu (generic)", "GPU ResourceFlavors and per-flavor quotas; replace the generic flavor with exact class-labeled flavors when hardware is known"}},

	{"kueue.*", fieldInfo{"", "", "Pass-through to the embedded Kueue chart (v0.18)"}},
	{"kuberay-operator.*", fieldInfo{"", "", "Pass-through to the embedded KubeRay chart (v1.6)"}},
	{"tau-core-controller.platformNamespace", fieldInfo{"string", "tau-platform", "Namespace for TauWorkspace CRs"}},
	{"tau-core-controller.image.repository", fieldInfo{"string", "mcr.microsoft.com/aks/ai-runtime/tau-core-controller", "Controller image repository"}},
	{"tau-core-controller.tauCluster.nodeLabelRules", fieldInfo{"list", "reviewed AKS GPU catalog", "VM-size rules that reconcile gpu-class and gpu-series Node labels"}},
	{"tau-core-controller.tauCluster.extraNodeLabelRules", fieldInfo{"list", "[]", "Additional cluster-specific GPU label reconciliation rules"}},
	{"taugrid-core.prewarm.enabled", fieldInfo{"bool", "false", "GPU image pre-pull DaemonSet"}},
	{"taugrid-core.stellar.enabled", fieldInfo{"bool", "false", "Stellar experiment dashboard"}},
	{"taugrid-core.lifecycleRecorder.enabled", fieldInfo{"bool", "false", "Run lifecycle recorder"}},
	{"taugrid-core.portal.enabled", fieldInfo{"bool", "false", "Unified observability portal"}},
	{"gpu-monitoring.*", fieldInfo{"", "", "Pass-through to the embedded GPU monitoring chart"}},
}

func ReferenceMarkdown() string {
	var sb strings.Builder
	sb.WriteString("# TauGrid cluster install values\n\n")
	sb.WriteString("Values accepted by `tau cluster install --values <file>` and `--set`.\n")
	sb.WriteString("These quotas bound concurrent Kueue admission; Kubernetes scheduling\n")
	sb.WriteString("still enforces the cluster's real capacity.\n\n")
	sb.WriteString("| Field | Type | Default | Description |\n")
	sb.WriteString("| --- | --- | --- | --- |\n")
	for _, entry := range catalog {
		typStr := entry.Type
		if typStr == "" {
			typStr = "—"
		}
		defStr := entry.Default
		if defStr == "" {
			defStr = "—"
		}
		fmt.Fprintf(&sb, "| `%s` | %s | %s | %s |\n",
			entry.Path, typStr, defStr, entry.Description)
	}
	sb.WriteString("\n## Key: baselineQueue.gpu.flavors\n\n")
	sb.WriteString("Declare GPU node taints on GPU flavors so CPU-only pods cannot consume GPU\n")
	sb.WriteString("quota when generic CPU quota is exhausted. Tau GPU pods already tolerate the\n")
	sb.WriteString("standard sku=gpu and nvidia.com/gpu taints. Do not repeat a matching taint in\n")
	sb.WriteString("the flavor tolerations, because Kueue would bypass that admission guard.\n\n")
	sb.WriteString("  baselineQueue:\n")
	sb.WriteString("    gpu:\n")
	sb.WriteString("      flavors:\n")
	sb.WriteString("        - name: a100-pool\n")
	sb.WriteString("          nodeLabels:\n")
	fmt.Fprintf(&sb, "            %s: a100-80gb\n", workloadmeta.LabelGPUClass)
	sb.WriteString("          nodeTaints:\n")
	sb.WriteString("            - key: sku\n")
	sb.WriteString("              value: gpu\n")
	sb.WriteString("              effect: NoSchedule\n")
	sb.WriteString("          tolerations: []\n")
	sb.WriteString("          resources:\n")
	sb.WriteString("            - name: nvidia.com/gpu\n")
	sb.WriteString("              nominalQuota: \"1\"\n")
	return sb.String()
}
