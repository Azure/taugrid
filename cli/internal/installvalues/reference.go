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

	{"baselineQueue.enabled", fieldInfo{"bool", "true", "Create the baseline ClusterQueue that references KEC-managed ResourceFlavors"}},
	{"baselineQueue.name", fieldInfo{"string", "jobqueue", "LocalQueue name (must be a valid DNS label)"}},
	{"baselineQueue.namespaceSelector", fieldInfo{"object", "{matchExpressions: [{key: " + workloadmeta.LabelWorkspace + ", operator: Exists}]}", "Which namespaces receive the LocalQueue"}},
	{"baselineQueue.resources", fieldInfo{"list", "cpu:100000, memory:100Ti", "CPU/memory quotas bounding Kueue admission"}},
	{"baselineQueue.gpu.coveredResources", fieldInfo{"list", "nvidia.com/gpu", "GPU resource names covered by the node-resource group"}},
	{"baselineQueue.gpu.flavors", fieldInfo{"list", "[]", "KEC-generated GPU ResourceFlavor names and per-flavor quotas"}},

	{"kueue.*", fieldInfo{"", "", "Pass-through to the embedded Kueue chart (v0.18)"}},
	{"kuberay-operator.*", fieldInfo{"", "", "Pass-through to the embedded KubeRay chart (v1.6)"}},
	{"tau-core-controller.image.repository", fieldInfo{"string", "mcr.microsoft.com/aks/ai-runtime/tau-core-controller", "Controller image repository"}},
	{"taugrid-core.prewarm.enabled", fieldInfo{"bool", "false", "GPU image pre-pull DaemonSet"}},
	{"taugrid-core.stellar.enabled", fieldInfo{"bool", "false", "Stellar experiment dashboard"}},
	{"taugrid-core.lifecycleRecorder.enabled", fieldInfo{"bool", "false", "Run lifecycle recorder"}},
	{"taugrid-core.portal.enabled", fieldInfo{"bool", "true", "Unified operator observability portal"}},
	{"taugrid-core.portal.serviceAccount.create", fieldInfo{"bool", "true", "Create the Portal ServiceAccount in the umbrella distribution"}},
	{"taugrid-core.portal.rbac.create", fieldInfo{"bool", "true", "Grant the Portal read-only Kubernetes access in the umbrella distribution"}},
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
	sb.WriteString("KEC derives GPU ResourceFlavor names from live AKS node inventory. Add only\n")
	sb.WriteString("flavors that KEC has created in the cluster. TauGrid references them and does\n")
	sb.WriteString("not create or mutate their selectors, topology, taints, or tolerations.\n\n")
	sb.WriteString("  baselineQueue:\n")
	sb.WriteString("    gpu:\n")
	sb.WriteString("      flavors:\n")
	sb.WriteString("        - name: aks-h200-ndisr-v5\n")
	sb.WriteString("          resources:\n")
	sb.WriteString("            - name: nvidia.com/gpu\n")
	sb.WriteString("              nominalQuota: \"1\"\n")
	return sb.String()
}
