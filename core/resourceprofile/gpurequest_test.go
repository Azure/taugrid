package profile

import (
	"strings"
	"testing"
)

func devicePluginProfile() Profile {
	return Profile{
		Name: "dp",
		Spec: map[string]any{
			"resources": map[string]any{
				"gpu": map[string]any{
					"count":      1,
					"size":       "l",
					"placement":  "single-device",
					"requestVia": GPURequestDevicePlugin,
				},
				"requests": map[string]any{"cpu": "8", "memory": "32Gi"},
			},
		},
	}
}

func TestGPURequestPlanFromProfile_DevicePlugin(t *testing.T) {
	plan := GPURequestPlanFromProfile(devicePluginProfile())
	if plan.Mode != GPURequestDevicePlugin {
		t.Fatalf("mode = %q, want device-plugin", plan.Mode)
	}
	if plan.Count != 1 {
		t.Fatalf("count = %d, want 1", plan.Count)
	}
	if plan.ResourceName != defaultGPUResourceName {
		t.Fatalf("resourceName = %q, want %q", plan.ResourceName, defaultGPUResourceName)
	}
}

func TestGPURequestPlanFromProfile_DRABackCompat(t *testing.T) {
	p := Profile{Spec: map[string]any{
		"resources": map[string]any{
			"gpu": map[string]any{"count": 1},
			"dra": map[string]any{"claimTemplate": "full-gpu"},
		},
	}}
	plan := GPURequestPlanFromProfile(p)
	if plan.Mode != gpuRequestDRA {
		t.Fatalf("mode = %q, want dra (back-compat)", plan.Mode)
	}
	if plan.ClaimTemplate != "full-gpu" {
		t.Fatalf("claimTemplate = %q, want full-gpu", plan.ClaimTemplate)
	}
}

func TestGPURequestPlanFromProfile_DevicePluginIgnoresStaleDRA(t *testing.T) {
	p := devicePluginProfile()
	p.Spec["resources"].(map[string]any)["dra"] = map[string]any{"claimTemplate": "full-gpu"}
	plan := GPURequestPlanFromProfile(p)
	if plan.Mode != GPURequestDevicePlugin {
		t.Fatalf("mode = %q, want device-plugin even with stale dra", plan.Mode)
	}
}

// An inherited-but-ignored dra block is a profile-authoring smell, not a
// broken contract: device-plugin mode ignores it, so it must never block a
// render.
func TestValidateGPUContract_DevicePluginToleratesInheritedDRA(t *testing.T) {
	p := devicePluginProfile()
	p.Spec["resources"].(map[string]any)["dra"] = map[string]any{"claimTemplate": "full-gpu"}
	if err := validateGPUContract(p); err != nil {
		t.Fatalf("ignored inherited dra block must not be a hard error: %v", err)
	}
}

func TestGPURequestPlanFromProfile_ResourceNameOverride(t *testing.T) {
	p := devicePluginProfile()
	p.Spec["resources"].(map[string]any)["gpu"].(map[string]any)["resourceName"] = "nvidia.com/mig-1g.10gb"
	plan := GPURequestPlanFromProfile(p)
	if plan.ResourceName != "nvidia.com/mig-1g.10gb" {
		t.Fatalf("resourceName = %q, want override", plan.ResourceName)
	}
}

func TestGPURequestPlanFromProfile_NoGPU(t *testing.T) {
	p := Profile{Spec: map[string]any{"resources": map[string]any{
		"requests": map[string]any{"cpu": "2"},
	}}}
	plan := GPURequestPlanFromProfile(p)
	if plan.Mode != "" {
		t.Fatalf("mode = %q, want empty for CPU-only profile", plan.Mode)
	}
}

func TestApplyGPUResources_DevicePlugin(t *testing.T) {
	resources := map[string]any{"requests": map[string]any{"cpu": "8"}}
	claims, err := ApplyGPUResources(resources, GPURequestPlanFromProfile(devicePluginProfile()))
	if err != nil {
		t.Fatal(err)
	}
	if claims != nil {
		t.Fatalf("device-plugin must not return resourceClaims, got %v", claims)
	}
	reqs := resources["requests"].(map[string]any)
	lims := resources["limits"].(map[string]any)
	if reqs[defaultGPUResourceName] != 1 || lims[defaultGPUResourceName] != 1 {
		t.Fatalf("expected nvidia.com/gpu=1 in requests+limits: reqs=%v lims=%v", reqs, lims)
	}
	if reqs["cpu"] != "8" {
		t.Fatalf("existing cpu request must be preserved: %v", reqs)
	}
	if _, ok := resources["claims"]; ok {
		t.Fatalf("device-plugin must not set resources.claims: %v", resources)
	}
}

func TestApplyGPUResources_DevicePluginConflict(t *testing.T) {
	resources := map[string]any{"requests": map[string]any{defaultGPUResourceName: 2}}
	_, err := ApplyGPUResources(resources, GPURequestPlanFromProfile(devicePluginProfile()))
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("expected conflict error, got %v", err)
	}
}

func TestApplyGPUResources_DevicePluginZeroCount(t *testing.T) {
	p := devicePluginProfile()
	p.Spec["resources"].(map[string]any)["gpu"].(map[string]any)["count"] = 0
	_, err := ApplyGPUResources(map[string]any{}, GPURequestPlanFromProfile(p))
	if err == nil || !strings.Contains(err.Error(), "count > 0") {
		t.Fatalf("expected count>0 error, got %v", err)
	}
}

func TestApplyGPUResources_DRA(t *testing.T) {
	p := Profile{Spec: map[string]any{"resources": map[string]any{
		"gpu": map[string]any{"count": 1},
		"dra": map[string]any{"claimTemplate": "full-gpu"},
	}}}
	resources := map[string]any{}
	claims, err := ApplyGPUResources(resources, GPURequestPlanFromProfile(p))
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 1 {
		t.Fatalf("expected one pod resourceClaim, got %v", claims)
	}
	c := claims[0].(map[string]any)
	if c["resourceClaimTemplateName"] != "full-gpu" {
		t.Fatalf("expected claimTemplate full-gpu, got %v", c)
	}
	if _, ok := resources["claims"]; !ok {
		t.Fatalf("expected container resources.claims for DRA, got %v", resources)
	}
	if _, ok := resources["requests"]; ok {
		t.Fatalf("DRA path must not set device-plugin requests: %v", resources)
	}
}

func TestApplyGPUResources_NoGPUIsNoop(t *testing.T) {
	resources := map[string]any{"requests": map[string]any{"cpu": "2"}}
	claims, err := ApplyGPUResources(resources, GPURequestPlan{})
	if err != nil {
		t.Fatal(err)
	}
	if claims != nil {
		t.Fatalf("no-GPU plan must return nil claims, got %v", claims)
	}
	if _, ok := resources["limits"]; ok {
		t.Fatalf("no-GPU plan must not add limits: %v", resources)
	}
}

func TestValidateGPUContract_DevicePluginRequiresCount(t *testing.T) {
	p := Profile{Spec: map[string]any{"resources": map[string]any{
		"gpu": map[string]any{"size": "l", "requestVia": GPURequestDevicePlugin},
	}}}
	if err := validateGPUContract(p); err == nil {
		t.Fatal("expected error when device-plugin profile has no count")
	}
}

func TestValidateGPUContract_CountWithoutMechanism(t *testing.T) {
	p := Profile{Spec: map[string]any{"resources": map[string]any{
		"gpu": map[string]any{"count": 1, "size": "l", "placement": "single-device"},
	}}}
	if err := validateGPUContract(p); err == nil {
		t.Fatal("expected error when GPU count set but no request mechanism")
	}
}

func TestValidateGPUContract_DevicePluginValid(t *testing.T) {
	if err := validateGPUContract(devicePluginProfile()); err != nil {
		t.Fatalf("valid device-plugin profile should pass: %v", err)
	}
}

func migProfile() Profile {
	return Profile{
		Name: "mig",
		Spec: map[string]any{
			"resources": map[string]any{
				"gpu": map[string]any{
					"count":        1,
					"requestVia":   gpuRequestMIG,
					"resourceName": "nvidia.com/mig-1g.18gb",
				},
			},
		},
	}
}

func TestGPURequestPlanFromProfile_MIG(t *testing.T) {
	plan := GPURequestPlanFromProfile(migProfile())
	if plan.Mode != gpuRequestMIG {
		t.Fatalf("mode = %q, want mig", plan.Mode)
	}
	if plan.ResourceName != "nvidia.com/mig-1g.18gb" {
		t.Fatalf("resourceName = %q, want nvidia.com/mig-1g.18gb", plan.ResourceName)
	}
	if plan.Count != 1 {
		t.Fatalf("count = %d, want 1", plan.Count)
	}
}

func TestGPURequestPlanFromProfile_MIGSliceAlias(t *testing.T) {
	p := Profile{Spec: map[string]any{
		"resources": map[string]any{
			"gpu": map[string]any{
				"count":        2,
				"requestVia":   "mig-slice",
				"resourceName": "nvidia.com/mig-3g.71gb",
			},
		},
	}}
	plan := GPURequestPlanFromProfile(p)
	if plan.Mode != gpuRequestMIG {
		t.Fatalf("mode = %q, want mig (via mig-slice alias)", plan.Mode)
	}
}

func TestApplyGPUResources_MIG(t *testing.T) {
	resources := map[string]any{"requests": map[string]any{"cpu": "4"}}
	claims, err := ApplyGPUResources(resources, GPURequestPlanFromProfile(migProfile()))
	if err != nil {
		t.Fatal(err)
	}
	if claims != nil {
		t.Fatalf("MIG must not return resourceClaims, got %v", claims)
	}
	reqs := resources["requests"].(map[string]any)
	lims := resources["limits"].(map[string]any)
	if reqs["nvidia.com/mig-1g.18gb"] != 1 || lims["nvidia.com/mig-1g.18gb"] != 1 {
		t.Fatalf("expected nvidia.com/mig-1g.18gb=1 in requests+limits: reqs=%v lims=%v", reqs, lims)
	}
	if reqs["cpu"] != "4" {
		t.Fatalf("existing cpu request must be preserved: %v", reqs)
	}
}
