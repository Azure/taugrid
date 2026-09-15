// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package serve

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/topology"
)

// The operator smoke uses the distributed renderer with CPU-only fixture
// resources. GPU allocation and cross-host placement remain unit-test contracts.
func TestRenderKindRayServiceFixture(t *testing.T) {
	p := distributedRayProfile()
	p.Topology.WorkloadPriorityClassName = "kind-serve-priority"
	p.Topology.PodPriorityClassName = "taugrid-train-default"
	p.Queue = "kind-cpu"
	options := Options{
		Name: "tau-kind-serve", Namespace: "ray",
		Image: "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0", RayVersion: "2.54.0",
		ImportPath: "serve_app:app", ServePort: 9000, Workers: 2, ShmSize: "256Mi",
		Env:          map[string]string{"PYTHONPATH": "/opt/tau-smoke"},
		Volumes:      []Volume{{Name: "app", ConfigMap: "tau-kind-serve-app"}},
		VolumeMounts: []VolumeMount{{Name: "app", MountPath: "/opt/tau-smoke", ReadOnly: true}},
	}
	raw, err := Render(p, options)
	if err != nil {
		t.Fatal(err)
	}
	object := decodeOne(t, raw)
	cluster := getPath(t, object, "spec", "rayClusterConfig").(map[string]any)
	headStart := getPath(t, cluster, "headGroupSpec", "rayStartParams").(map[string]any)
	headStart["object-store-memory"] = "134217728"
	head := getPath(t, cluster, "headGroupSpec", "template").(map[string]any)
	worker := cluster["workerGroupSpecs"].([]any)[0].(map[string]any)
	worker["rayStartParams"].(map[string]any)["num-gpus"] = "0"
	worker["rayStartParams"].(map[string]any)["object-store-memory"] = "134217728"
	workerTemplate := worker["template"].(map[string]any)
	for _, template := range []map[string]any{head, workerTemplate} {
		metadata := template["metadata"].(map[string]any)
		labels := metadata["labels"].(map[string]any)
		for key := range p.Resources.GPU.Labels() {
			delete(labels, key)
		}
		delete(labels, topology.LabelGPUClass)
		delete(metadata, "annotations")
		pod := template["spec"].(map[string]any)
		delete(pod, "nodeSelector")
		container := pod["containers"].([]any)[0].(map[string]any)
		if container["name"] == "ray-worker" {
			container["resources"] = map[string]any{
				"requests": map[string]any{"cpu": "50m", "memory": "512Mi"},
				"limits":   map[string]any{"cpu": "1", "memory": "2Gi"},
			}
		}
	}
	// Kind has one host; leave system affinity and tolerations on the head.
	delete(workerTemplate["spec"].(map[string]any), "affinity")
	metadata := object["metadata"].(map[string]any)
	delete(metadata, "annotations")
	for key := range p.Resources.GPU.Labels() {
		delete(metadata["labels"].(map[string]any), key)
	}
	raw, err = yaml.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	path := os.Getenv("TAU_KIND_RAYSERVICE_MANIFEST")
	if path == "" {
		path = filepath.Join(t.TempDir(), "rayservice.yaml")
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
}
