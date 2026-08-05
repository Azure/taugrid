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

	{"baselineQueue.enabled", fieldInfo{"bool", "true", "Create the baseline ClusterQueue, LocalQueue, ResourceFlavor, and Topology"}},
	{"baselineQueue.name", fieldInfo{"string", "jobqueue", "LocalQueue name (must be a valid DNS label)"}},
	{"baselineQueue.namespaceSelector", fieldInfo{"object", "{matchExpressions: [{key: " + workloadmeta.LabelWorkspace + ", operator: Exists}]}", "Which namespaces receive the LocalQueue"}},
	{"baselineQueue.topology.enabled", fieldInfo{"bool", "true", "Create a Topology object for hostname-level scheduling"}},
	{"baselineQueue.topology.name", fieldInfo{"string", "default-node-topology", "Topology object name"}},
	{"baselineQueue.flavor.name", fieldInfo{"string", "taugrid-default", "ResourceFlavor name"}},
	{"baselineQueue.flavor.nodeLabels", fieldInfo{"map", "{kubernetes.io/os: linux}", "Node selector labels for the flavor"}},
	{"baselineQueue.flavor.tolerations", fieldInfo{"list", "[]", "Tolerations injected into workloads admitted through this flavor (critical for GPU taints)"}},
	{"baselineQueue.resources", fieldInfo{"list", "cpu:100000, memory:100Ti, nvidia.com/gpu:1000", "Resource quotas bounding Kueue admission"}},

	{"kueue.*", fieldInfo{"", "", "Pass-through to the embedded Kueue chart (v0.18)"}},
	{"kuberay-operator.*", fieldInfo{"", "", "Pass-through to the embedded KubeRay chart (v1.6)"}},
	{"tau-core-controller.platformNamespace", fieldInfo{"string", "tau-platform", "Namespace for TauWorkspace CRs"}},
	{"tau-core-controller.image.repository", fieldInfo{"string", "aksairuntime.azurecr.io/.../tau-core-controller", "Controller image repository"}},
	{"tau-core-controller.tauCluster.nodeLabelRules", fieldInfo{"list", "[]", "Node topology label reconciliation rules"}},
	{"taugrid-core.prewarm.enabled", fieldInfo{"bool", "false", "GPU image pre-pull DaemonSet"}},
	{"taugrid-core.stellar.enabled", fieldInfo{"bool", "false", "Stellar experiment dashboard"}},
	{"taugrid-core.lifecycleRecorder.enabled", fieldInfo{"bool", "false", "Run lifecycle recorder"}},
	{"taugrid-core.portal.enabled", fieldInfo{"bool", "false", "Unified observability portal"}},
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
	sb.WriteString("\n## Key: baselineQueue.flavor.tolerations\n\n")
	sb.WriteString("When the cluster uses GPU taints (e.g., sku=gpu:NoSchedule), Kueue excludes\n")
	sb.WriteString("tainted nodes while assigning flavors and the workload is never admitted. Tau\n")
	sb.WriteString("injects the sku=gpu and nvidia.com/gpu tolerations into the GPU workloads it\n")
	sb.WriteString("renders, and Kueue unions those with the flavor's; set this field so workloads\n")
	sb.WriteString("Tau does not render are admitted too.\n\n")
	sb.WriteString("  baselineQueue:\n")
	sb.WriteString("    flavor:\n")
	sb.WriteString("      tolerations:\n")
	sb.WriteString("        - key: sku\n")
	sb.WriteString("          value: gpu\n")
	sb.WriteString("          effect: NoSchedule\n")
	return sb.String()
}
