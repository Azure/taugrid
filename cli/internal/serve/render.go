// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package serve renders a tau Profile + user options into a KubeRay
// RayService manifest. North-star §1 / §5 — serving.
//
// V0 scope:
//   - RayService CR with a single headGroup (no workerGroup). This is the
//     simplest KubeRay shape that proves the admission → head-pod-up →
//     HTTP-200-on-/healthz loop end-to-end. Workers come when there's
//     real traffic pressure to justify scale-out; today users
//     run a single-pod serve.
//   - Kueue queue label (kueue.x-k8s.io/queue-name) from profile. Kueue
//     RayService integration is enabled cluster-side; labelling
//     the parent CR is how admission is requested.
//
// V0 non-goals (deliberate, see anti-pattern #6 — ship all 5 commands
// before the surface is locked):
//   - Multi-replica workerGroupSpecs (template left in bash CLI; port
//     when needed).
//   - vLLM tensor-parallelism auto-configuration from profile. The image
//     is the user's serve container today; they pass --args or bake it.
//   - RayService status/scale/delete subcommands — stubs still.
package serve

import (
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

// Default RayService parameters. Pinned; bump deliberately.
const (
	DefaultRayVersion = "2.40.0"
	DefaultImportPath = "serve:app"
	DefaultServePort  = 8000
	DashboardPort     = 8265
)

// Options collects everything the renderer needs that isn't in the Profile.
type Options struct {
	Name        string // required
	Namespace   string // required
	Image       string // overrides Profile.Runtime.Image
	Replicas    int    // serve deployment replicas when ReplicasSet is true
	ReplicasSet bool   // render a Ray Serve deployment override
	ImportPath  string // Ray Serve import path (default serve:app)
	ServePort   int    // head pod serve port (default 8000)
	RayVersion  string // default 2.40.0
	Args        []string
	Env         map[string]string
	EnvVars     []envspec.Var
	RuntimePip  []string

	VolumeMounts []VolumeMount
	Volumes      []Volume

	Autoscaling *AutoscalingOptions
}

// Render turns a resolved Profile + Options into a single-document YAML
// RayService manifest ready for `kubectl apply -f -`.
func Render(p profile.Profile, o Options) ([]byte, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}

	image, err := resolveImage(p, o)
	if err != nil {
		return nil, err
	}
	importPath := o.ImportPath
	if importPath == "" {
		importPath = DefaultImportPath
	}
	port := o.ServePort
	if port <= 0 {
		port = DefaultServePort
	}
	rayVersion := o.RayVersion
	if rayVersion == "" {
		rayVersion = DefaultRayVersion
	}
	env, err := envspec.Merge(envspec.FromMap(o.Env), o.EnvVars)
	if err != nil {
		return nil, err
	}
	runtimePip, err := resolveRuntimePip(o.RuntimePip)
	if err != nil {
		return nil, err
	}
	gpu := p.Resources.GPU
	gpuLabels := gpu.Labels()
	gpuAnnotations := gpu.Annotations()

	// Kueue's RayService integration watches the CR for queue-name; without it
	// the CR is never admitted, so an unlabelled RayService is a submit that
	// silently never runs. Also propagated to head pod labels for observability.
	queueName := profileLocalQueue(p)
	if queueName == "" {
		return nil, errors.New("render RayService: Kueue LocalQueue is required")
	}
	labels := map[string]any{
		workloadmeta.LabelService:   o.Name,
		workloadmeta.LabelProfile:   p.Name,
		"kueue.x-k8s.io/queue-name": queueName,
	}
	for k, v := range gpuLabels {
		labels[k] = v
	}
	headPodLabels := map[string]any{
		workloadmeta.LabelService: o.Name,
	}
	for k, v := range gpuLabels {
		headPodLabels[k] = v
	}

	headPodSpec := map[string]any{}

	headContainer := map[string]any{
		"name":  "ray-head",
		"image": image,
		"ports": []any{
			map[string]any{"containerPort": int64(port), "name": "serve"},
			map[string]any{"containerPort": int64(DashboardPort), "name": "dashboard"},
		},
	}
	if len(o.Args) > 0 {
		headContainer["args"] = stringsToAny(o.Args)
	}
	if len(env) > 0 {
		headContainer["env"] = envspec.K8sList(env)
	}
	if len(o.VolumeMounts) > 0 {
		headContainer["volumeMounts"] = volumeMountsToAny(o.VolumeMounts)
	}

	// Resources: CPU/memory requests from profile, plus device-plugin GPUs.
	resources := map[string]any{}
	if p.Resources.Requests != nil {
		resources["requests"] = p.Resources.Requests
	}
	if p.Resources.Limits != nil {
		resources["limits"] = p.Resources.Limits
	}
	profile.AddGPUResources(resources, gpu.Count)
	if len(resources) > 0 {
		headContainer["resources"] = resources
	}
	headPodSpec["containers"] = []any{headContainer}
	if len(o.Volumes) > 0 {
		headPodSpec["volumes"] = volumesToAny(o.Volumes)
	}

	var serveConfig strings.Builder
	fmt.Fprintf(&serveConfig, "applications:\n")
	fmt.Fprintf(&serveConfig, "  - name: default\n")
	fmt.Fprintf(&serveConfig, "    route_prefix: /\n")
	fmt.Fprintf(&serveConfig, "    import_path: %s\n", importPath)
	directEnv := envspec.DirectMap(env)
	if len(runtimePip) > 0 || len(directEnv) > 0 {
		fmt.Fprintf(&serveConfig, "    runtime_env:\n")
		if len(runtimePip) > 0 {
			fmt.Fprintf(&serveConfig, "      pip:\n")
			for _, pkg := range runtimePip {
				fmt.Fprintf(&serveConfig, "        - %q\n", pkg)
			}
		}
		if len(directEnv) > 0 {
			fmt.Fprintf(&serveConfig, "      env_vars:\n")
			keys := make([]string, 0, len(directEnv))
			for k := range directEnv {
				keys = append(keys, k)
			}
			sortStrings(keys)
			for _, k := range keys {
				fmt.Fprintf(&serveConfig, "        %s: %q\n", k, directEnv[k])
			}
		}
	}
	if o.Autoscaling != nil {
		a := o.Autoscaling.WithDefaults()
		if a.TargetQPS == 0 {
			a.TargetQPS = 5 // RayService default; HPA path uses 0 = CPU-only
		}
		fmt.Fprintf(&serveConfig, "    deployments:\n")
		fmt.Fprintf(&serveConfig, "      - name: default\n")
		fmt.Fprintf(&serveConfig, "        autoscaling_config:\n")
		fmt.Fprintf(&serveConfig, "          min_replicas: %d\n", a.MinReplicas)
		fmt.Fprintf(&serveConfig, "          max_replicas: %d\n", a.MaxReplicas)
		fmt.Fprintf(&serveConfig, "          target_num_ongoing_requests_per_replica: %d\n", a.TargetQPS)
		fmt.Fprintf(&serveConfig, "          downscale_delay_s: %d\n", a.ScaleDownDelay)
	} else if o.ReplicasSet {
		fmt.Fprintf(&serveConfig, "    deployments:\n")
		fmt.Fprintf(&serveConfig, "      - name: default\n")
		fmt.Fprintf(&serveConfig, "        num_replicas: %d\n", o.Replicas)
	}

	rayService := map[string]any{
		"apiVersion": "ray.io/v1",
		"kind":       "RayService",
		"metadata": map[string]any{
			"name":        o.Name,
			"namespace":   o.Namespace,
			"labels":      labels,
			"annotations": stringMapToAny(gpuAnnotations),
		},
		"spec": map[string]any{
			"serveConfigV2": serveConfig.String(),
			"rayClusterConfig": map[string]any{
				"rayVersion": rayVersion,
				"headGroupSpec": map[string]any{
					"rayStartParams": map[string]any{
						"dashboard-host": "0.0.0.0",
					},
					"template": map[string]any{
						"metadata": map[string]any{
							"labels":      headPodLabels,
							"annotations": stringMapToAny(gpuAnnotations),
						},
						"spec": headPodSpec,
					},
				},
			},
		},
	}
	if len(gpuAnnotations) == 0 {
		delete(rayService["metadata"].(map[string]any), "annotations")
		tmpl := rayService["spec"].(map[string]any)["rayClusterConfig"].(map[string]any)["headGroupSpec"].(map[string]any)["template"].(map[string]any)
		delete(tmpl["metadata"].(map[string]any), "annotations")
	}

	var buf strings.Builder
	enc := yaml.NewEncoder(&yamlWriter{b: &buf})
	enc.SetIndent(2)
	if err := enc.Encode(rayService); err != nil {
		return nil, fmt.Errorf("marshal RayService: %w", err)
	}
	_ = enc.Close()
	return []byte(buf.String()), nil
}

func (o Options) validate() error {
	if o.Name == "" {
		return errors.New("Options.Name is required")
	}
	if o.Namespace == "" {
		return errors.New("Options.Namespace is required")
	}
	if o.ReplicasSet && o.Replicas < 0 {
		return errors.New("Options.Replicas must be >= 0")
	}
	if o.ReplicasSet && o.Autoscaling != nil {
		return errors.New("Replicas and Autoscaling are mutually exclusive")
	}
	if o.Autoscaling != nil {
		if err := o.Autoscaling.Validate(); err != nil {
			return err
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

func resolveImage(p profile.Profile, o Options) (string, error) {
	if o.Image != "" {
		return o.Image, nil
	}
	if p.Runtime.Image != "" {
		return p.Runtime.Image, nil
	}
	return "", fmt.Errorf("no image: profile %q declares no runtime image and --image was not set", p.Name)
}

// yamlWriter is a minimal io.Writer → strings.Builder adapter that lets
// us use yaml.Encoder (which needs io.Writer) into a Builder.
type yamlWriter struct{ b *strings.Builder }

func (w *yamlWriter) Write(p []byte) (int, error) { return w.b.Write(p) }

func stringsToAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func profileLocalQueue(p profile.Profile) string {
	return strings.TrimSpace(p.Queue)
}

func resolveRuntimePip(override []string) ([]string, error) {
	pip := append([]string{}, override...)
	for i, pkg := range pip {
		if strings.TrimSpace(pkg) == "" {
			return nil, fmt.Errorf("runtime.pip[%d]: blank entry", i)
		}
	}
	return pip, nil
}
