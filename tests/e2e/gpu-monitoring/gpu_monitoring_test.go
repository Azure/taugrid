// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gpumonitoring

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/results"
)

func TestMain(m *testing.M) {
	code := m.Run()
	results.FlushAll()
	os.Exit(code)
}

const gpuMonitoringNamespace = "gpu-monitoring"

var gpuMonitoringSKUs = []string{
	"a10-2g",
	"a100",
	"a100-pcie-1g",
	"a100-pcie-2g",
	"a100-pcie-4g",
	"h100",
	"h100-nvl-1g",
	"h100-nvl-2g",
	"h100-nvl-confidential-1g",
	"h200",
	"gb200",
	"gb300",
	"spark",
}

var gpuMonitoringDaemonSets = func() []string {
	out := make([]string, len(gpuMonitoringSKUs))
	for i, sku := range gpuMonitoringSKUs {
		out[i] = "gpu-monitoring-" + sku
	}
	return out
}()

func TestGPUMonitoringDaemonSetExists(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())

	for _, name := range gpuMonitoringDaemonSets {
		err := tc.WaitForDaemonSet(gpuMonitoringNamespace, name, 2*time.Minute)
		require.NoError(t, err, "%s daemonset should exist", name)
	}
}

// TestGPUMonitoringCollectorConfigMaps verifies each per-SKU ConfigMap rendered
// by the chart contains the expected scrape targets (NPD, node-exporter, and
// either the managed GPU or GPU Operator DCGM endpoint) plus a non-empty rule
// set. Runs on a CPU-only CI cluster — the DaemonSet pods do not schedule but
// the ConfigMaps are installed.
func TestGPUMonitoringCollectorConfigMaps(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())
	ctx := context.Background()

	for _, sku := range gpuMonitoringSKUs {
		name := "gpu-monitoring-gpu-metrics-collector-" + sku
		t.Run(sku, func(t *testing.T) {
			cm, err := tc.KubeClient().CoreV1().ConfigMaps(gpuMonitoringNamespace).Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err, "ConfigMap %s should exist", name)

			rules, ok := cm.Data["rules.yaml"]
			require.True(t, ok, "ConfigMap %s should contain rules.yaml key", name)

			// NPD target is always present (npdScrape defaults true).
			require.Contains(t, rules, "node-problem-detector",
				"scrape targets should include node-problem-detector")

			// node-exporter present for all SKUs shipped today.
			require.Contains(t, rules, "node-exporter",
				"scrape targets should include node-exporter")

			// DCGM is either the managed host endpoint or the GPU Operator
			// Service. The runtime test verifies the Service uses
			// internalTrafficPolicy: Local before accepting it.
			require.Contains(t, rules, "dcgm-exporter",
				"scrape targets should include dcgm-exporter")
			require.True(t,
				strings.Contains(rules, "http://localhost:19400/metrics") ||
					strings.Contains(rules, "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics"),
				"SKU %s should use a supported node-local DCGM endpoint", sku)
			require.NotContains(t, rules, "http://localhost:9400/metrics",
				"SKU %s should not scrape :9400 — the bundled dcgm-exporter container was removed", sku)

			// The DCGM target must be required and publish its reachability as
			// a Node condition. Without it, losing the exporter silences every
			// DCGM rule and the node still looks healthy.
			require.Contains(t, rules, "availabilityCondition: DcgmExporterUnavailable",
				"SKU %s should publish DCGM scrape-target availability as a Node condition", sku)
			require.Regexp(t,
				`name: dcgm-exporter\s+url: \S+\s+required: true\s+availabilityCondition: DcgmExporterUnavailable`,
				rules,
				"SKU %s should mark the dcgm-exporter target required", sku)

			// Rules must be non-empty and include the opinionated default set.
			// These are the rule names we advertise as "opinionated defaults" that
			// customers can layer their own rules on top of. If any one of them
			// disappears it's a regression against the monitoring product claim —
			// update this list intentionally when the opinionated set changes.
			require.Contains(t, rules, "rules:", "ConfigMap %s should have a rules: section", name)
			opinionatedRuleNames := []string{
				// ECC (hardware failure signal)
				"ecc-dbe-retired",
				"ecc-dbe-volatile",
				// XID (driver/hardware critical codes)
				"xid-48", "xid-63", "xid-64", "xid-79", "xid-94", "xid-95",
				// NVLink (fabric health)
				"nvlink-crc-flit",
				"nvlink-crc-data",
				"nvlink-replay",
				// Thermal / power (imminent throttle or hardware stress)
				"thermal-violation",
				"power-violation",
				// InfiniBand (fabric health on multi-node SKUs)
				"ib-link-down",
			}
			for _, ruleName := range opinionatedRuleNames {
				require.Contains(t, rules, "name: "+ruleName,
					"opinionated default rule %q missing from ConfigMap %s; if intentional update the assertion list",
					ruleName, name)
			}
		})
	}
}

// TestGPUMonitoringDaemonSetHasCollectorSidecar verifies the metrics-collector
// sidecar is wired into every per-SKU DaemonSet with expected args and that
// the collector-config volume references the matching per-SKU ConfigMap.
// Also asserts the bundled dcgm-exporter container is NOT present — DCGM is
// consumed from the host systemd unit instead.
func TestGPUMonitoringDaemonSetHasCollectorSidecar(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())
	ctx := context.Background()

	for _, sku := range gpuMonitoringSKUs {
		dsName := "gpu-monitoring-" + sku
		wantCMName := "gpu-monitoring-gpu-metrics-collector-" + sku
		t.Run(sku, func(t *testing.T) {
			ds, err := tc.KubeClient().AppsV1().DaemonSets(gpuMonitoringNamespace).Get(ctx, dsName, metav1.GetOptions{})
			require.NoError(t, err, "DaemonSet %s should exist", dsName)

			var collector *string
			for i := range ds.Spec.Template.Spec.Containers {
				c := &ds.Spec.Template.Spec.Containers[i]
				require.NotEqual(t, "dcgm-exporter", c.Name,
					"DaemonSet %s should not ship a bundled dcgm-exporter container; DCGM comes from the host systemd unit on :19400", dsName)
				if c.Name == "metrics-collector" {
					argsJoined := strings.Join(c.Args, " ")
					require.Contains(t, argsJoined, "--config=/etc/npd-metrics-collector/rules.yaml",
						"metrics-collector missing --config arg on %s", dsName)
					require.Contains(t, argsJoined, "--scrape-interval=",
						"metrics-collector missing --scrape-interval arg on %s", dsName)

					// Sidecar must mount the collector-config volume at the
					// path it reads rules.yaml from.
					var sawMount bool
					for _, m := range c.VolumeMounts {
						if m.Name == "collector-config" && m.MountPath == "/etc/npd-metrics-collector" {
							sawMount = true
							break
						}
					}
					require.True(t, sawMount, "metrics-collector should mount collector-config at /etc/npd-metrics-collector on %s", dsName)
					collector = &c.Name
				}
			}
			require.NotNil(t, collector, "DaemonSet %s should contain a metrics-collector sidecar", dsName)

			// The collector-config pod volume should reference the per-SKU
			// ConfigMap, not a shared name.
			var cmRef string
			for _, v := range ds.Spec.Template.Spec.Volumes {
				if v.Name == "collector-config" && v.ConfigMap != nil {
					cmRef = v.ConfigMap.Name
					break
				}
			}
			require.Equal(t, wantCMName, cmRef,
				"collector-config volume on %s should reference %q (got %q)",
				dsName, wantCMName, cmRef)
		})
	}
}
