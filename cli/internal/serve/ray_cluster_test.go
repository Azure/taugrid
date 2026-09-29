// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package serve

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/envspec"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func distributedRayProfile() profile.Profile {
	p := makeServeProfile()
	p.ExecutionTarget = profile.ExecutionTargetSingleCluster
	p.Topology.Mode = profile.ModeFixed
	p.Topology.Placement = profile.PlacementSameNetworkDomain
	p.Topology.PodPriorityClassName = "serve-priority"
	p.Topology.WorkloadPriorityClassName = "serve-priority"
	return p
}

func distributedRayOptions() Options {
	return Options{
		Name: "distributed-model", Namespace: "alpha",
		Image: "example.invalid/ray:fixture", RayVersion: "2.58.0",
		Workers: 8, ShmSize: "32Gi", ImportPath: "model_app:app",
		Replicas: 1, ReplicasSet: true,
		Volumes:      []Volume{{Name: "models", PVC: "model-weights"}},
		VolumeMounts: []VolumeMount{{Name: "models", MountPath: "/models", ReadOnly: true}},
		Env:          map[string]string{"MODEL_CONFIG": `{"context_length":1048576}`},
		EnvVars:      []envspec.Var{envspec.Secret("HF_TOKEN", "model-auth", "token")},
		Labels:       map[string]string{workloadmeta.LabelWorkspace: "research"},
		Annotations:  map[string]string{"tau.azure.com/workload-profile-source": "snapshot"},
	}
}

func TestRenderDistributedRayService(t *testing.T) {
	p, options := distributedRayProfile(), distributedRayOptions()
	raw, err := Render(p, options)
	if err != nil {
		t.Fatal(err)
	}
	documents := decodeAll(t, raw)
	if len(documents) != 1 || documents[0]["kind"] != "RayService" {
		t.Fatalf("expected only RayService, got:\n%s", raw)
	}
	object := documents[0]
	cluster := getPath(t, object, "spec", "rayClusterConfig").(map[string]any)
	head := cluster["headGroupSpec"].(map[string]any)
	headPod := getPath(t, head, "template", "spec").(map[string]any)
	headContainer := headPod["containers"].([]any)[0].(map[string]any)
	for _, field := range []string{"requests", "limits"} {
		if getPath(t, headContainer, "resources", field).(map[string]any)["nvidia.com/gpu"] != nil {
			t.Fatalf("CPU head must not consume a ninth GPU: %#v", headContainer)
		}
	}
	start := head["rayStartParams"].(map[string]any)
	if start["num-cpus"] != "0" || start["num-gpus"] != "0" {
		t.Fatalf("application actors must be scheduled on workers, not the CPU head: %#v", start)
	}
	wantHeadTolerations := []any{
		map[string]any{"key": "CriticalAddonsOnly", "operator": "Exists", "effect": "NoSchedule"},
	}
	if !reflect.DeepEqual(headPod["tolerations"], wantHeadTolerations) {
		t.Fatalf("CPU head must tolerate the system pool but not GPU-only taints: %#v", headPod["tolerations"])
	}
	if getPath(t, headPod, "affinity", "nodeAffinity") == nil {
		t.Fatal("CPU head must preserve the system-pool placement contract")
	}
	headAnnotations := getPath(t, head, "template", "metadata", "annotations").(map[string]any)
	if headAnnotations["kueue.x-k8s.io/podset-unconstrained-topology"] != nil {
		t.Fatal("GPU TAS annotations must not be applied to the CPU head")
	}
	groups := cluster["workerGroupSpecs"].([]any)
	if len(groups) != 1 {
		t.Fatalf("want one fixed GPU worker pool: %#v", groups)
	}
	group := groups[0].(map[string]any)
	for _, field := range []string{"replicas", "minReplicas", "maxReplicas"} {
		if group[field] != 8 {
			t.Fatalf("%s=%v, want 8", field, group[field])
		}
	}
	workerPod := getPath(t, group, "template", "spec").(map[string]any)
	workerContainer := workerPod["containers"].([]any)[0].(map[string]any)
	for _, field := range []string{"requests", "limits"} {
		if getPath(t, workerContainer, "resources", field).(map[string]any)["nvidia.com/gpu"] != 1 {
			t.Fatalf("each worker must allocate one GPU: %#v", workerContainer)
		}
	}
	if group["rayStartParams"].(map[string]any)["num-gpus"] != "1" {
		t.Fatal("Ray logical GPU count must match the device-plugin request")
	}
	annotations := getPath(t, group, "template", "metadata", "annotations").(map[string]any)
	if annotations["kueue.x-k8s.io/podset-required-topology"] != "tau.azure.com/network-domain" {
		t.Fatalf("worker topology contract missing: %#v", annotations)
	}
	spread := getPath(t, workerPod, "affinity", "podAntiAffinity",
		"requiredDuringSchedulingIgnoredDuringExecution").([]any)[0].(map[string]any)
	if !reflect.DeepEqual(spread["matchLabelKeys"], []any{"ray.io/cluster"}) {
		t.Fatalf("worker spreading must not couple old and new Ray clusters: %#v", spread)
	}
	for _, container := range []map[string]any{headContainer, workerContainer} {
		if container["command"] != nil || container["args"] != nil {
			t.Fatal("KubeRay must own ray start; the model entrypoint is a Serve import path")
		}
		values := map[string]any{}
		for _, rawEnv := range container["env"].([]any) {
			env := rawEnv.(map[string]any)
			values[env["name"].(string)] = env["value"]
			if env["name"] == "HF_TOKEN" && (env["valueFrom"] == nil || env["value"] != nil) {
				t.Fatal("secret references must not be resolved into direct values")
			}
		}
		if values[RayServeNodesEnv] != "8" || values[RayServeGPUsEnv] != "1" {
			t.Fatalf("missing authoritative worker geometry: %#v", values)
		}
	}
	for _, pod := range []map[string]any{headPod, workerPod} {
		volumes := pod["volumes"].([]any)
		shm := volumes[1].(map[string]any)["emptyDir"].(map[string]any)
		if shm["medium"] != "Memory" || shm["sizeLimit"] != "32Gi" {
			t.Fatalf("expected real tmpfs, not disk-backed emptyDir: %#v", shm)
		}
	}
	var application map[string]any
	if err := yaml.Unmarshal([]byte(getPath(t, object, "spec", "serveConfigV2").(string)), &application); err != nil {
		t.Fatal(err)
	}
	if application["http_options"].(map[string]any)["port"] != 8000 {
		t.Fatal("Serve HTTP listener must match the advertised container port")
	}
	app := application["applications"].([]any)[0].(map[string]any)
	if app["import_path"] != "model_app:app" ||
		app["deployments"].([]any)[0].(map[string]any)["num_replicas"] != 1 {
		t.Fatalf("expected one model replica, not eight unconstrained copies: %#v", app)
	}
	if p.Resources.Requests["nvidia.com/gpu"] != nil {
		t.Fatal("rendering mutated the caller's profile resource map")
	}
}

func TestDistributedRayServicePorts(t *testing.T) {
	for _, port := range []int{8000, 9000} {
		t.Run(fmt.Sprint(port), func(t *testing.T) {
			options := distributedRayOptions()
			options.ServePort = port
			raw, err := Render(distributedRayProfile(), options)
			if err != nil {
				t.Fatal(err)
			}
			object := decodeOne(t, raw)
			cluster := getPath(t, object, "spec", "rayClusterConfig").(map[string]any)
			head := getPath(t, cluster, "headGroupSpec", "template", "spec", "containers").([]any)[0].(map[string]any)
			group := cluster["workerGroupSpecs"].([]any)[0].(map[string]any)
			worker := getPath(t, group, "template", "spec", "containers").([]any)[0].(map[string]any)
			for _, container := range []map[string]any{head, worker} {
				ports, _ := container["ports"].([]any)
				namedPorts := map[string]any{}
				for _, item := range ports {
					value := item.(map[string]any)
					namedPorts[value["name"].(string)] = value["containerPort"]
				}
				if namedPorts["serve"] != port {
					t.Fatalf("%s Serve port = %v, want %d", container["name"], namedPorts["serve"], port)
				}
				if container["name"] == "ray-worker" && (namedPorts["dashboard"] != nil || namedPorts["gcs-server"] != nil) {
					t.Fatalf("worker must not advertise head-only ports: %#v", namedPorts)
				}
			}
			var config struct {
				HTTPOptions struct {
					Port int `yaml:"port"`
				} `yaml:"http_options"`
			}
			if err := yaml.Unmarshal([]byte(getPath(t, object, "spec", "serveConfigV2").(string)), &config); err != nil {
				t.Fatal(err)
			}
			if config.HTTPOptions.Port != port {
				t.Fatalf("HTTP port = %d, want %d", config.HTTPOptions.Port, port)
			}
		})
	}
}

func TestDistributedRayRejectsIncompatibleInputs(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*profile.Profile, *Options)
		want   string
	}{
		{"no GPU", func(p *profile.Profile, _ *Options) { p.Resources.GPU.Count = 0 }, "at least one GPU"},
		{"elastic", func(p *profile.Profile, _ *Options) { p.Topology.Mode = profile.ModeElastic }, "fixed"},
		{"placement", func(p *profile.Profile, _ *Options) { p.Topology.Placement = profile.PlacementUnconstrained }, "same-network-domain"},
		{"remote", func(p *profile.Profile, _ *Options) { p.ExecutionTarget = profile.ExecutionTargetMultiKueue }, "singleCluster"},
		{"raw launcher", func(_ *profile.Profile, o *Options) { o.Args = []string{"--use-ray"} }, "legacy --args"},
		{"bad shm", func(_ *profile.Profile, o *Options) { o.ShmSize = "0" }, "--shm-size"},
		{"shape override", func(_ *profile.Profile, o *Options) { o.Env[RayServeNodesEnv] = "4" }, "supplied by the workload profile"},
		{"shape secret", func(_ *profile.Profile, o *Options) {
			o.EnvVars = append(o.EnvVars, envspec.Secret(RayServeGPUsEnv, "shape", "gpu"))
		}, "supplied by the workload profile"},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, options := distributedRayProfile(), distributedRayOptions()
			test.change(&p, &options)
			out, err := Render(p, options)
			if err == nil || !strings.Contains(err.Error(), test.want) || len(out) > 0 {
				t.Fatalf("err=%v, want %q with no partial output", err, test.want)
			}
		})
	}
}

func TestRayApplicationReplicasDoNotChangeWorkerPool(t *testing.T) {
	options := distributedRayOptions()
	options.Replicas = 2
	raw, err := Render(distributedRayProfile(), options)
	if err != nil {
		t.Fatal(err)
	}
	cluster := getPath(t, decodeOne(t, raw), "spec", "rayClusterConfig").(map[string]any)
	if cluster["workerGroupSpecs"].([]any)[0].(map[string]any)["replicas"] != 8 {
		t.Fatal("application replica changes must not silently expand the authorized GPU pool")
	}
}

func TestRenderRayServiceMetadata(t *testing.T) {
	for _, shape := range []struct {
		name    string
		workers int32
		gpus    int
	}{
		{"CPU head only", 0, 0},
		{"single GPU", 1, 1},
		{"distributed GPU", 8, 1},
	} {
		for _, customAnnotations := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/custom-annotations=%t", shape.name, customAnnotations), func(t *testing.T) {
				p := makeServeProfile()
				p.Resources.GPU = profile.GPUContract{Count: shape.gpus}
				if shape.workers > 1 {
					p = distributedRayProfile()
				}
				options := Options{
					Name: "model", Namespace: "alpha", Image: "example.invalid/ray:fixture",
					Workers: shape.workers,
				}
				if customAnnotations {
					options.Annotations = map[string]string{"example.invalid/review": "kept"}
				}
				raw, err := Render(p, options)
				if err != nil {
					t.Fatal(err)
				}
				object := decodeOne(t, raw)
				for _, metadata := range []struct {
					name string
					path []string
					want bool
				}{
					{"root", []string{"metadata"}, customAnnotations || shape.gpus > 0},
					{"head", []string{"spec", "rayClusterConfig", "headGroupSpec", "template", "metadata"},
						customAnnotations || (shape.gpus > 0 && shape.workers <= 1)},
				} {
					value := getPath(t, object, metadata.path...).(map[string]any)
					annotations, exists := value["annotations"]
					if exists != metadata.want {
						t.Fatalf("%s annotations present=%t, want %t:\n%s", metadata.name, exists, metadata.want, raw)
					}
					if customAnnotations && annotations.(map[string]any)["example.invalid/review"] != "kept" {
						t.Fatalf("%s lost caller annotations: %#v", metadata.name, annotations)
					}
				}
			})
		}
	}
}
