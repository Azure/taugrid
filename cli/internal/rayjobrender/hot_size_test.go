// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rayjobrender

import "testing"

func rayPodSpecs(t *testing.T, doc map[string]any) map[string]map[string]any {
	t.Helper()
	cluster := doc["spec"].(map[string]any)["rayClusterSpec"].(map[string]any)
	specs := map[string]map[string]any{
		"head": cluster["headGroupSpec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any),
	}
	for _, item := range cluster["workerGroupSpecs"].([]any) {
		group := item.(map[string]any)
		specs["worker/"+group["groupName"].(string)] = group["template"].(map[string]any)["spec"].(map[string]any)
	}
	if len(specs) < 2 {
		t.Fatalf("expected head and worker pods, got %v", specs)
	}
	return specs
}

func tauHotEmptyDir(t *testing.T, role string, pod map[string]any) map[string]any {
	t.Helper()
	for _, item := range pod["volumes"].([]any) {
		v := item.(map[string]any)
		if v["name"] == "tau-hot" {
			return v["emptyDir"].(map[string]any)
		}
	}
	t.Fatalf("%s pod has no tau-hot volume: %+v", role, pod["volumes"])
	return nil
}

func TestRenderHotSizeSetsTauHotSizeLimitOnEveryPod(t *testing.T) {
	o := archiveOptions()
	o.HotSize = "500Gi"
	for role, pod := range rayPodSpecs(t, renderWithArchive(t, o)) {
		if got := tauHotEmptyDir(t, role, pod)["sizeLimit"]; got != "500Gi" {
			t.Fatalf("%s tau-hot sizeLimit = %v, want 500Gi", role, got)
		}
	}
}

func TestRenderWithoutHotSizeKeepsUnboundedTauHot(t *testing.T) {
	for role, pod := range rayPodSpecs(t, renderWithArchive(t, archiveOptions())) {
		if emptyDir := tauHotEmptyDir(t, role, pod); len(emptyDir) != 0 {
			t.Fatalf("%s tau-hot emptyDir = %+v, want no sizeLimit", role, emptyDir)
		}
	}
}
