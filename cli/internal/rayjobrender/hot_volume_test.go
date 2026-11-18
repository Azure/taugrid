// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rayjobrender

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
)

func hotVolumeOptions(gpusPerWorker int) Options {
	o := archiveOptions()
	o.GPUsPerWorker = gpusPerWorker
	o.Profile.HotVolume = &profile.HotVolume{StorageClassName: "tau-hot", DefaultSize: "1Ti", MaxSize: "2Ti"}
	return o
}

func tauHotVolume(t *testing.T, role string, pod map[string]any) map[string]any {
	t.Helper()
	for _, item := range pod["volumes"].([]any) {
		v := item.(map[string]any)
		if v["name"] == "tau-hot" {
			return v
		}
	}
	t.Fatalf("%s pod has no tau-hot volume: %+v", role, pod["volumes"])
	return nil
}

func podFSGroup(pod map[string]any) (string, string) {
	context, _ := pod["securityContext"].(map[string]any)
	if context == nil || context["fsGroup"] == nil {
		return "", ""
	}
	policy, _ := context["fsGroupChangePolicy"].(string)
	return fmt.Sprint(context["fsGroup"]), policy
}

func TestRenderHotVolumeProfileGivesWorkersEphemeralClaims(t *testing.T) {
	tests := []struct {
		name          string
		gpusPerWorker int
		hotSize       string
		wantSize      string
		wantHeadLimit any
	}{
		{name: "GPU workers with default size", gpusPerWorker: 1, wantSize: "1Ti"},
		{name: "GPU workers with hot_size", gpusPerWorker: 8, hotSize: "500Gi", wantSize: "500Gi", wantHeadLimit: "500Gi"},
		{name: "CPU workers", wantSize: "1Ti"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			o := hotVolumeOptions(tt.gpusPerWorker)
			o.HotSize = tt.hotSize
			for role, pod := range rayPodSpecs(t, renderWithArchive(t, o)) {
				hot := tauHotVolume(t, role, pod)
				if role == "head" {
					emptyDir, ok := hot["emptyDir"].(map[string]any)
					if !ok || hot["ephemeral"] != nil || emptyDir["sizeLimit"] != tt.wantHeadLimit {
						t.Fatalf("head tau-hot = %+v, want emptyDir with sizeLimit %v", hot, tt.wantHeadLimit)
					}
					if group, _ := podFSGroup(pod); group != "" {
						t.Fatalf("head fsGroup = %q, want unset", group)
					}
					continue
				}
				if _, ok := hot["emptyDir"]; ok {
					t.Fatalf("%s tau-hot is an emptyDir on a hot-volume profile: %+v", role, hot)
				}
				ephemeral, ok := hot["ephemeral"].(map[string]any)
				if !ok {
					t.Fatalf("%s tau-hot is not an ephemeral volume: %+v", role, hot)
				}
				spec := ephemeral["volumeClaimTemplate"].(map[string]any)["spec"]
				want := map[string]any{
					"accessModes":      []any{"ReadWriteOnce"},
					"storageClassName": "tau-hot",
					"resources":        map[string]any{"requests": map[string]any{"storage": tt.wantSize}},
				}
				if !reflect.DeepEqual(spec, want) {
					t.Fatalf("%s tau-hot claim spec = %+v, want %+v", role, spec, want)
				}
				if group, policy := podFSGroup(pod); group != "65532" || policy != "OnRootMismatch" {
					t.Fatalf("%s fsGroup = %q (%q), want 65532 with OnRootMismatch", role, group, policy)
				}
			}
		})
	}
}

func TestRenderHotVolumeProfileKeepsWorkerFSGroupInRestrictedMode(t *testing.T) {
	o := hotVolumeOptions(1)
	o.SecurityMode = runconfig.SecurityModeRestricted
	for role, pod := range rayPodSpecs(t, renderWithArchive(t, o)) {
		if role == "head" {
			continue
		}
		if group, policy := podFSGroup(pod); group != "65532" || policy != "OnRootMismatch" {
			t.Fatalf("%s fsGroup = %q (%q), want 65532 with OnRootMismatch", role, group, policy)
		}
	}
}

func TestRenderHotVolumeProfileRejectsHotSizeAboveMaxSize(t *testing.T) {
	o := hotVolumeOptions(1)
	o.HotSize = "3Ti"
	if _, err := Render(o); err == nil || !strings.Contains(err.Error(), "exceeds the profile's hotVolume.maxSize 2Ti") {
		t.Fatalf("Render error = %v, want maxSize rejection", err)
	}
}

func TestRenderHotVolumeProfileRunsWorkerPreflightBeforeRayStart(t *testing.T) {
	for _, hot := range []bool{true, false} {
		t.Run(fmt.Sprintf("hot=%v", hot), func(t *testing.T) {
			o := hotVolumeOptions(1)
			if !hot {
				o.Profile.HotVolume = nil
			}
			cluster := renderWithArchive(t, o)["spec"].(map[string]any)["rayClusterSpec"].(map[string]any)
			templates := map[string]map[string]any{
				"head": cluster["headGroupSpec"].(map[string]any)["template"].(map[string]any),
			}
			for _, item := range cluster["workerGroupSpecs"].([]any) {
				templates["worker"] = item.(map[string]any)["template"].(map[string]any)
			}
			for role, template := range templates {
				wantOverride := hot && role == "worker"
				annotations, _ := template["metadata"].(map[string]any)["annotations"].(map[string]any)
				if got := annotations["ray.io/overwrite-container-cmd"] == "true"; got != wantOverride {
					t.Fatalf("%s overwrite-container-cmd = %v, want %v: %+v", role, got, wantOverride, annotations)
				}
				if wantOverride && annotations["adx-mon/scrape"] != "true" {
					t.Fatalf("%s lost pod annotations: %+v", role, annotations)
				}
				container := template["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
				command, _ := container["command"].([]any)
				if !wantOverride {
					if command != nil {
						t.Fatalf("%s command = %v, want KubeRay's generated command", role, command)
					}
				} else {
					if len(command) != 3 || command[0] != "bash" || command[1] != "-lc" {
						t.Fatalf("%s command = %v, want bash -lc script", role, command)
					}
					script := command[2].(string)
					preflight := strings.Index(script, "TAU_HOT_REQUIRE_LOCAL=1 forbids falling back")
					start := strings.Index(script, `exec bash -lc "$KUBERAY_GEN_RAY_START_CMD"`)
					if preflight < 0 || start < preflight || !strings.Contains(script, `ulimit -n "${RAY_START_ULIMIT_OPEN_FILES:-65536}"`) {
						t.Fatalf("%s script does not run the preflight before ray start:\n%s", role, script)
					}
				}
				require := ""
				for _, item := range container["env"].([]any) {
					if e := item.(map[string]any); e["name"] == "TAU_HOT_REQUIRE_LOCAL" {
						require = e["value"].(string)
					}
				}
				if want := map[bool]string{true: "1"}[wantOverride]; require != want {
					t.Fatalf("%s TAU_HOT_REQUIRE_LOCAL = %q, want %q", role, require, want)
				}
			}
		})
	}
}
