// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package serve

import (
	"fmt"
	"strconv"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/Azure/taugrid/core/envspec"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	RayServeNodesEnv = "TAU_SERVE_NODES"
	RayServeGPUsEnv  = "TAU_SERVE_GPUS_PER_NODE"
)

func validateRayWorkerOptions(p profile.Profile, o Options) error {
	if o.Workers < 0 {
		return fmt.Errorf("RayService worker count must be non-negative")
	}
	if o.Workers > 1 {
		if p.Resources.GPU.Count < 1 || p.Topology.Mode != profile.ModeFixed ||
			p.Topology.Placement != profile.PlacementMultiNodeNCCL ||
			p.ExecutionTarget != profile.ExecutionTargetSingleCluster {
			return fmt.Errorf("multi-node RayService requires a fixed, multi-node-nccl, singleCluster profile with at least one GPU per worker")
		}
		if len(o.Args) > 0 {
			return fmt.Errorf("multi-node RayService cannot use legacy --args for Ray startup; configure the Ray Serve application with --import-path and --env")
		}
	}
	if o.ShmSize == "" {
		return nil
	}
	size, err := resource.ParseQuantity(o.ShmSize)
	if err != nil || size.Sign() <= 0 {
		return fmt.Errorf("--shm-size must be a positive Kubernetes quantity, such as 32Gi")
	}
	for _, volume := range o.Volumes {
		if volume.Name == "tau-shm" {
			return fmt.Errorf("--shm-size conflicts with the tau-shm volume")
		}
	}
	for _, mount := range o.VolumeMounts {
		if mount.MountPath == "/dev/shm" {
			return fmt.Errorf("--shm-size conflicts with a mount at /dev/shm")
		}
	}
	return nil
}

func distributedRayEnvironment(env []envspec.Var, workers int32, gpus int) ([]envspec.Var, error) {
	values := map[string]string{
		RayServeNodesEnv: strconv.Itoa(int(workers)),
		RayServeGPUsEnv:  strconv.Itoa(gpus),
	}
	for _, variable := range env {
		if expected, reserved := values[variable.Name]; reserved &&
			(variable.ValueFrom != nil || variable.Value != expected) {
			return nil, fmt.Errorf("%s is supplied by the workload profile and must be %q", variable.Name, expected)
		}
	}
	return envspec.Merge(env, envspec.FromMap(values))
}

func copyResources(source map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range source {
		out[key] = value
	}
	return out
}

func addRaySharedMemory(pod, container map[string]any, o Options) {
	if o.ShmSize == "" {
		return
	}
	size := resource.MustParse(o.ShmSize)
	volumes := volumesToAny(o.Volumes)
	pod["volumes"] = append(volumes, map[string]any{
		"name":     "tau-shm",
		"emptyDir": map[string]any{"medium": "Memory", "sizeLimit": size.String()},
	})
	mounts := volumeMountsToAny(o.VolumeMounts)
	container["volumeMounts"] = append(mounts, map[string]any{
		"name": "tau-shm", "mountPath": "/dev/shm",
	})
}

func distributedRayCluster(p profile.Profile, o Options, image, rayVersion string, port int, env []envspec.Var, plan topology.Plan) map[string]any {
	template := func(head bool) map[string]any {
		labels := stringMapToAny(o.Labels)
		labels[workloadmeta.LabelService] = o.Name
		labels[workloadmeta.LabelProfile] = p.Name
		annotations := stringMapToAny(o.Annotations)
		gpu := p.Resources.GPU
		name := "ray-worker"
		if head {
			gpu = profile.GPUContract{Count: 0}
			name = "ray-head"
			annotations = stringMapToAny(topology.WithoutKueueTopologyAnnotations(o.Annotations))
		}
		for key, value := range gpu.Labels() {
			labels[key] = value
		}
		for key, value := range gpu.Annotations() {
			annotations[key] = value
		}
		for key, value := range plan.Labels {
			if !head || key != topology.LabelGPUClass {
				labels[key] = value
			}
		}
		if !head {
			for key, value := range plan.Annotations {
				annotations[key] = value
			}
		}
		ports := []any{map[string]any{"containerPort": int64(port), "name": "serve"}}
		if head {
			ports = append(ports,
				map[string]any{"containerPort": int64(DashboardPort), "name": "dashboard"},
				map[string]any{"containerPort": 6379, "name": "gcs-server"},
			)
		}
		container := map[string]any{
			"name": name, "image": image, "env": envspec.K8sList(env),
			"ports": ports,
		}
		if len(o.VolumeMounts) > 0 {
			container["volumeMounts"] = volumeMountsToAny(o.VolumeMounts)
		}
		pod := map[string]any{"containers": []any{container}}
		if len(o.Volumes) > 0 {
			pod["volumes"] = volumesToAny(o.Volumes)
		}
		if plan.PodPriorityClassName != "" {
			pod["priorityClassName"] = plan.PodPriorityClassName
		}
		if head {
			container["resources"] = map[string]any{
				"requests": map[string]any{"cpu": "1", "memory": "2Gi"},
				"limits":   map[string]any{"cpu": "2", "memory": "4Gi"},
			}
			pod["affinity"] = topology.SystemNodeAffinity()
			pod["tolerations"] = []any{
				map[string]any{"key": "CriticalAddonsOnly", "operator": "Exists", "effect": "NoSchedule"},
			}
		} else {
			requests, limits := copyResources(p.Resources.Requests), copyResources(p.Resources.Limits)
			if requests["cpu"] == nil {
				requests["cpu"] = "1"
			}
			if requests["memory"] == nil {
				requests["memory"] = "8Gi"
			}
			resources := map[string]any{"requests": requests, "limits": limits}
			profile.AddGPUResources(resources, gpu.Count)
			container["resources"] = resources
			pod["tolerations"] = []any{
				map[string]any{"key": "sku", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"},
				map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
			}
			if len(plan.NodeSelector) > 0 {
				pod["nodeSelector"] = stringMapToAny(plan.NodeSelector)
			}
			pod["affinity"] = map[string]any{
				"podAntiAffinity": map[string]any{
					"requiredDuringSchedulingIgnoredDuringExecution": []any{
						map[string]any{
							"topologyKey": "kubernetes.io/hostname",
							"labelSelector": map[string]any{"matchLabels": map[string]any{
								"ray.io/node-type": "worker", workloadmeta.LabelService: o.Name,
							}},
							"matchLabelKeys": []any{"ray.io/cluster"},
						},
					},
				},
			}
		}
		addRaySharedMemory(pod, container, o)
		metadata := map[string]any{"labels": labels}
		if len(annotations) > 0 {
			metadata["annotations"] = annotations
		}
		return map[string]any{"metadata": metadata, "spec": pod}
	}

	return map[string]any{
		"rayVersion": rayVersion,
		"headGroupSpec": map[string]any{
			"rayStartParams": map[string]any{
				"dashboard-host": "0.0.0.0", "num-cpus": "0", "num-gpus": "0",
			},
			"template": template(true),
		},
		"workerGroupSpecs": []any{
			map[string]any{
				"groupName": "gpu-workers",
				"replicas":  o.Workers, "minReplicas": o.Workers, "maxReplicas": o.Workers,
				"rayStartParams": map[string]any{"num-gpus": strconv.Itoa(p.Resources.GPU.Count)},
				"template":       template(false),
			},
		},
	}
}
