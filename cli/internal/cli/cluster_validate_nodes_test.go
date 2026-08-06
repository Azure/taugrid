package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestParseValidationOutput(t *testing.T) {
	raw := `nvidia_smi_rc=0
gpu_count=8
driver_version=550.90.07
nvlink_rc=0
nvlink_inactive=0
ecc_dbe=0
ib_total=8
ib_active=8`

	d := parseValidationOutput(raw)

	if d.NvidiaSMIRC != 0 {
		t.Errorf("NvidiaSMIRC=%d, want 0", d.NvidiaSMIRC)
	}
	if d.GPUCount != 8 {
		t.Errorf("GPUCount=%d, want 8", d.GPUCount)
	}
	if d.DriverVersion != "550.90.07" {
		t.Errorf("DriverVersion=%q, want 550.90.07", d.DriverVersion)
	}
	if d.NVLinkRC != 0 {
		t.Errorf("NVLinkRC=%d, want 0", d.NVLinkRC)
	}
	if d.NVLinkInactive != 0 {
		t.Errorf("NVLinkInactive=%d, want 0", d.NVLinkInactive)
	}
	if d.ECCErrors != 0 {
		t.Errorf("ECCErrors=%d, want 0", d.ECCErrors)
	}
	if d.IBTotal != 8 {
		t.Errorf("IBTotal=%d, want 8", d.IBTotal)
	}
	if d.IBActive != 8 {
		t.Errorf("IBActive=%d, want 8", d.IBActive)
	}
}

func TestParseValidationOutputEmpty(t *testing.T) {
	d := parseValidationOutput("")
	if d.NVLinkInactive != -1 {
		t.Errorf("NVLinkInactive=%d, want -1 for empty output", d.NVLinkInactive)
	}
}

func TestParseValidationOutputPartial(t *testing.T) {
	raw := `nvidia_smi_rc=0
gpu_count=4
extra_noise_line`

	d := parseValidationOutput(raw)
	if d.GPUCount != 4 {
		t.Errorf("GPUCount=%d, want 4", d.GPUCount)
	}
	if d.DriverVersion != "" {
		t.Errorf("DriverVersion=%q, want empty", d.DriverVersion)
	}
}

func TestAssessHealthy(t *testing.T) {
	d := validationData{
		NvidiaSMIRC:    0,
		GPUCount:       8,
		DriverVersion:  "550.90.07",
		NVLinkRC:       0,
		NVLinkInactive: 0,
		ECCErrors:      0,
		IBTotal:        8,
		IBActive:       8,
		HasOutput:      true,
	}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 8}
	res := assessHealth(d, node)

	if res.Status != statusHealthy {
		t.Errorf("Status=%v, want HEALTHY", res.Status)
	}
	if len(res.Reasons) != 0 {
		t.Errorf("Reasons=%v, want empty", res.Reasons)
	}
}

func TestAssessUnhealthyNvidiaSMI(t *testing.T) {
	d := validationData{NvidiaSMIRC: 1, HasOutput: true}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 8}
	res := assessHealth(d, node)

	if res.Status != statusUnhealthy {
		t.Errorf("Status=%v, want UNHEALTHY", res.Status)
	}
	if len(res.Reasons) != 1 || res.Reasons[0] != "nvidia-smi failed" {
		t.Errorf("Reasons=%v, want [nvidia-smi failed]", res.Reasons)
	}
}

func TestAssessUnhealthyGPUCount(t *testing.T) {
	d := validationData{
		NvidiaSMIRC: 0,
		GPUCount:    7,
		HasOutput:   true,
	}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 8}
	res := assessHealth(d, node)

	if res.Status != statusUnhealthy {
		t.Errorf("Status=%v, want UNHEALTHY", res.Status)
	}
}

func TestAssessUnhealthyNVLink(t *testing.T) {
	d := validationData{
		NvidiaSMIRC:    0,
		GPUCount:       8,
		NVLinkRC:       0,
		NVLinkInactive: 3,
		HasOutput:      true,
	}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 8}
	res := assessHealth(d, node)

	if res.Status != statusUnhealthy {
		t.Errorf("Status=%v, want UNHEALTHY", res.Status)
	}
	if res.NVLinkOK {
		t.Error("NVLinkOK=true, want false")
	}
}

func TestAssessDegradedECC(t *testing.T) {
	d := validationData{
		NvidiaSMIRC:    0,
		GPUCount:       8,
		NVLinkInactive: 0,
		ECCErrors:      2,
		IBTotal:        8,
		IBActive:       8,
		HasOutput:      true,
	}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 8}
	res := assessHealth(d, node)

	if res.Status != statusDegraded {
		t.Errorf("Status=%v, want DEGRADED", res.Status)
	}
}

func TestAssessDegradedIB(t *testing.T) {
	d := validationData{
		NvidiaSMIRC:    0,
		GPUCount:       8,
		NVLinkInactive: 0,
		ECCErrors:      0,
		IBTotal:        8,
		IBActive:       7,
		HasOutput:      true,
	}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 8}
	res := assessHealth(d, node)

	if res.Status != statusDegraded {
		t.Errorf("Status=%v, want DEGRADED", res.Status)
	}
	found := false
	for _, r := range res.Reasons {
		if strings.Contains(r, "IB") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected IB reason in %v", res.Reasons)
	}
}

func TestAssessNoIB(t *testing.T) {
	d := validationData{
		NvidiaSMIRC:    0,
		GPUCount:       8,
		NVLinkInactive: 0,
		ECCErrors:      0,
		IBTotal:        0,
		IBActive:       0,
		HasOutput:      true,
	}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 8}
	res := assessHealth(d, node)

	if res.Status != statusHealthy {
		t.Errorf("Status=%v, want HEALTHY (no IB expected)", res.Status)
	}
}

func TestAssessNoAllocGPU(t *testing.T) {
	d := validationData{
		NvidiaSMIRC: 0,
		GPUCount:    4,
		HasOutput:   true,
	}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 0}
	res := assessHealth(d, node)

	if res.Status != statusHealthy {
		t.Errorf("Status=%v, want HEALTHY when AllocGPU=0 (no expectation)", res.Status)
	}
}

func TestAssessNoOutputReturnsUnknown(t *testing.T) {
	d := validationData{}
	node := gpuNodeInfo{Name: "node1", AllocGPU: 8}
	res := assessHealth(d, node)

	if res.Status != statusUnknown {
		t.Errorf("Status=%v, want UNKNOWN when no output received", res.Status)
	}
	if res.PodError == "" {
		t.Error("PodError should be set when no output received")
	}
}

func TestHealthStatusString(t *testing.T) {
	tests := []struct {
		s    healthStatus
		want string
	}{
		{statusHealthy, "HEALTHY"},
		{statusDegraded, "DEGRADED"},
		{statusUnhealthy, "UNHEALTHY"},
		{statusUnknown, "UNKNOWN"},
	}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("%d.String()=%q, want %q", tt.s, got, tt.want)
		}
	}
}

func TestRunClusterValidateNodesFailsMinHealthyWhenNoNodes(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "nodes", "-o", "json"): `{"items":[]}`,
		},
		errors: map[string]error{},
	}
	var out strings.Builder
	err := runClusterValidateNodes(context.Background(), runner, validateNodesSpec{MinHealthy: 1}, &out, &strings.Builder{})
	if err == nil {
		t.Fatal("expected min-healthy to fail when no GPU nodes are found")
	}
	if !strings.Contains(err.Error(), "only 0 healthy nodes") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRunClusterValidateNodesAnyDoesNotAddClassSelector(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "nodes", "-o", "json"): `{"items":[]}`,
		},
		errors: map[string]error{},
	}
	err := runClusterValidateNodes(
		context.Background(),
		runner,
		validateNodesSpec{GPUClass: "any"},
		&strings.Builder{},
		&strings.Builder{},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range runner.calls {
		if containsString(call, "-l") {
			t.Fatalf("gpu_class any added a node selector: %v", call)
		}
	}
}

func TestRunClusterValidateNodesAnyRejectsClassSelector(t *testing.T) {
	runner := &fakeRawRunner{outputs: map[string]string{}, errors: map[string]error{}}
	err := runClusterValidateNodes(
		context.Background(),
		runner,
		validateNodesSpec{
			GPUClass: "any",
			Selector: workloadmeta.NodeLabelGPUClass + " in (a100-80gb,h100-95gb)",
		},
		&strings.Builder{},
		&strings.Builder{},
	)
	if err == nil || !strings.Contains(err.Error(), "unconstrained") {
		t.Fatalf("expected gpu_class any selector error, got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("conflicting class selector queried cluster: %v", runner.calls)
	}
}

func TestRunClusterValidateNodesNormalizesLegacyGPUClass(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "nodes", "-o", "json", "-l", workloadmeta.NodeLabelGPUClass+"=a100-80gb"): `{"items":[]}`,
		},
		errors: map[string]error{},
	}
	var errOut strings.Builder
	err := runClusterValidateNodes(
		context.Background(),
		runner,
		validateNodesSpec{GPUClass: "a100-nvlink-80gb"},
		&strings.Builder{},
		&errOut,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errOut.String(), `use "a100-80gb" instead`) {
		t.Fatalf("missing legacy warning: %q", errOut.String())
	}
}

func TestRunClusterValidateNodesRejectsUnknownGPUClassBeforeQuery(t *testing.T) {
	runner := &fakeRawRunner{outputs: map[string]string{}, errors: map[string]error{}}
	err := runClusterValidateNodes(
		context.Background(),
		runner,
		validateNodesSpec{GPUClass: "a100"},
		&strings.Builder{},
		&strings.Builder{},
	)
	if err == nil || !strings.Contains(err.Error(), "unsupported --gpu-class") {
		t.Fatalf("expected unsupported class error, got %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("invalid class queried cluster: %v", runner.calls)
	}
}

func TestRunClusterValidateNodesFailsOnUnhealthyByDefault(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "nodes", "-o", "json"): `{
				"items":[{
					"metadata":{"name":"gpu-node","labels":{"node.kubernetes.io/instance-type":"Standard_ND96isr_H200_v5"}},
					"status":{"allocatable":{"nvidia.com/gpu":"8"}}
				}]
			}`,
		},
		errors: map[string]error{
			fakeRawKey("create", "-f", "-", "-n", validateNamespace): errors.New("pod security denied"),
		},
	}
	var out strings.Builder
	err := runClusterValidateNodes(context.Background(), runner, validateNodesSpec{Timeout: 10}, &out, &strings.Builder{})
	if err == nil {
		t.Fatal("expected unhealthy/unknown validation result to fail by default")
	}
	if !strings.Contains(err.Error(), "node validation failed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidationPodNameIncludesUniqueSuffix(t *testing.T) {
	first := validationPodName("very-long-node-name-that-would-otherwise-overflow-kubernetes-pod-name-limits", 1)
	second := validationPodName("very-long-node-name-that-would-otherwise-overflow-kubernetes-pod-name-limits", 2)
	if first == second {
		t.Fatalf("expected unique pod names, got %q", first)
	}
	for _, name := range []string{first, second} {
		if len(name) > 63 {
			t.Fatalf("pod name %q length=%d, want <=63", name, len(name))
		}
		if !strings.HasPrefix(name, validatePodPrefix) {
			t.Fatalf("pod name %q missing prefix %q", name, validatePodPrefix)
		}
	}
}

func TestBuildValidationPodSpec(t *testing.T) {
	spec := buildValidationPodSpec("tau-validate-node1", "node1")
	if !strings.Contains(spec, `"nodeName":"node1"`) {
		t.Error("pod spec missing nodeName")
	}
	if !strings.Contains(spec, `"hostPID":true`) {
		t.Error("pod spec missing hostPID")
	}
	if !strings.Contains(spec, `"privileged":true`) {
		t.Error("pod spec missing privileged")
	}
	if !strings.Contains(spec, "nsenter") {
		t.Error("pod spec missing nsenter in script")
	}
	if !strings.Contains(spec, "nvidia-smi") {
		t.Error("pod spec missing nvidia-smi check")
	}
	if !strings.Contains(spec, "infiniband") {
		t.Error("pod spec missing IB check")
	}
}

func TestPrintNodeHealthTable(t *testing.T) {
	results := []nodeHealthResult{
		{
			Node: "node1", GPUCount: 8, AllocGPU: 8,
			NVLinkOK: true, IBTotal: 8, IBActive: 8,
			ECCErrors: 0, DriverVer: "550.90.07",
			Status: statusHealthy,
		},
		{
			Node: "node2", GPUCount: 8, AllocGPU: 8,
			NVLinkOK: true, IBTotal: 8, IBActive: 7,
			ECCErrors: 0, DriverVer: "550.90.07",
			Status: statusDegraded, Reasons: []string{"IB 7/8 active"},
		},
		{
			Node: "node3", GPUCount: 7, AllocGPU: 8,
			NVLinkOK: false, NVLinkDetail: "2 inactive",
			IBTotal: 8, IBActive: 8,
			ECCErrors: 1, DriverVer: "550.90.07",
			Status: statusUnhealthy, Reasons: []string{"GPU count 7 < expected 8"},
		},
	}

	var sb strings.Builder
	printNodeHealthTable(&sb, results)
	out := sb.String()

	if !strings.Contains(out, "NODE") {
		t.Error("missing header")
	}
	if !strings.Contains(out, "node1") {
		t.Error("missing node1")
	}
	if !strings.Contains(out, "HEALTHY") {
		t.Error("missing HEALTHY status")
	}
	if !strings.Contains(out, "DEGRADED") {
		t.Error("missing DEGRADED status")
	}
	if !strings.Contains(out, "UNHEALTHY") {
		t.Error("missing UNHEALTHY status")
	}
	if !strings.Contains(out, "8/8") {
		t.Error("missing GPU count 8/8")
	}
	if !strings.Contains(out, "7/8") {
		t.Error("missing partial count")
	}
	if !strings.Contains(out, "2 inactive") {
		t.Error("missing NVLink detail")
	}
}

func TestValidateNodesCLIHelp(t *testing.T) {
	out, err := runCluster(t, "validate", "nodes", "--help")
	if err != nil {
		t.Fatalf("--help failed: %v\n%s", err, out)
	}
	for _, want := range []string{"gpu-class", "min-healthy", "timeout", "context"} {
		if !strings.Contains(out, want) {
			t.Errorf("help missing flag %q", want)
		}
	}
}

func TestParseValidationOutputReadsMIGMode(t *testing.T) {
	d := parseValidationOutput("nvidia_smi_rc=0\ngpu_count=1\nmig_mode=Enabled\n")
	if d.MIGMode != "Enabled" {
		t.Fatalf("MIGMode = %q, want Enabled", d.MIGMode)
	}
}

func TestAssessHealthFlagsMIGEnabledWithNoInstancesAsUnhealthy(t *testing.T) {
	d := validationData{HasOutput: true, GPUCount: 1, MIGMode: "Enabled", MIGInstances: 0}
	res := assessHealth(d, gpuNodeInfo{Name: "n1", AllocGPU: 1})
	if res.Status != statusUnhealthy {
		t.Fatalf("status = %v, want unhealthy", res.Status)
	}
	if joined := strings.Join(res.Reasons, " "); !strings.Contains(joined, "MIG") {
		t.Fatalf("reasons = %q, want a MIG reason", joined)
	}
}

// MIG with instances configured is a supported way to run GPUs (profiles select
// it via requestVia: mig), so it must not be reported as a fault.
func TestAssessHealthAllowsMIGWithConfiguredInstances(t *testing.T) {
	d := validationData{HasOutput: true, GPUCount: 1, MIGMode: "Enabled", MIGInstances: 7}
	res := assessHealth(d, gpuNodeInfo{Name: "n1", AllocGPU: 1})
	if res.Status != statusHealthy {
		t.Fatalf("status = %v (%v), want healthy", res.Status, res.Reasons)
	}
}

func TestAssessHealthAllowsMIGDisabled(t *testing.T) {
	d := validationData{HasOutput: true, GPUCount: 1, MIGMode: "Disabled"}
	if res := assessHealth(d, gpuNodeInfo{Name: "n1", AllocGPU: 1}); res.Status != statusHealthy {
		t.Fatalf("status = %v, want healthy", res.Status)
	}
}

func TestLooksLikeGPUNodeMatchesAzureGPUSKUs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		labels       map[string]string
		instanceType string
		want         bool
	}{
		{"aks accelerator label", map[string]string{"kubernetes.azure.com/accelerator": "nvidia"}, "Standard_D4s_v5", true},
		{"nc series", map[string]string{}, "Standard_NC24ads_A100_v4", true},
		{"nd series", map[string]string{}, "Standard_ND96isr_H200_v5", true},
		{"cpu sku", map[string]string{}, "Standard_D4s_v5", false},
		{"no signal at all", map[string]string{}, "", false},
	} {
		if got := looksLikeGPUNode(tc.labels, tc.instanceType); got != tc.want {
			t.Errorf("%s: looksLikeGPUNode = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAdvertisesNVIDIAResourceCoversMIGAndOverrides(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alloc map[string]string
		want  bool
	}{
		{"whole gpu", map[string]string{"nvidia.com/gpu": "1"}, true},
		{"mig profile", map[string]string{"nvidia.com/mig-1g.10gb": "7"}, true},
		{"zero gpus", map[string]string{"nvidia.com/gpu": "0"}, false},
		{"cpu only", map[string]string{"cpu": "8"}, false},
	} {
		if got := advertisesNVIDIAResource(tc.alloc); got != tc.want {
			t.Errorf("%s: advertisesNVIDIAResource = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A DRA cluster publishes GPUs as ResourceSlices, not as nvidia.com/* extended
// resources, so a healthy DRA node advertises zero allocatable GPUs. It must
// not be reported as a missing device plugin.
func TestDiscoverStrandedGPUNodesIgnoresDRANodes(t *testing.T) {
	nodesJSON := `{"items":[{"metadata":{"name":"gpu-dra","labels":{"node.kubernetes.io/instance-type":"Standard_NC24ads_A100_v4"}},"status":{"allocatable":{"cpu":"24"}}},
	{"metadata":{"name":"gpu-stranded","labels":{"node.kubernetes.io/instance-type":"Standard_NC24ads_A100_v4"}},"status":{"allocatable":{"cpu":"24"}}}]}`

	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "nodes", "-o", "json"):          nodesJSON,
			fakeRawKey("get", "resourceslices", "-o", "json"): `{"items":[{"spec":{"nodeName":"gpu-dra"}}]}`,
		},
		errors: map[string]error{},
	}

	got, err := discoverStrandedGPUNodes(context.Background(), runner, "")
	if err != nil {
		t.Fatalf("discoverStrandedGPUNodes: %v", err)
	}
	if len(got) != 1 || got[0].Name != "gpu-stranded" {
		t.Fatalf("got %+v, want only gpu-stranded (the DRA node must be ignored)", got)
	}
}

// MIG advertises per-profile resources rather than nvidia.com/gpu; such a node
// is registered and must not be reported as stranded.
func TestDiscoverStrandedGPUNodesIgnoresMIGAdvertisedNodes(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "nodes", "-o", "json"): `{"items":[{"metadata":{"name":"gpu-mig","labels":{"node.kubernetes.io/instance-type":"Standard_NC24ads_A100_v4"}},"status":{"allocatable":{"nvidia.com/mig-1g.10gb":"7"}}}]}`,
		},
		errors: map[string]error{},
	}
	got, err := discoverStrandedGPUNodes(context.Background(), runner, "")
	if err != nil {
		t.Fatalf("discoverStrandedGPUNodes: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %+v, want none (MIG-advertised node is registered)", got)
	}
}

// --- GPU source detection -------------------------------------------------
//
// Researchers submit against device-plugin, DRA, or MIG (core/resourceprofile
// requestVia), so discovery has to account for all three. These tests pin the
// four properties that matter: device-plugin parity, DRA-only detection, no
// double counting when a node offers both, and a sane MIG count.

func nodeItemsFromJSON(t *testing.T, raw string) []nodeItem {
	t.Helper()
	var doc struct {
		Items []nodeItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	return doc.Items
}

func gpuNode(t *testing.T, name, alloc string) []nodeItem {
	t.Helper()
	return nodeItemsFromJSON(t, `{"items":[{"metadata":{"name":"`+name+
		`","labels":{"node.kubernetes.io/instance-type":"Standard_ND96isr_H200_v5"}},`+
		`"status":{"allocatable":{`+alloc+`}}}]}`)
}

// Device-plugin nodes must be counted exactly as before this change.
func TestClassifyGPUNodesCountsDevicePluginNodesUnchanged(t *testing.T) {
	got := classifyGPUNodes(gpuNode(t, "dp", `"nvidia.com/gpu":"8"`), nil)
	if len(got) != 1 {
		t.Fatalf("got %+v, want exactly one node", got)
	}
	if got[0].AllocGPU != 8 {
		t.Errorf("AllocGPU = %d, want 8", got[0].AllocGPU)
	}
	if got[0].Source != gpuSourceDevicePlugin {
		t.Errorf("Source = %q, want %q", got[0].Source, gpuSourceDevicePlugin)
	}
	if got[0].InstanceType != "Standard_ND96isr_H200_v5" {
		t.Errorf("InstanceType = %q, want the instance-type label", got[0].InstanceType)
	}
}

// A node with no GPUs at all is still skipped, and a zero-quantity
// nvidia.com/gpu is not a GPU node.
func TestClassifyGPUNodesSkipsNonGPUNodes(t *testing.T) {
	if got := classifyGPUNodes(gpuNode(t, "cpu", `"cpu":"8"`), nil); len(got) != 0 {
		t.Fatalf("got %+v, want none for a CPU node", got)
	}
	if got := classifyGPUNodes(gpuNode(t, "zero", `"nvidia.com/gpu":"0"`), nil); len(got) != 0 {
		t.Fatalf("got %+v, want none for nvidia.com/gpu=0", got)
	}
}

// A DRA-only node advertises no nvidia.com/* resource at all. Before this
// change it was skipped entirely, so an all-DRA cluster reported "no GPU nodes
// found" and failed --min-healthy on healthy hardware.
func TestClassifyGPUNodesDetectsDRAOnlyNodes(t *testing.T) {
	got := classifyGPUNodes(gpuNode(t, "dra", `"cpu":"96"`), map[string]int{"dra": 8})
	if len(got) != 1 {
		t.Fatalf("got %+v, want the DRA node to be detected", got)
	}
	if got[0].Source != gpuSourceDRA {
		t.Errorf("Source = %q, want %q", got[0].Source, gpuSourceDRA)
	}
	if got[0].AllocGPU != 8 {
		t.Errorf("AllocGPU = %d, want 8 DRA devices", got[0].AllocGPU)
	}
}

// A node running both a device plugin and a DRA driver must be counted once,
// from the device plugin, not 8+8.
func TestClassifyGPUNodesDoesNotDoubleCountDevicePluginAndDRA(t *testing.T) {
	got := classifyGPUNodes(gpuNode(t, "both", `"nvidia.com/gpu":"8"`), map[string]int{"both": 8})
	if len(got) != 1 {
		t.Fatalf("got %+v, want exactly one node", got)
	}
	if got[0].AllocGPU != 8 {
		t.Errorf("AllocGPU = %d, want 8 (counted once, not 16)", got[0].AllocGPU)
	}
	if got[0].Source != gpuSourceDevicePlugin {
		t.Errorf("Source = %q, want device-plugin to take precedence", got[0].Source)
	}
}

// MIG nodes advertise profile slices, never nvidia.com/gpu. They must be
// discovered (not silently skipped), and must not carry the raw slice total as
// a whole-GPU count, because that count is then compared against nvidia-smi.
func TestClassifyGPUNodesReportsMIGSlicesWithoutFakingWholeGPUs(t *testing.T) {
	got := classifyGPUNodes(gpuNode(t, "mig", `"nvidia.com/mig-1g.10gb":"7","nvidia.com/mig-2g.20gb":"2"`), nil)
	if len(got) != 1 {
		t.Fatalf("got %+v, want the MIG node to be discovered", got)
	}
	if got[0].Source != gpuSourceMIG {
		t.Errorf("Source = %q, want %q", got[0].Source, gpuSourceMIG)
	}
	if got[0].MIGSlices != 9 {
		t.Errorf("MIGSlices = %d, want 9 summed across profiles", got[0].MIGSlices)
	}
	if got[0].AllocGPU != 0 {
		t.Errorf("AllocGPU = %d, want 0: slice counts are not whole GPUs", got[0].AllocGPU)
	}
}

// The regression this guards: AllocGPU=9 vs nvidia-smi's 1 physical GPU would
// report a perfectly healthy MIG node as UNHEALTHY.
func TestAssessHealthDoesNotFlagMIGNodeOnSliceCountMismatch(t *testing.T) {
	node := classifyGPUNodes(gpuNode(t, "mig", `"nvidia.com/mig-1g.10gb":"7"`), nil)[0]
	d := validationData{HasOutput: true, GPUCount: 1, MIGMode: "Enabled", MIGInstances: 7}
	if res := assessHealth(d, node); res.Status != statusHealthy {
		t.Fatalf("status = %v (%v), want healthy", res.Status, res.Reasons)
	}
}

// A resourceName override for whole GPUs is not a MIG profile.
func TestClassifyGPUNodesTreatsNonMIGOverridesAsWholeGPUs(t *testing.T) {
	got := classifyGPUNodes(gpuNode(t, "override", `"nvidia.com/a100":"4"`), nil)
	if len(got) != 1 || got[0].Source != gpuSourceDevicePlugin || got[0].AllocGPU != 4 {
		t.Fatalf("got %+v, want one device-plugin node with 4 GPUs", got)
	}
}

func TestNodesWithResourceSlicesCountsDistinctGPUDevices(t *testing.T) {
	// Two slices for the same pool partition one node's devices; a repeated
	// device name across slices must not inflate the count.
	slices := `{"items":[
		{"spec":{"driver":"gpu.nvidia.com","nodeName":"n1","pool":{"name":"n1"},"devices":[{"name":"gpu-0"},{"name":"gpu-1"}]}},
		{"spec":{"driver":"gpu.nvidia.com","nodeName":"n1","pool":{"name":"n1"},"devices":[{"name":"gpu-1"},{"name":"gpu-2"}]}},
		{"spec":{"driver":"dra.net","nodeName":"n1","pool":{"name":"nics"},"devices":[{"name":"eth0"}]}},
		{"spec":{"driver":"gpu.nvidia.com","nodeName":"n2","pool":{"name":"n2"},"devices":[{"name":"gpu-0-mig-1g.10gb-0"},{"name":"gpu-0-mig-1g.10gb-1"}]}}
	]}`
	runner := &fakeRawRunner{
		outputs: map[string]string{fakeRawKey("get", "resourceslices", "-o", "json"): slices},
		errors:  map[string]error{},
	}
	got := nodesWithResourceSlices(context.Background(), runner)
	if got["n1"] != 3 {
		t.Errorf("n1 = %d, want 3 distinct GPU devices (deduped, NIC driver excluded)", got["n1"])
	}
	if got["n2"] != 1 {
		t.Errorf("n2 = %d, want 1: MIG devices collapse onto their parent GPU", got["n2"])
	}
}

// Clusters without resource.k8s.io, or without RBAC to list it, must degrade to
// extended-resource detection rather than failing the command.
func TestNodesWithResourceSlicesDegradesWhenAPIUnavailable(t *testing.T) {
	runner := &fakeRawRunner{outputs: map[string]string{}, errors: map[string]error{}}
	if got := nodesWithResourceSlices(context.Background(), runner); len(got) != 0 {
		t.Fatalf("got %+v, want empty when the API is unavailable", got)
	}
}

// End to end through the command: a DRA-only cluster must find its node instead
// of printing "no GPU nodes found".
func TestRunClusterValidateNodesDiscoversDRAOnlyCluster(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "nodes", "-o", "json"): `{"items":[{
				"metadata":{"name":"dra-node","labels":{"node.kubernetes.io/instance-type":"Standard_ND96isr_H200_v5"}},
				"status":{"allocatable":{"cpu":"96"}}}]}`,
			fakeRawKey("get", "resourceslices", "-o", "json"): `{"items":[{"spec":{"driver":"gpu.nvidia.com","nodeName":"dra-node","pool":{"name":"p"},"devices":[{"name":"gpu-0"}]}}]}`,
		},
		errors: map[string]error{
			fakeRawKey("create", "-f", "-", "-n", validateNamespace): errors.New("pod security denied"),
		},
	}
	var out strings.Builder
	err := runClusterValidateNodes(context.Background(), runner, validateNodesSpec{Timeout: 10}, &out, &strings.Builder{})
	if strings.Contains(out.String(), "no GPU nodes found") {
		t.Fatalf("DRA node was not discovered:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "found 1 GPU node") {
		t.Fatalf("expected 1 discovered GPU node, got:\n%s", out.String())
	}
	// The pod could not be created, so validation still fails; that is the
	// pre-existing behaviour and not what this test is about.
	if err == nil {
		t.Fatal("expected the failed validation pod to surface an error")
	}
}

// The single node fetch is shared between the inventory and the stranded
// report, so the command must not issue it twice.
func TestRunClusterValidateNodesFetchesNodesOnce(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "nodes", "-o", "json"): `{"items":[]}`,
		},
		errors: map[string]error{},
	}
	_ = runClusterValidateNodes(context.Background(), runner, validateNodesSpec{Timeout: 10}, &strings.Builder{}, &strings.Builder{})
	fetches := 0
	for _, call := range runner.calls {
		if fakeRawKey(call...) == fakeRawKey("get", "nodes", "-o", "json") {
			fetches++
		}
	}
	if fetches != 1 {
		t.Fatalf("get nodes issued %d times, want 1", fetches)
	}
}

// --- real cluster data ----------------------------------------------------
//
// Every other fixture in this file is hand-written, which only proves the
// classifier is self-consistent with data we invented. testdata/
// <cluster>-nodes.json is a trimmed capture of
// `kubectl get nodes -o json` from the <cluster> test cluster
// (2x A100 + 2x H200 GPU nodes, 5 system nodes), reduced to names, the labels
// detection reads, and allocatable capacity.
//
// It carries a shape none of the hand-written fixtures had: a real H200 node
// advertises nvidia.com/gpu: 8 *and* nvidia.com/mig-1g.18gb, mig-2g.35gb,
// mig-3g.71gb all at quantity "0". Nodes are only MIG-partitioned when a
// profile has non-zero capacity, so those zero entries must not push the node
// down the MIG arm, which would report AllocGPU 0 for a healthy 8-GPU node.
func TestClassifyGPUNodesAgainstRealClusterInventory(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "<cluster>-nodes.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	items := nodeItemsFromJSON(t, string(raw))
	if len(items) != 9 {
		t.Fatalf("fixture has %d nodes, want the captured 9", len(items))
	}

	// The cluster has no DRA drivers installed, which is itself the common
	// real-world case the detection has to stay correct for.
	got := classifyGPUNodes(items, nil)

	want := map[string]int{
		"aks-gpu-98828081-vmss000000": 8, // Standard_ND96amsr_A100_v4
		"aks-gpu-98828081-vmss000001": 8, // Standard_ND96amsr_A100_v4
		"flex-h200-eastus2euap-6m22l": 8, // Standard_ND96isr_H200_v5, zero-qty MIG keys
		"flex-h200-eastus2euap-c8r87": 8, // Standard_ND96isr_H200_v5
	}
	if len(got) != len(want) {
		t.Fatalf("classified %d GPU nodes, want %d: %+v", len(got), len(want), got)
	}
	total := 0
	for _, n := range got {
		expect, ok := want[n.Name]
		if !ok {
			t.Errorf("unexpected GPU node %q (system nodes must be excluded)", n.Name)
			continue
		}
		if n.AllocGPU != expect {
			t.Errorf("%s: AllocGPU = %d, want %d", n.Name, n.AllocGPU, expect)
		}
		if n.Source != gpuSourceDevicePlugin {
			t.Errorf("%s: Source = %q, want %q", n.Name, n.Source, gpuSourceDevicePlugin)
		}
		if n.MIGSlices != 0 {
			t.Errorf("%s: MIGSlices = %d, want 0: zero-quantity MIG profiles are not partitions", n.Name, n.MIGSlices)
		}
		total += n.AllocGPU
	}
	if total != 32 {
		t.Errorf("total GPUs = %d, want 32 (2x8 A100 + 2x8 H200)", total)
	}
}

// The same real capture must not produce a stranded-node warning: every GPU
// node on it has a registered device plugin, and the system nodes are not GPU
// hardware. A false warning here would train people to ignore a real one.
func TestClassifyStrandedGPUNodesFindsNoneOnHealthyRealCluster(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "<cluster>-nodes.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if got := classifyStrandedGPUNodes(nodeItemsFromJSON(t, string(raw)), nil); len(got) != 0 {
		t.Fatalf("got %+v, want no stranded nodes on a healthy cluster", got)
	}
}

// Zero-quantity MIG profiles alongside a real nvidia.com/gpu count, reduced to
// the property under test. Taken from flex-h200-eastus2euap-6m22l.
func TestCountNVIDIAResourcesIgnoresZeroQuantityMIGProfiles(t *testing.T) {
	whole, mig := countNVIDIAResources(map[string]string{
		"nvidia.com/gpu":         "8",
		"nvidia.com/mig-1g.18gb": "0",
		"nvidia.com/mig-2g.35gb": "0",
		"nvidia.com/mig-3g.71gb": "0",
	})
	if whole != 8 {
		t.Errorf("whole = %d, want 8", whole)
	}
	if mig != 0 {
		t.Errorf("mig = %d, want 0: a profile with no capacity is not a partition", mig)
	}
}

// Guards the fixture itself, not the classifier. The capture contains two H200
// nodes that look interchangeable, but flex-h200-eastus2euap-6m22l is the only
// real-world instance of a MIG-capable card running in whole-GPU mode:
// nvidia.com/gpu:8 alongside nvidia.com/mig-*:0. It is the sole evidence that
// precedence checks whole-GPU capacity before MIG profile keys. Pruning it as a
// duplicate would leave a MIG-first classifier -- which reports that node as 0
// GPUs -- passing every remaining test in this file.
//
// Asserting the property rather than the node count means trimming the fixture
// fails with a reason instead of an off-by-one someone can "fix" by editing 9.
func TestRealClusterFixtureRetainsMIGCapableWholeGPUNode(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "<cluster>-nodes.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	// Read allocatable directly rather than through countNVIDIAResources. This
	// test makes a claim about the fixture, so it must not fail when only the
	// production code changes -- otherwise a counting bug reports itself as a
	// missing fixture node and sends the reader to the wrong file.
	for _, it := range nodeItemsFromJSON(t, string(raw)) {
		wholeGPU, zeroedMIGProfile := false, false
		for name, qty := range it.Status.Allocatable {
			suffix, ok := strings.CutPrefix(name, "nvidia.com/")
			switch {
			case !ok:
			case strings.HasPrefix(suffix, "mig-"):
				if qty == "0" {
					zeroedMIGProfile = true
				}
			case qty != "0" && qty != "":
				wholeGPU = true
			}
		}
		if wholeGPU && zeroedMIGProfile {
			return // property preserved
		}
	}
	t.Fatal("fixture no longer contains a MIG-capable node running in whole-GPU mode " +
		"(nvidia.com/gpu > 0 with nvidia.com/mig-*: 0); restore it or the precedence " +
		"rule loses its only real-world coverage")
}
