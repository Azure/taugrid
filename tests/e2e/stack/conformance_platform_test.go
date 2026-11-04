// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package stack

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConformancePlatformMutationRequiresExplicitMode(t *testing.T) {
	path, err := filepath.Abs(filepath.Join("harness", "fineweb_ib_conformance.sh"))
	require.NoError(t, err)
	dir := conformanceArtifactDir(t)
	script := filepath.Join(dir, "ib.sh")
	require.NoError(t, os.WriteFile(script, []byte("printf '%s\\n' IB_CALLED\n"), 0600))
	for _, prep := range []string{"assert-only", "direct-platform", "", "none", "$(unresolved)"} {
		for _, workload := range []string{"platform-direct", "manager", ""} {
			t.Run(prep+"/"+workload, func(t *testing.T) {
				cmd := exec.Command("bash", "-c", `
source "$1"
assert_platform_access_contract() { :; }
platform_context() { printf '%s\n' offline; }
platform_kubeconfig() { printf '%s\n' offline; }
enable_ib
`, "test", path)
				cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "IB_SCRIPT=" + script,
					"FINEWEB_PLATFORM_PREP_MODE=" + prep, "FINEWEB_WORKLOAD_ACCESS_MODE=" + workload,
					"GPU_SELECTOR_EXPR=example.com/accelerator=selected"}
				out, err := cmd.CombinedOutput()
				if prep == "direct-platform" && workload == "platform-direct" {
					require.NoError(t, err, "%s", out)
					require.Equal(t, 2, strings.Count(string(out), "IB_CALLED"))
				} else {
					require.Error(t, err, "%s", out)
					require.NotContains(t, string(out), "IB_CALLED")
				}
			})
		}
	}
}

func TestConformanceImageOverride(t *testing.T) {
	for _, script := range []string{"fineweb_ib_conformance.sh", "large_gpu_conformance.sh"} {
		t.Run(script, func(t *testing.T) {
			dir := conformanceArtifactDir(t)
			output := filepath.Join(dir, "outputs")
			image := "registry.example/ray@sha256:" + strings.Repeat("a", 64)
			cmd := exec.Command("bash", "harness/"+script, "resolve-ray-image")
			cmd.Env = append(os.Environ(), "GITHUB_OUTPUT="+output, "RAY_E2E_IMAGE="+image, "RAY_E2E_VERSION=2.53.0")
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "%s", out)
			got, err := os.ReadFile(output)
			require.NoError(t, err)
			require.Contains(t, string(got), "image="+image+"\n")
			require.Contains(t, string(got), "version=2.53.0\n")
		})
	}
}

func TestLargeGPUWorkloadOnlyCleanupRealPath(t *testing.T) {
	for _, c := range []struct {
		command            string
		crashed, forbidden bool
	}{
		{"run-conformance", false, false},
		{"run-conformance", true, false},
		{"run-conformance", false, true},
		{"cleanup-namespace", false, true},
	} {
		t.Run(fmt.Sprintf("%s/crash=%t/forbidden=%t", c.command, c.crashed, c.forbidden), func(t *testing.T) {
			dir := conformanceArtifactDir(t)
			config, log := filepath.Join(dir, "workload.yaml"), filepath.Join(dir, "calls")
			require.NoError(t, os.WriteFile(config, []byte("{}"), 0600))
			kubectl := `#!/bin/sh
	printf '%s\n' "$*" >> "$UNIT_KUBECTL_CALLS"
	case "$*" in
	  *"config view"*) printf '%s\n' '{"clusters":[{"cluster":{"server":"https://offline.invalid"}}],"users":[{"user":{}}]}' ;;
	  *"delete rayjob "*)
	    if [ "$UNIT_DELETE_FORBIDDEN" = "true" ]; then echo Forbidden >&2; exit 1; fi ;;
	  *) echo "unexpected kubectl call" >&2; exit 99 ;;
	esac
	`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "kubectl"), []byte(kubectl), 0700))
			goStub := `#!/bin/sh
	if [ "$UNIT_GO_CRASH" = "true" ]; then exit 17; fi
	printf '%s\n' '{"Action":"run","Test":"TestNanoGPTRayTrainLargeGPU"}' '{"Action":"pass","Test":"TestNanoGPTRayTrainLargeGPU"}' '{"Action":"pass"}'
	`
			require.NoError(t, os.WriteFile(filepath.Join(dir, "go"), []byte(goStub), 0700))
			cmd := exec.Command("bash", "harness/large_gpu_conformance.sh", c.command)
			for _, entry := range os.Environ() {
				if strings.HasPrefix(entry, "LARGE_GPU_PLATFORM_KUBECONFIG=") ||
					strings.HasPrefix(entry, "LARGE_GPU_PLATFORM_KUBE_CONTEXT=") ||
					strings.HasPrefix(entry, "DIRECT_PLATFORM_KUBECONFIG=") ||
					strings.HasPrefix(entry, "DIRECT_PLATFORM_CONTEXT=") {
					continue
				}
				cmd.Env = append(cmd.Env, entry)
			}
			cmd.Env = append(cmd.Env, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
				"UNIT_KUBECTL_CALLS="+log, fmt.Sprintf("UNIT_GO_CRASH=%t", c.crashed),
				fmt.Sprintf("UNIT_DELETE_FORBIDDEN=%t", c.forbidden),
				"LARGE_GPU_WORKLOAD_KUBECONFIG="+config, "LARGE_GPU_WORKLOAD_KUBE_CONTEXT=workload",
				"LARGE_GPU_WORKLOAD_ACCESS_MODE=platform-direct", "LARGE_GPU_PLATFORM_CLUSTER_IDENTITY=https://offline.invalid",
				"E2E_STACK_USE_ARGOCD_QUEUE=1", "E2E_STACK_NAMESPACE=shared",
				"E2E_STACK_QUEUE=jobqueue", "E2E_STACK_LARGE_GPU_QUEUE=jobqueue",
				"CONFORMANCE_GO_TEST_TIMEOUT=245m", "CONFORMANCE_ADMISSION_TIMEOUT=30m")
			out, err := cmd.CombinedOutput()
			if c.crashed || c.forbidden {
				require.Error(t, err, "%s", out)
			} else {
				require.NoError(t, err, "%s", out)
			}
			calls, err := os.ReadFile(log)
			require.NoError(t, err)
			require.Contains(t, string(calls), "--kubeconfig "+config+" --context workload --request-timeout=30s delete rayjob "+rayJobNameNanoGPT+" -n shared")
			require.Contains(t, string(calls), "--wait=true --timeout=4m --cascade=foreground")
		})
	}
}
