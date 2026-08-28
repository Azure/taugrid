// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Deployment-kind serving renderer — plain k8s Deployment that goes
// through Kueue via the Pod integration (not the RayService integration).
//
// Why this exists:
//   - Not every serving workload is a Ray app. Some inference services
//     (e.g. legacy TTS/LLM/STT stacks) run as plain Deployments + Kueue
//     pod integration:
//     annotation kueue.x-k8s.io/pod-suspending-parent: deployment
//     label      kueue.x-k8s.io/managed: "true" (on pod template)
//     label      kueue.x-k8s.io/queue-name: <localqueue> (Deployment + pod template)
//     The RayService renderer can't express this shape.
//
// V0 scope:
//   - Single main container. Optional sidecar via Options.Sidecars.
//   - Optional init containers via Options.InitContainers.
//   - Profile-driven CPU/memory and device-plugin GPU resources.
//   - Kueue pod-integration annotations + queue label wired correctly.
//
// V0 non-goals:
//   - PDB generation. Callers bring their own.
//   - Rolling-update strategy tuning. Uses Deployment defaults.
package serve

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// Container is a minimal container spec that callers can pass for
// sidecar / init slots. The main container is assembled from the
// profile + top-level DeploymentOptions.
type Container struct {
	Name         string            `yaml:"name"`
	Image        string            `yaml:"image"`
	Command      []string          `yaml:"command,omitempty"`
	Args         []string          `yaml:"args,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	Ports        []int             `yaml:"ports,omitempty"`
	VolumeMounts []VolumeMount     `yaml:"volumeMounts,omitempty"`
}

// VolumeMount mirrors k8s VolumeMount (subset).
type VolumeMount struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
	ReadOnly  bool   `yaml:"readOnly,omitempty"`
	SubPath   string `yaml:"subPath,omitempty"`
}

// Volume is a minimal volume: either emptyDir, configMap by name, secret
// by name, or PVC by claimName. Callers pick one.
type Volume struct {
	Name      string
	EmptyDir  bool
	ConfigMap string
	Secret    string
	PVC       string
}

// AutoscalingOptions configures horizontal pod autoscaling. When attached
// to DeploymentOptions the renderer emits an autoscaling/v2 HPA alongside
// the Deployment. When attached to Options (RayService) it injects
// autoscaling_config into the Ray Serve deployment.
type AutoscalingOptions struct {
	MinReplicas    int32 // >= 0; default 1
	MaxReplicas    int32 // > 0; required
	TargetQPS      int   // per-replica target QPS; 0 = CPU-only scaling
	ScaleDownDelay int   // scale-down stabilization window in seconds; default 300
}

func (a AutoscalingOptions) Validate() error {
	if a.MaxReplicas <= 0 {
		return errors.New("Autoscaling.MaxReplicas must be > 0")
	}
	if a.MinReplicas < 0 {
		return errors.New("Autoscaling.MinReplicas must be >= 0")
	}
	if a.MinReplicas > a.MaxReplicas {
		return errors.New("Autoscaling.MinReplicas must be <= MaxReplicas")
	}
	if a.TargetQPS < 0 {
		return errors.New("Autoscaling.TargetQPS must be >= 0")
	}
	if a.ScaleDownDelay < 0 {
		return errors.New("Autoscaling.ScaleDownDelay must be >= 0")
	}
	return nil
}

func (a AutoscalingOptions) WithDefaults() AutoscalingOptions {
	out := a
	if out.MinReplicas == 0 {
		out.MinReplicas = 1
	}
	if out.ScaleDownDelay == 0 {
		out.ScaleDownDelay = 300
	}
	return out
}

// HTTPProbe is a minimal HTTP GET probe for the main serve container.
type HTTPProbe struct {
	Path             string
	Port             int
	FailureThreshold int
}

// DeploymentOptions is the renderer input for the Deployment kind.
// Keeps the main-container options at the top level (name, image, args,
// ports) so simple cases stay one-liners, while init+sidecar live in
// the arrays for the multi-container shapes (fish-speech-tts etc.).
type DeploymentOptions struct {
	Name         string // required
	Namespace    string // required
	Image        string // overrides Profile.Runtime.Image
	Replicas     int32  // default 1
	Command      []string
	Args         []string
	Env          map[string]string
	EnvVars      []envspec.Var
	RuntimePip   []string
	Ports        []int
	VolumeMounts []VolumeMount
	Labels       map[string]string
	Annotations  map[string]string

	InitContainers []Container
	Sidecars       []Container
	Volumes        []Volume

	ReadinessProbe HTTPProbe
	StartupProbe   HTTPProbe
	LivenessProbe  HTTPProbe

	ServicePort       int
	ServiceTargetPort int

	// KueueManaged toggles the Kueue pod-integration annotations +
	// "managed" label on the pod template. Default true (the whole
	// point of this renderer is Kueue-admitted serving).
	KueueManaged *bool

	// Autoscaling, when non-nil, renders an autoscaling/v2 HPA
	// alongside the Deployment and omits spec.replicas so the HPA
	// owns the replica count.
	Autoscaling *AutoscalingOptions
}

// RenderDeployment turns a resolved Profile + DeploymentOptions into a
// single-doc YAML Deployment manifest ready for `kubectl apply -f -`.
func RenderDeployment(p profile.Profile, o DeploymentOptions) ([]byte, error) {
	if err := o.validateDeployment(); err != nil {
		return nil, err
	}

	mainImage, err := resolveDeploymentImage(p, o)
	if err != nil {
		return nil, err
	}
	runtimePip, err := resolveRuntimePip(o.RuntimePip)
	if err != nil {
		return nil, err
	}
	if len(runtimePip) > 0 {
		return nil, fmt.Errorf("runtime.pip is only supported for --kind=rayservice; for --kind=deployment bake dependencies into the image or profile runtime image")
	}
	replicas := int64(o.Replicas)
	if replicas <= 0 {
		replicas = 1
	}
	kueueManaged := true
	if o.KueueManaged != nil {
		kueueManaged = *o.KueueManaged
	}
	gpu := p.Resources.GPU
	gpuLabels := gpu.Labels()
	gpuAnnotations := gpu.Annotations()

	// Kueue's Deployment webhook writes the suspending-parent annotation, the
	// managed label, and queue-name as one set, and only when it has a queue
	// name. This renderer writes them itself, so it has to keep them together:
	// the annotation alone makes Kueue's Pod webhook gate the pod before it
	// ever consults queue-name, and without a queue there is no LocalQueue to
	// admit it out of that gate. Emitting a partial set produces pods that are
	// SchedulingGated forever with no Workload object to diagnose.
	queueName := profileLocalQueue(p)
	if kueueManaged && queueName == "" {
		return nil, errors.New("render Kueue-managed Deployment: Kueue LocalQueue is required")
	}

	deployLabels := stringMapToAny(o.Labels)
	deployLabels["app"] = o.Name
	deployLabels[workloadmeta.LabelService] = o.Name
	deployLabels[workloadmeta.LabelProfile] = p.Name
	podLabels := stringMapToAny(o.Labels)
	podLabels["app"] = o.Name
	podLabels[workloadmeta.LabelService] = o.Name
	podLabels[workloadmeta.LabelProfile] = p.Name
	for k, v := range gpuLabels {
		deployLabels[k] = v
		podLabels[k] = v
	}
	rootAnnotations := stringMapToAny(o.Annotations)
	podAnnotations := stringMapToAny(o.Annotations)
	for k, v := range gpuAnnotations {
		rootAnnotations[k] = v
		podAnnotations[k] = v
	}
	if kueueManaged {
		deployLabels["kueue.x-k8s.io/queue-name"] = queueName
		podLabels["kueue.x-k8s.io/queue-name"] = queueName
		podLabels["kueue.x-k8s.io/managed"] = "true"
		podAnnotations["kueue.x-k8s.io/pod-suspending-parent"] = "deployment"
	}

	// Main container.
	mainContainer := map[string]any{
		"name":  "main",
		"image": mainImage,
	}
	if len(o.Command) > 0 {
		mainContainer["command"] = stringsToAny(o.Command)
	}
	if len(o.Args) > 0 {
		mainContainer["args"] = stringsToAny(o.Args)
	}
	if len(o.Ports) > 0 {
		var ports []any
		for _, p := range o.Ports {
			ports = append(ports, map[string]any{"containerPort": int64(p)})
		}
		mainContainer["ports"] = ports
	}
	if probe := probeToAny(o.ReadinessProbe, o.defaultProbePort()); len(probe) > 0 {
		mainContainer["readinessProbe"] = probe
	}
	if probe := probeToAny(o.StartupProbe, o.defaultProbePort()); len(probe) > 0 {
		mainContainer["startupProbe"] = probe
	}
	if probe := probeToAny(o.LivenessProbe, o.defaultProbePort()); len(probe) > 0 {
		mainContainer["livenessProbe"] = probe
	}
	env, err := envspec.Merge(envspec.FromMap(o.Env), o.EnvVars)
	if err != nil {
		return nil, err
	}
	if len(env) > 0 {
		mainContainer["env"] = envspec.K8sList(env)
	}
	if len(o.VolumeMounts) > 0 {
		mainContainer["volumeMounts"] = volumeMountsToAny(o.VolumeMounts)
	}
	// Resources plus device-plugin GPUs from the profile.
	podSpec := map[string]any{}
	resources := map[string]any{}
	if p.Resources.Requests != nil {
		resources["requests"] = p.Resources.Requests
	}
	if p.Resources.Limits != nil {
		resources["limits"] = p.Resources.Limits
	}
	profile.AddGPUResources(resources, gpu.Count)
	if len(resources) > 0 {
		mainContainer["resources"] = resources
	}

	// Assemble containers (init + main + sidecars).
	containers := []any{mainContainer}
	for _, s := range o.Sidecars {
		containers = append(containers, containerToAny(s))
	}
	podSpec["containers"] = containers
	if len(o.InitContainers) > 0 {
		var inits []any
		for _, ic := range o.InitContainers {
			inits = append(inits, containerToAny(ic))
		}
		podSpec["initContainers"] = inits
	}
	if len(o.Volumes) > 0 {
		podSpec["volumes"] = volumesToAny(o.Volumes)
	}
	if gpu.Count > 0 {
		podSpec["tolerations"] = []any{
			map[string]any{"key": "sku", "operator": "Equal", "value": "gpu", "effect": "NoSchedule"},
			map[string]any{"key": "nvidia.com/gpu", "operator": "Exists", "effect": "NoSchedule"},
		}
	}

	topoProfile := p
	topoProfile.Lane = ""
	topoPlan, err := topology.Build(topoProfile, topology.Options{})
	if err != nil {
		return nil, fmt.Errorf("render Deployment topology: %w", err)
	}
	for k, v := range topoPlan.Annotations {
		podAnnotations[k] = v
	}

	deploySpec := map[string]any{
		"strategy": map[string]any{
			"type": "Recreate",
		},
		"selector": map[string]any{
			"matchLabels": map[string]any{"app": o.Name},
		},
		"template": map[string]any{
			"metadata": map[string]any{
				"labels":      podLabels,
				"annotations": podAnnotations,
			},
			"spec": podSpec,
		},
	}
	if o.Autoscaling == nil {
		deploySpec["replicas"] = replicas
	}

	deployment := map[string]any{
		"apiVersion": "apps/v1",
		"kind":       "Deployment",
		"metadata": map[string]any{
			"name":        o.Name,
			"namespace":   o.Namespace,
			"labels":      deployLabels,
			"annotations": rootAnnotations,
		},
		"spec": deploySpec,
	}
	if len(podAnnotations) == 0 {
		// Drop empty annotations so the rendered YAML is cleaner when
		// Kueue is opted out.
		tmpl := deployment["spec"].(map[string]any)["template"].(map[string]any)
		meta := tmpl["metadata"].(map[string]any)
		delete(meta, "annotations")
	}
	if len(rootAnnotations) == 0 {
		delete(deployment["metadata"].(map[string]any), "annotations")
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&yamlWriter{b: &buf})
	enc.SetIndent(2)
	if err := enc.Encode(deployment); err != nil {
		return nil, fmt.Errorf("marshal Deployment: %w", err)
	}
	if o.ServicePort > 0 {
		if err := enc.Encode(o.serviceObject(p)); err != nil {
			return nil, fmt.Errorf("marshal Service: %w", err)
		}
	}
	if o.Autoscaling != nil {
		if err := enc.Encode(o.hpaObject()); err != nil {
			return nil, fmt.Errorf("marshal HPA: %w", err)
		}
	}
	_ = enc.Close()
	return []byte(buf.String()), nil
}

func (o DeploymentOptions) validateDeployment() error {
	if o.Name == "" {
		return errors.New("DeploymentOptions.Name is required")
	}
	if o.Namespace == "" {
		return errors.New("DeploymentOptions.Namespace is required")
	}
	if o.Replicas < 0 {
		return errors.New("DeploymentOptions.Replicas must be >= 0")
	}
	if err := o.validateProbe("readinessProbe", o.ReadinessProbe); err != nil {
		return err
	}
	if err := o.validateProbe("startupProbe", o.StartupProbe); err != nil {
		return err
	}
	if err := o.validateProbe("livenessProbe", o.LivenessProbe); err != nil {
		return err
	}
	if o.ServicePort < 0 {
		return errors.New("DeploymentOptions.ServicePort must be >= 0")
	}
	if o.ServiceTargetPort < 0 {
		return errors.New("DeploymentOptions.ServiceTargetPort must be >= 0")
	}
	if o.Autoscaling != nil {
		if o.Replicas > 0 {
			return errors.New("Replicas and Autoscaling are mutually exclusive")
		}
		if err := o.Autoscaling.Validate(); err != nil {
			return err
		}
	}
	for i, ic := range o.InitContainers {
		if ic.Name == "" || ic.Image == "" {
			return fmt.Errorf("initContainer[%d]: name and image are required", i)
		}
	}
	for i, s := range o.Sidecars {
		if s.Name == "" || s.Image == "" {
			return fmt.Errorf("sidecar[%d]: name and image are required", i)
		}
	}
	for i, v := range o.Volumes {
		if v.Name == "" {
			return fmt.Errorf("volume[%d]: name is required", i)
		}
		count := 0
		if v.EmptyDir {
			count++
		}
		if v.ConfigMap != "" {
			count++
		}
		if v.Secret != "" {
			count++
		}
		if v.PVC != "" {
			count++
		}
		if count != 1 {
			return fmt.Errorf("volume[%s]: exactly one of emptyDir/configMap/secret/pvc must be set", v.Name)
		}
	}
	return nil
}

func (o DeploymentOptions) validateProbe(name string, p HTTPProbe) error {
	if p.Path == "" {
		return nil
	}
	if !strings.HasPrefix(p.Path, "/") {
		return fmt.Errorf("%s.Path must start with /", name)
	}
	if p.Port < 0 {
		return fmt.Errorf("%s.Port must be >= 0", name)
	}
	if p.FailureThreshold < 0 {
		return fmt.Errorf("%s.FailureThreshold must be >= 0", name)
	}
	if p.Port == 0 && o.defaultProbePort() == 0 {
		return fmt.Errorf("%s.Path requires a port; set a probe port, service port, service target port, or deployment port", name)
	}
	return nil
}

func (o DeploymentOptions) defaultProbePort() int {
	if o.ServiceTargetPort > 0 {
		return o.ServiceTargetPort
	}
	if o.ServicePort > 0 {
		return o.ServicePort
	}
	if len(o.Ports) > 0 {
		return o.Ports[0]
	}
	return 0
}

func (o DeploymentOptions) serviceTargetPort() int {
	if o.ServiceTargetPort > 0 {
		return o.ServiceTargetPort
	}
	if len(o.Ports) > 0 {
		return o.Ports[0]
	}
	return o.ServicePort
}

func (o DeploymentOptions) serviceObject(p profile.Profile) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       "Service",
		"metadata": map[string]any{
			"name":      o.Name,
			"namespace": o.Namespace,
			"labels": map[string]any{
				"app":                     o.Name,
				workloadmeta.LabelService: o.Name,
				workloadmeta.LabelProfile: p.Name,
			},
		},
		"spec": map[string]any{
			"type":     "ClusterIP",
			"selector": map[string]any{"app": o.Name},
			"ports": []any{
				map[string]any{
					"name":       "http",
					"protocol":   "TCP",
					"port":       int64(o.ServicePort),
					"targetPort": int64(o.serviceTargetPort()),
				},
			},
		},
	}
}

func (o DeploymentOptions) hpaObject() map[string]any {
	a := o.Autoscaling.WithDefaults()
	metrics := []any{
		map[string]any{
			"type": "Resource",
			"resource": map[string]any{
				"name": "cpu",
				"target": map[string]any{
					"type":               "Utilization",
					"averageUtilization": int64(80),
				},
			},
		},
	}
	if a.TargetQPS > 0 {
		metrics = append(metrics, map[string]any{
			"type": "Pods",
			"pods": map[string]any{
				"metric": map[string]any{"name": "http_requests_per_second"},
				"target": map[string]any{
					"type":         "AverageValue",
					"averageValue": fmt.Sprintf("%d", a.TargetQPS),
				},
			},
		})
	}
	return map[string]any{
		"apiVersion": "autoscaling/v2",
		"kind":       "HorizontalPodAutoscaler",
		"metadata": map[string]any{
			"name":      o.Name,
			"namespace": o.Namespace,
		},
		"spec": map[string]any{
			"scaleTargetRef": map[string]any{
				"apiVersion": "apps/v1",
				"kind":       "Deployment",
				"name":       o.Name,
			},
			"minReplicas": int64(a.MinReplicas),
			"maxReplicas": int64(a.MaxReplicas),
			"metrics":     metrics,
			"behavior": map[string]any{
				"scaleDown": map[string]any{
					"stabilizationWindowSeconds": int64(a.ScaleDownDelay),
				},
			},
		},
	}
}

func resolveDeploymentImage(p profile.Profile, o DeploymentOptions) (string, error) {
	if o.Image != "" {
		return o.Image, nil
	}
	if p.Runtime.Image != "" {
		return p.Runtime.Image, nil
	}
	return "", fmt.Errorf("no image: set --image or a profile runtime image")
}

func probeToAny(p HTTPProbe, defaultPort int) map[string]any {
	if p.Path == "" {
		return nil
	}
	port := p.Port
	if port == 0 {
		port = defaultPort
	}
	out := map[string]any{
		"httpGet": map[string]any{
			"path": p.Path,
			"port": int64(port),
		},
	}
	if p.FailureThreshold > 0 {
		out["failureThreshold"] = int64(p.FailureThreshold)
	}
	return out
}

func containerToAny(c Container) map[string]any {
	m := map[string]any{
		"name":  c.Name,
		"image": c.Image,
	}
	if len(c.Command) > 0 {
		m["command"] = stringsToAny(c.Command)
	}
	if len(c.Args) > 0 {
		m["args"] = stringsToAny(c.Args)
	}
	if len(c.Env) > 0 {
		m["env"] = envMapToList(c.Env)
	}
	if len(c.Ports) > 0 {
		var ports []any
		for _, p := range c.Ports {
			ports = append(ports, map[string]any{"containerPort": int64(p)})
		}
		m["ports"] = ports
	}
	if len(c.VolumeMounts) > 0 {
		m["volumeMounts"] = volumeMountsToAny(c.VolumeMounts)
	}
	return m
}

func envMapToList(env map[string]string) []any {
	// Stable order for deterministic output.
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sortStrings(keys)
	out := make([]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, map[string]any{"name": k, "value": env[k]})
	}
	return out
}

func stringMapToAny(values map[string]string) map[string]any {
	out := map[string]any{}
	for k, v := range values {
		if k != "" && v != "" {
			out[k] = v
		}
	}
	return out
}

func volumeMountsToAny(vms []VolumeMount) []any {
	out := make([]any, 0, len(vms))
	for _, vm := range vms {
		m := map[string]any{"name": vm.Name, "mountPath": vm.MountPath}
		if vm.ReadOnly {
			m["readOnly"] = true
		}
		if vm.SubPath != "" {
			m["subPath"] = vm.SubPath
		}
		out = append(out, m)
	}
	return out
}

func volumesToAny(vs []Volume) []any {
	out := make([]any, 0, len(vs))
	for _, v := range vs {
		m := map[string]any{"name": v.Name}
		switch {
		case v.EmptyDir:
			m["emptyDir"] = map[string]any{}
		case v.ConfigMap != "":
			m["configMap"] = map[string]any{"name": v.ConfigMap}
		case v.Secret != "":
			m["secret"] = map[string]any{"secretName": v.Secret}
		case v.PVC != "":
			m["persistentVolumeClaim"] = map[string]any{"claimName": v.PVC}
		}
		out = append(out, m)
	}
	return out
}

// Small local sort wrapper so we don't import sort everywhere; kept
// alongside the renderer to avoid cross-file shuffling.
func sortStrings(s []string) {
	// insertion sort is fine for env var counts
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
