// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package jobrender

import (
	"strings"
	"testing"
)

func renderedPodSpec(t *testing.T, o Options) map[string]any {
	t.Helper()
	out, err := Render(trainProfile(), o)
	if err != nil {
		t.Fatal(err)
	}
	m := parseYAML(t, out)
	return m["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)
}

func podVolume(pod map[string]any, name string) map[string]any {
	volumes, _ := pod["volumes"].([]any)
	for _, item := range volumes {
		v := item.(map[string]any)
		if v["name"] == name {
			return v
		}
	}
	return nil
}

func containerMountPath(pod map[string]any, name string) string {
	c := pod["containers"].([]any)[0].(map[string]any)
	mounts, _ := c["volumeMounts"].([]any)
	for _, item := range mounts {
		m := item.(map[string]any)
		if m["name"] == name {
			return m["mountPath"].(string)
		}
	}
	return ""
}

func containerEnv(pod map[string]any, name string) (string, bool) {
	c := pod["containers"].([]any)[0].(map[string]any)
	env, _ := c["env"].([]any)
	for _, item := range env {
		e := item.(map[string]any)
		if e["name"] == name {
			value, _ := e["value"].(string)
			return value, true
		}
	}
	return "", false
}

func TestRender_HotSizeSetsTauHotSizeLimit(t *testing.T) {
	pod := renderedPodSpec(t, Options{
		Name:      "hot",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		PVCMount:  "blob-training",
		HotSize:   "500Gi",
	})
	hot := podVolume(pod, "tau-hot")
	if hot == nil {
		t.Fatalf("tau-hot volume missing: %+v", pod["volumes"])
	}
	emptyDir := hot["emptyDir"].(map[string]any)
	if emptyDir["sizeLimit"] != "500Gi" {
		t.Fatalf("tau-hot emptyDir = %+v, want sizeLimit 500Gi", emptyDir)
	}
	if got := containerMountPath(pod, "tau-hot"); got != "/mnt" {
		t.Fatalf("tau-hot mountPath = %q, want /mnt", got)
	}
}

func TestRender_HotSizeMountsTauHotWithoutDurableData(t *testing.T) {
	pod := renderedPodSpec(t, Options{
		Name:      "hot",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		HotSize:   "2Ti",
	})
	hot := podVolume(pod, "tau-hot")
	if hot == nil {
		t.Fatalf("tau-hot volume missing without /data: %+v", pod["volumes"])
	}
	if got := hot["emptyDir"].(map[string]any)["sizeLimit"]; got != "2Ti" {
		t.Fatalf("tau-hot sizeLimit = %v, want 2Ti", got)
	}
	if got := containerMountPath(pod, "tau-hot"); got != "/mnt" {
		t.Fatalf("tau-hot mountPath = %q, want /mnt", got)
	}
	if got, ok := containerEnv(pod, "TAU_HOT_DIR"); !ok || got != "/mnt" {
		t.Fatalf("TAU_HOT_DIR = %q (set=%v), want /mnt", got, ok)
	}
	if _, ok := containerEnv(pod, "TAU_DATA_DIR"); ok {
		t.Fatal("TAU_DATA_DIR advertised without durable data")
	}
	if podVolume(pod, "data") != nil {
		t.Fatal("hot_size must not add a data volume")
	}
}

func TestRender_NoHotSizeKeepsUnboundedTauHot(t *testing.T) {
	pod := renderedPodSpec(t, Options{
		Name:      "hot",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
		PVCMount:  "blob-training",
	})
	emptyDir := podVolume(pod, "tau-hot")["emptyDir"].(map[string]any)
	if _, ok := emptyDir["sizeLimit"]; ok {
		t.Fatalf("tau-hot gained a sizeLimit without storage.hot_size: %+v", emptyDir)
	}

	pod = renderedPodSpec(t, Options{
		Name:      "no-storage",
		Namespace: "tau",
		Command:   []string{"python", "train.py"},
	})
	if podVolume(pod, "tau-hot") != nil {
		t.Fatal("tau-hot mounted without /data or storage.hot_size")
	}
}

func TestRender_HotSizeRejectsCustomMntMount(t *testing.T) {
	_, err := Render(trainProfile(), Options{
		Name:         "hot",
		Namespace:    "tau",
		Command:      []string{"python", "train.py"},
		Volumes:      []Volume{{Name: "scratch", PVC: "scratch-pvc"}},
		VolumeMounts: []VolumeMount{{Name: "scratch", MountPath: "/mnt"}},
		HotSize:      "500Gi",
	})
	if err == nil || !strings.Contains(err.Error(), "storage.hot_size") {
		t.Fatalf("Render error = %v, want storage.hot_size conflict with custom /mnt mount", err)
	}
}
