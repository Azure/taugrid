// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestShellSingleQuote(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"", "''"},
		{"foo", "'foo'"},
		{"/data/checkpoints/finetunes/run-1/metrics.json", "'/data/checkpoints/finetunes/run-1/metrics.json'"},
		{"a$b`c\\d", "'a$b`c\\d'"},
		{"it's", `'it'\''s'`},
		{"$(rm -rf /)", "'$(rm -rf /)'"},
		{"--field=$(injected)", "'--field=$(injected)'"},
	}
	for _, tc := range cases {
		if got := shellSingleQuote(tc.in); got != tc.want {
			t.Errorf("shellSingleQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHelperPodYAML_RoundTrip(t *testing.T) {
	out, err := helperPodYAML(helperPodSpec{
		Name:      "tau-get-foo-12345",
		Namespace: "team-a",
		LabelApp:  "tau-pvc-get",
		Image:     "busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662",
		PVCName:   "blob-training",
		TTLSec:    60,
		Script:    "cat 'foo'\n",
		Placement: helperPodPlacement{
			NodeSelector: map[string]string{"pool": "storage"},
			Affinity:     &helperPodAffinity{NodeAffinity: map[string]any{"preferredDuringSchedulingIgnoredDuringExecution": []any{}}},
			Tolerations:  []map[string]any{{"key": "storage", "operator": "Exists"}},
		},
	})
	if err != nil {
		t.Fatalf("helperPodYAML: %v", err)
	}
	var got helperPodObject
	if err := yaml.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-parse: %v\n%s", err, out)
	}
	if got.APIVersion != "v1" || got.Kind != "Pod" {
		t.Errorf("apiVersion/kind: %q/%q", got.APIVersion, got.Kind)
	}
	if got.Metadata.Name != "tau-get-foo-12345" || got.Metadata.Namespace != "team-a" {
		t.Errorf("metadata: %+v", got.Metadata)
	}
	if got.Spec.RestartPolicy != "Never" || got.Spec.ActiveDeadlineSeconds != 60 {
		t.Errorf("podSpec scalars: %+v", got.Spec)
	}
	if len(got.Spec.Containers) != 1 || got.Spec.Containers[0].Image != "busybox:1.36.1@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662" {
		t.Errorf("container: %+v", got.Spec.Containers)
	}
	if len(got.Spec.Volumes) != 1 || got.Spec.Volumes[0].PersistentVolumeClaim.ClaimName != "blob-training" {
		t.Errorf("volumes: %+v", got.Spec.Volumes)
	}
	if got.Spec.NodeSelector["pool"] != "storage" || got.Spec.Affinity == nil || len(got.Spec.Affinity.NodeAffinity) == 0 || len(got.Spec.Tolerations) != 1 {
		t.Errorf("placement: %+v", got.Spec)
	}
}

func TestHelperPodYAML_ResistsYAMLInjection(t *testing.T) {
	hostile := "cat /etc/passwd\nhostNetwork: true\nserviceAccountName: cluster-admin\nhostPID: true\n# trailing"
	out, err := helperPodYAML(helperPodSpec{
		Name:      "tau-ls-hostile",
		Namespace: "default",
		LabelApp:  "tau-pvc-list",
		Image:     pvcHelperImage,
		PVCName:   "blob-training",
		TTLSec:    60,
		Script:    hostile,
	})
	if err != nil {
		t.Fatalf("helperPodYAML: %v", err)
	}

	var typed helperPodObject
	if err := yaml.Unmarshal(out, &typed); err != nil {
		t.Fatalf("re-parse typed: %v\n%s", err, out)
	}
	if len(typed.Spec.Containers) != 1 || len(typed.Spec.Containers[0].Args) != 1 {
		t.Fatalf("want 1 container with 1 arg, got %+v", typed.Spec.Containers)
	}
	if !strings.Contains(typed.Spec.Containers[0].Args[0], "hostNetwork: true") {
		t.Errorf("hostile content not preserved in args: %q", typed.Spec.Containers[0].Args[0])
	}

	for _, line := range strings.Split(string(out), "\n") {
		for _, key := range []string{"hostNetwork:", "hostPID:", "hostIPC:", "serviceAccountName:", "automountServiceAccountToken:"} {
			for _, prefix := range []string{key, "  " + key, "    " + key} {
				if line == prefix || strings.HasPrefix(line, prefix+" ") {
					t.Errorf("privileged key %q leaked as a structural YAML field: %q", key, line)
				}
			}
		}
	}
}

func TestHelperPodYAMLForPVCUsesNamespaceOnlyFallbackWhenPVReadForbidden(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n team-a get pvc artifacts -o json":
			return `{"spec":{"volumeName":"pv-artifacts"},"status":{"phase":"Bound"}}`, nil
		case "get pv pv-artifacts -o json":
			return "", errors.New(`Error from server (Forbidden): persistentvolumes "pv-artifacts" is forbidden`)
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	raw, err := helperPodYAMLForPVC(context.Background(), runner, helperPodSpec{
		Name:      "tau-get-artifacts",
		Namespace: "team-a",
		LabelApp:  "tau-pvc-get",
		Image:     pvcHelperImage,
		PVCName:   "artifacts",
		TTLSec:    60,
		Script:    "true",
	})
	if err != nil {
		t.Fatalf("namespace-only fallback failed: %v", err)
	}
	var pod helperPodObject
	if err := yaml.Unmarshal(raw, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.NodeSelector != nil || pod.Spec.Affinity != nil || pod.Spec.Tolerations != nil {
		t.Fatalf("namespace-only fallback placement = %#v, want scheduler-managed placement", pod.Spec)
	}
}

func TestResolveHelperPodPlacementFallsBackWhenCSINodeListForbidden(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n team-a get pvc artifacts -o json":
			return `{"spec":{"volumeName":"pv-artifacts"},"status":{"phase":"Bound"}}`, nil
		case "get pv pv-artifacts -o json":
			return `{"spec":{"csi":{"driver":"file.csi.azure.com"}}}`, nil
		case "get csinode -o json":
			return "", errors.New(`Error from server (Forbidden): csinodes.storage.k8s.io is forbidden`)
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	got, err := resolveHelperPodPlacement(context.Background(), runner, "team-a", "artifacts")
	if err != nil {
		t.Fatalf("CSINode-denied fallback failed: %v", err)
	}
	if got.NodeSelector != nil || got.Affinity != nil || got.Tolerations != nil {
		t.Fatalf("CSINode-denied fallback placement = %#v, want scheduler-managed placement", got)
	}
}

func TestResolveHelperPodPlacementDropsProducerComputePlacementForPortableCSI(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n team-a get pvc artifacts -o json":
			return `{"spec":{"volumeName":"pv-artifacts"},"status":{"phase":"Bound"}}`, nil
		case "get pv pv-artifacts -o json":
			return `{"spec":{"csi":{"driver":"file.csi.azure.com"}}}`, nil
		case "get csinode -o json":
			return `{"items":[
				{"metadata":{"name":"cpu-storage"},"spec":{"drivers":[{"name":"file.csi.azure.com"}]}},
				{"metadata":{"name":"gpu-scaled-down"},"spec":{"drivers":null}}
			]}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})
	base := helperPodPlacement{
		NodeSelector: map[string]string{"kueue.azure.com/gpu-series": "ndm-a100-v4"},
		Tolerations: []map[string]any{
			{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
		},
	}

	raw, err := helperPodYAMLForPVC(context.Background(), runner, helperPodSpec{
		Name:      "tau-get-artifacts",
		Namespace: "team-a",
		LabelApp:  "tau-pvc-get",
		Image:     pvcHelperImage,
		PVCName:   "artifacts",
		TTLSec:    60,
		Script:    "true",
		Placement: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	var pod helperPodObject
	if err := yaml.Unmarshal(raw, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.NodeSelector != nil {
		t.Fatalf("helper inherited producer nodeSelector: %#v", pod.Spec.NodeSelector)
	}
	if pod.Spec.Tolerations != nil {
		t.Fatalf("helper inherited producer tolerations: %#v", pod.Spec.Tolerations)
	}
	nodeAffinity := pod.Spec.Affinity.NodeAffinity
	required := nodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	terms := required["nodeSelectorTerms"].([]any)
	term := terms[0].(map[string]any)
	fields := term["matchFields"].([]any)
	if len(fields) != 1 {
		t.Fatalf("matchFields = %#v, want one CSI compatibility requirement", fields)
	}
	requirement := fields[0].(map[string]any)
	if requirement["key"] != "metadata.name" || requirement["operator"] != "In" {
		t.Fatalf("requirement = %#v", requirement)
	}
	if got := requirement["values"].([]any); !reflect.DeepEqual(got, []any{"cpu-storage"}) {
		t.Fatalf("compatible nodes = %v, want [cpu-storage]", got)
	}
}

func TestResolveHelperPodPlacementUsesPVNodeAffinityForLocalStorage(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n team-a get pvc artifacts -o json":
			return `{"spec":{"volumeName":"pv-artifacts"},"status":{"phase":"Bound"}}`, nil
		case "get pv pv-artifacts -o json":
			return `{"spec":{"local":{"path":"/mnt/local"},"nodeAffinity":{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"kubernetes.io/hostname","operator":"In","values":["storage-node"]}]}]}}}}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	got, err := resolveHelperPodPlacement(context.Background(), runner, "team-a", "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	required := got.Affinity.NodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	terms := required["nodeSelectorTerms"].([]any)
	expressions := terms[0].(map[string]any)["matchExpressions"].([]any)
	if expressions[0].(map[string]any)["key"] != "kubernetes.io/hostname" {
		t.Fatalf("local PV affinity was not preserved: %#v", got)
	}
}

func TestResolveHelperPodPlacementRejectsUnsafeLocalStorageWithoutNodeAffinity(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n team-a get pvc artifacts -o json":
			return `{"spec":{"volumeName":"pv-artifacts"},"status":{"phase":"Bound"}}`, nil
		case "get pv pv-artifacts -o json":
			return `{"spec":{"hostPath":{"path":"/mnt/local"}}}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	_, err := resolveHelperPodPlacement(context.Background(), runner, "team-a", "artifacts")
	if err == nil || !strings.Contains(err.Error(), "node-local") || !strings.Contains(err.Error(), "node affinity") {
		t.Fatalf("unsafe local storage error = %v", err)
	}
}

func TestResolveHelperPodPlacementConstrainsHeterogeneousCSI(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n team-a get pvc artifacts -o json":
			return `{"spec":{"volumeName":"pv-artifacts"},"status":{"phase":"Bound"}}`, nil
		case "get pv pv-artifacts -o json":
			return `{"spec":{"csi":{"driver":"blob.csi.azure.com"}}}`, nil
		case "get csinode -o json":
			return `{"items":[
				{"metadata":{"name":"storage-b"},"spec":{"drivers":[{"name":"blob.csi.azure.com"}]}},
				{"metadata":{"name":"worker"},"spec":{"drivers":null}},
				{"metadata":{"name":"storage-a"},"spec":{"drivers":[{"name":"blob.csi.azure.com"}]}}
			]}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	got, err := resolveHelperPodPlacement(context.Background(), runner, "team-a", "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	required := got.Affinity.NodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	terms := required["nodeSelectorTerms"].([]any)
	if len(terms) != 2 {
		t.Fatalf("node selector terms = %d, want one OR term per compatible node", len(terms))
	}
	var nodeNames []string
	for _, rawTerm := range terms {
		fields := rawTerm.(map[string]any)["matchFields"].([]any)
		values := fields[0].(map[string]any)["values"].([]string)
		if len(values) != 1 {
			t.Fatalf("metadata.name values = %v, want exactly one Kubernetes-valid value", values)
		}
		nodeNames = append(nodeNames, values[0])
	}
	if !reflect.DeepEqual(nodeNames, []string{"storage-a", "storage-b"}) {
		t.Fatalf("compatible node terms = %v, want [storage-a storage-b]", nodeNames)
	}
}

func TestResolveHelperPodPlacementIntersectsPVTopologyWithCompatibleNodes(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n team-a get pvc artifacts -o json":
			return `{"spec":{"volumeName":"pv-artifacts"},"status":{"phase":"Bound"}}`, nil
		case "get pv pv-artifacts -o json":
			return `{"spec":{"csi":{"driver":"blob.csi.azure.com"},"nodeAffinity":{"required":{"nodeSelectorTerms":[{"matchExpressions":[{"key":"topology.kubernetes.io/zone","operator":"In","values":["zone-a"]}]},{"matchExpressions":[{"key":"topology.kubernetes.io/zone","operator":"In","values":["zone-b"]}]}]}}}}`, nil
		case "get csinode -o json":
			return `{"items":[
				{"metadata":{"name":"storage-b"},"spec":{"drivers":[{"name":"blob.csi.azure.com"}]}},
				{"metadata":{"name":"storage-a"},"spec":{"drivers":[{"name":"blob.csi.azure.com"}]}},
				{"metadata":{"name":"worker"},"spec":{"drivers":null}}
			]}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	got, err := resolveHelperPodPlacement(context.Background(), runner, "team-a", "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	required := got.Affinity.NodeAffinity["requiredDuringSchedulingIgnoredDuringExecution"].(map[string]any)
	terms := required["nodeSelectorTerms"].([]any)
	if len(terms) != 4 {
		t.Fatalf("node selector terms = %d, want 2 PV terms x 2 compatible nodes", len(terms))
	}
	var intersections []string
	for _, rawTerm := range terms {
		term := rawTerm.(map[string]any)
		expressions := term["matchExpressions"].([]any)
		zoneValues := expressions[0].(map[string]any)["values"].([]any)
		fields := term["matchFields"].([]any)
		nodeValues := fields[0].(map[string]any)["values"].([]string)
		if len(nodeValues) != 1 {
			t.Fatalf("metadata.name values = %v, want exactly one Kubernetes-valid value", nodeValues)
		}
		intersections = append(intersections, zoneValues[0].(string)+"/"+nodeValues[0])
	}
	want := []string{"zone-a/storage-a", "zone-a/storage-b", "zone-b/storage-a", "zone-b/storage-b"}
	if !reflect.DeepEqual(intersections, want) {
		t.Fatalf("PV/node intersections = %v, want %v", intersections, want)
	}
}

func TestResolveHelperPodPlacementLeavesUniversallyRegisteredCSIUnconstrained(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch strings.Join(args, " ") {
		case "-n team-a get pvc artifacts -o json":
			return `{"spec":{"volumeName":"pv-artifacts"},"status":{"phase":"Bound"}}`, nil
		case "get pv pv-artifacts -o json":
			return `{"spec":{"csi":{"driver":"file.csi.azure.com"}}}`, nil
		case "get csinode -o json":
			return `{"items":[{"metadata":{"name":"node-a"},"spec":{"drivers":[{"name":"file.csi.azure.com"}]}},{"metadata":{"name":"node-b"},"spec":{"drivers":[{"name":"file.csi.azure.com"}]}}]}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	got, err := resolveHelperPodPlacement(context.Background(), runner, "team-a", "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	if got.Affinity != nil {
		t.Fatalf("portable storage gained affinity: %#v", got.Affinity)
	}
}

func TestResolveHelperPodPlacementLeavesUnboundPVCForSchedulerBinding(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		if strings.Join(args, " ") != "-n team-a get pvc artifacts -o json" {
			t.Fatalf("unexpected kubectl args: %v", args)
		}
		return `{"spec":{"storageClassName":"portable"},"status":{"phase":"Pending"}}`, nil
	})

	got, err := resolveHelperPodPlacement(context.Background(), runner, "team-a", "artifacts")
	if err != nil {
		t.Fatal(err)
	}
	if got.Affinity != nil {
		t.Fatalf("unbound PVC gained affinity: %#v", got.Affinity)
	}
}

func TestWaitForHelperPodTerminalIgnoresMixedFailedSchedulingReasons(t *testing.T) {
	podReads := 0
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch {
		case len(args) > 1 && args[0] == "get" && args[1] == "-n":
			podReads++
			if podReads == 1 {
				return "Pending", nil
			}
			return "Succeeded", nil
		case len(args) > 1 && args[0] == "get" && args[1] == "events":
			return `{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/3 nodes are available: 1 didn't match Pod's node affinity/selector, 2 Insufficient cpu, 1 had untolerated taint"}]}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	phase, err := waitForHelperPodTerminal(context.Background(), runner, "team-a", "tau-ls-run", 3*time.Second)
	if err != nil || phase != "Succeeded" {
		t.Fatalf("mixed scheduler warning became terminal: phase=%q err=%v", phase, err)
	}
}

func TestWaitForHelperPodTerminalIgnoresStaleWarningAfterScheduling(t *testing.T) {
	podReads := 0
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch {
		case len(args) > 1 && args[0] == "get" && args[1] == "-n":
			podReads++
			switch podReads {
			case 1:
				return "Pending", nil
			case 2:
				return "Running", nil
			default:
				return "Succeeded", nil
			}
		case len(args) > 1 && args[0] == "get" && args[1] == "events":
			return `{"items":[{"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes are available: 1 didn't match Pod's node affinity/selector"}]}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	phase, err := waitForHelperPodTerminal(context.Background(), runner, "team-a", "tau-ls-run", 4*time.Second)
	if err != nil || phase != "Succeeded" {
		t.Fatalf("stale scheduler warning became terminal: phase=%q err=%v", phase, err)
	}
}

func TestWaitForHelperPodTerminalIncludesEventsAtTimeout(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch {
		case len(args) > 1 && args[0] == "get" && args[1] == "-n":
			return "Pending", nil
		case len(args) > 1 && args[0] == "get" && args[1] == "events":
			return `{"items":[{"type":"Warning","reason":"FailedMount","message":"driver name blob.csi.azure.com not found in the list of registered CSI drivers"},{"type":"Warning","reason":"FailedScheduling","message":"0/2 nodes are available: 2 Insufficient cpu"}]}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	start := time.Now()
	_, err := waitForHelperPodTerminal(context.Background(), runner, "team-a", "tau-ls-run", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "context deadline exceeded") || !strings.Contains(err.Error(), "FailedMount") || !strings.Contains(err.Error(), "FailedScheduling") {
		t.Fatalf("timeout diagnostic = %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("bounded timeout took %v", elapsed)
	}
}

func TestWaitForHelperPodTerminalUsesFailedPhaseAsTerminal(t *testing.T) {
	runner := rawRunnerFunc(func(_ context.Context, args []string, _ []byte) (string, error) {
		switch {
		case len(args) > 1 && args[0] == "get" && args[1] == "-n":
			return "Failed", nil
		case len(args) > 1 && args[0] == "get" && args[1] == "events":
			return `{"items":[{"type":"Warning","reason":"FailedMount","message":"mount failed after retries"}]}`, nil
		default:
			t.Fatalf("unexpected kubectl args: %v", args)
			return "", nil
		}
	})

	phase, err := waitForHelperPodTerminal(context.Background(), runner, "team-a", "tau-ls-run", 10*time.Second)
	if phase != "Failed" || err == nil || !strings.Contains(err.Error(), "FailedMount") {
		t.Fatalf("failed phase result: phase=%q err=%v", phase, err)
	}
}

func TestPVCHelperPodNamePreservesUniqueSuffixForLongRuns(t *testing.T) {
	runName := strings.Repeat("very-long-run-", 8)
	first, err := pvcHelperPodName("tau-get", runName)
	if err != nil {
		t.Fatal(err)
	}
	second, err := pvcHelperPodName("tau-get", runName)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) > 63 || len(second) > 63 {
		t.Fatalf("helper names exceed DNS limit: %d %d", len(first), len(second))
	}
	if first == second {
		t.Fatalf("helper names collided: %q", first)
	}
}

func TestDecodePVCListLogsRequiresCompletionMarker(t *testing.T) {
	_, err := decodePVCListLogs(
		"/data/tau-workspaces/default/chaos-horizon/w128",
		"blob-training",
		pvcHelperListCompleteMarker+":tau-ls-chaos",
		"",
	)
	if err == nil || !strings.Contains(err.Error(), "missing its completion marker") {
		t.Fatalf("missing-marker error = %v", err)
	}
}

func TestDecodePVCListLogsAcceptsCompletedZeroEntryListing(t *testing.T) {
	marker := pvcHelperListCompleteMarker + ":tau-ls-chaos"
	entries, err := decodePVCListLogs(
		"/data/tau-workspaces/default/chaos-horizon/w128",
		"blob-training",
		marker,
		marker+"\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want none", entries)
	}
}

func TestDecodePVCListLogsParsesEntriesBeforeCompletionMarker(t *testing.T) {
	marker := pvcHelperListCompleteMarker + ":tau-ls-chaos"
	encoded := func(value string) string {
		return base64.StdEncoding.EncodeToString([]byte(value))
	}
	entries, err := decodePVCListLogs(
		"/data/tau-workspaces/default/chaos-horizon/w128",
		"blob-training",
		marker,
		encoded("evidence_w128.json")+"\n"+
			encoded("rollout_w128.png")+"\n"+
			encoded("summary_w128.json")+"\n"+
			marker+"\n",
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"evidence_w128.json", "rollout_w128.png", "summary_w128.json"}
	if strings.Join(entries, "\n") != strings.Join(want, "\n") {
		t.Fatalf("entries = %v, want %v", entries, want)
	}
}

func TestPVCListHelperEnumeratesNestedDirectory(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"metrics.json":                    `{"loss":1.25}`,
		"checkpoint/metadata/_METADATA":   "metadata",
		"checkpoint/state/array_0/shard0": "weights",
	} {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	logs, stderr, err := runPVCListHelper(t, root, 0, nil, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"checkpoint",
		"checkpoint/metadata",
		"checkpoint/metadata/_METADATA",
		"checkpoint/state",
		"checkpoint/state/array_0",
		"checkpoint/state/array_0/shard0",
		"metrics.json",
	}
	if strings.Join(entries, "\n") != strings.Join(want, "\n") {
		t.Fatalf("entries:\n%q\nwant:\n%q", entries, want)
	}
}

func TestPVCListHelperKeepsModelAndDatasetListingsShallow(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"models/model-a/runs/run-1.json":         "{}",
		"datasets/fineweb/versions/v1/data.json": "{}",
	} {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, dir := range []string{"models", "datasets"} {
		listRoot := filepath.Join(root, dir)
		logs, stderr, err := runPVCListHelper(t, listRoot, 0, nil, false)
		if err != nil {
			t.Fatalf("%s helper failed: %v\nstderr:\n%s", dir, err, stderr)
		}
		entries, err := decodePVCListLogs(listRoot, "test-pvc", "test-complete", logs)
		if err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"models": "model-a", "datasets": "fineweb"}[dir]
		if strings.Join(entries, "\n") != want {
			t.Fatalf("%s entries = %q, want only direct child %q", dir, entries, want)
		}
	}
}

func TestPVCListHelperEnumeratesTrailingSlashPath(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs, stderr, err := runPVCListHelper(t, root+string(os.PathSeparator), 0, nil, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(entries, "\n") != "artifact.json" {
		t.Fatalf("entries = %q, want relative artifact.json", entries)
	}
}

func TestPVCListHelperAcceptsGenuinelyEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	logs, stderr, err := runPVCListHelper(t, root, 0, nil, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %q, want none", entries)
	}
}

func TestPVCListHelperRetriesAfterSuppressedInitialListing(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "artifact.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	realFind, err := exec.LookPath("find")
	if err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	findState := filepath.Join(t.TempDir(), "find-state")
	sleepState := filepath.Join(t.TempDir(), "sleep-state")
	fakeFind := filepath.Join(binDir, "find")
	findScript := `#!/bin/sh
if [ ! -e "$TAU_TEST_FIND_STATE" ]; then
  : >"$TAU_TEST_FIND_STATE"
  exit 0
fi
if [ ! -e "$TAU_TEST_SLEEP_STATE" ]; then
  exit 8
fi
exec "$TAU_TEST_REAL_FIND" "$@"
`
	if err := os.WriteFile(fakeFind, []byte(findScript), 0o700); err != nil {
		t.Fatal(err)
	}
	fakeSleep := filepath.Join(binDir, "sleep")
	if err := os.WriteFile(fakeSleep, []byte("#!/bin/sh\n: >\"$TAU_TEST_SLEEP_STATE\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(
		os.Environ(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"TAU_TEST_FIND_STATE="+findState,
		"TAU_TEST_SLEEP_STATE="+sleepState,
		"TAU_TEST_REAL_FIND="+realFind,
	)

	logs, stderr, err := runPVCListHelper(t, root, 12, env, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(entries, "\n") != "artifact.json" {
		t.Fatalf("entries = %q, want artifact.json", entries)
	}
}

func TestPVCListHelperRejectsDirectFilePath(t *testing.T) {
	file := filepath.Join(t.TempDir(), "metrics.json")
	if err := os.WriteFile(file, []byte(`{"loss":1.25}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, stderr, err := runPVCListHelper(t, file, 0, nil, true)
	if err == nil {
		t.Fatal("helper unexpectedly treated a direct file path as a directory")
	}
	if !strings.Contains(stderr, pvcHelperNotFoundMarker) {
		t.Fatalf("stderr = %q, want %s", stderr, pvcHelperNotFoundMarker)
	}
}

func TestPVCListHelperRejectsMissingPath(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	_, stderr, err := runPVCListHelper(t, missing, 0, nil, true)
	if err == nil {
		t.Fatal("helper unexpectedly accepted a missing path")
	}
	if !strings.Contains(stderr, pvcHelperNotFoundMarker) {
		t.Fatalf("stderr = %q, want %s", stderr, pvcHelperNotFoundMarker)
	}
}

func TestPVCListHelperPreservesSpecialFilenames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows filenames cannot contain the newline fixture")
	}
	root := t.TempDir()
	names := []string{
		" leading-space",
		"-leading-dash",
		"embedded\nnewline",
		"quote'and$dollar",
		"subdir/tab\tname",
	}
	for _, name := range names {
		file := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	logs, stderr, err := runPVCListHelper(t, root, 0, nil, true)
	if err != nil {
		t.Fatalf("helper failed: %v\nstderr:\n%s", err, stderr)
	}
	entries, err := decodePVCListLogs(root, "test-pvc", "test-complete", logs)
	if err != nil {
		t.Fatal(err)
	}
	want := append([]string(nil), names...)
	want = append(want, "subdir")
	sort.Strings(want)
	if strings.Join(entries, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("entries = %q, want %q", entries, want)
	}
}

func TestPVCListHelperReportsEnumerationFailure(t *testing.T) {
	binDir := t.TempDir()
	fakeFind := filepath.Join(binDir, "find")
	if err := os.WriteFile(fakeFind, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))

	_, stderr, err := runPVCListHelper(t, t.TempDir(), 0, env, true)
	if err == nil {
		t.Fatal("helper unexpectedly accepted a failed directory probe")
	}
	if !strings.Contains(stderr, pvcHelperListFailedMarker) {
		t.Fatalf("stderr = %q, want %s", stderr, pvcHelperListFailedMarker)
	}
}

func runPVCListHelper(t *testing.T, dir string, settleSeconds int, env []string, recursive bool) (string, string, error) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test executes a POSIX shell script")
	}
	cmd := exec.Command(
		"sh",
		"-c",
		pvcListHelperScript(recursive),
		"helper",
		dir,
		"test-complete",
		strconv.Itoa(settleSeconds),
	)
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}
