// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package serve

import (
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/envspec"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func deployBaseProfile() profile.Profile {
	return profile.Profile{
		Name:    "ai-serve-gpu-a100",
		Queue:   "serving-queue",
		Runtime: profile.Runtime{Image: "my-registry/my-serve:latest"},
		Resources: profile.Resources{
			Requests: map[string]any{"cpu": "4", "memory": "16Gi"},
			Limits:   map[string]any{"cpu": "8", "memory": "32Gi"},
			GPU:      profile.GPUContract{Count: 1, Size: "l"},
		},
	}
}

func decodeOne(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(b, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return m
}

func decodeAll(t *testing.T, b []byte) []map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(string(b)))
	var out []map[string]any
	for {
		var m map[string]any
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("decode all: %v", err)
		}
		if len(m) == 0 {
			continue
		}
		out = append(out, m)
	}
	return out
}

func getPath(t *testing.T, m map[string]any, keys ...string) any {
	t.Helper()
	cur := any(m)
	for _, k := range keys {
		cm, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %v: not a map at key %q (got %T)", keys, k, cur)
		}
		cur = cm[k]
	}
	return cur
}

func TestRenderDeployment_Defaults(t *testing.T) {
	p := deployBaseProfile()
	o := DeploymentOptions{Name: "fish-tts", Namespace: "tau"}
	b, err := RenderDeployment(p, o)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	doc := decodeOne(t, b)

	if kind, _ := doc["kind"].(string); kind != "Deployment" {
		t.Fatalf("kind=%q want Deployment", kind)
	}
	if api, _ := doc["apiVersion"].(string); api != "apps/v1" {
		t.Fatalf("apiVersion=%q want apps/v1", api)
	}
	if r := getPath(t, doc, "spec", "replicas"); r != int(1) && r != int64(1) {
		t.Fatalf("default replicas should be 1, got %v (%T)", r, r)
	}
	strategy := getPath(t, doc, "spec", "strategy").(map[string]any)
	if strategy["type"] != "Recreate" {
		t.Fatalf("Deployment updates should not surge scarce GPU pods, got strategy=%v", strategy)
	}
	// Deployment-level queue label
	labels := getPath(t, doc, "metadata", "labels").(map[string]any)
	if labels["kueue.x-k8s.io/queue-name"] != "serving-queue" {
		t.Fatalf("missing queue-name label: %v", labels)
	}
	if labels[workloadmeta.AnnotationGPUCount] != "1" {
		t.Fatalf("deployment labels should expose GPU contract: %v", labels)
	}
	deployAnn := getPath(t, doc, "metadata", "annotations").(map[string]any)
	if deployAnn[workloadmeta.AnnotationGPUContract] != "count=1,size=l" {
		t.Fatalf("deployment annotations should expose GPU contract: %v", deployAnn)
	}
	// Pod-template Kueue managed label + suspending-parent annotation
	podLabels := getPath(t, doc, "spec", "template", "metadata", "labels").(map[string]any)
	if podLabels["kueue.x-k8s.io/managed"] != "true" {
		t.Fatalf("pod template missing kueue.x-k8s.io/managed=true: %v", podLabels)
	}
	podAnn := getPath(t, doc, "spec", "template", "metadata", "annotations").(map[string]any)
	if podAnn["kueue.x-k8s.io/pod-suspending-parent"] != "deployment" {
		t.Fatalf("pod template missing suspending-parent annotation: %v", podAnn)
	}
	// The pod template must carry queue-name alongside the managed label and
	// the suspending-parent annotation, because that annotation makes Kueue's
	// Pod webhook gate the pod before it consults queue-name. #1317 shipped the
	// Deployment-level label while omitting this one, so asserting only the
	// Deployment would not catch it.
	if podLabels["kueue.x-k8s.io/queue-name"] != "serving-queue" {
		t.Fatalf("pod template must carry queue-name or its pods gate forever: %v", podLabels)
	}
	if podAnn[workloadmeta.AnnotationGPUContract] != "count=1,size=l" {
		t.Fatalf("pod template annotations should expose GPU contract: %v", podAnn)
	}
	// Main container image from profile runtime.image
	containers := getPath(t, doc, "spec", "template", "spec", "containers").([]any)
	if len(containers) != 1 {
		t.Fatalf("want 1 container, got %d", len(containers))
	}
	c := containers[0].(map[string]any)
	if c["image"] != "my-registry/my-serve:latest" {
		t.Fatalf("image not pulled from profile: %v", c["image"])
	}
	resources := c["resources"].(map[string]any)
	spec := getPath(t, doc, "spec", "template", "spec").(map[string]any)
	if _, ok := spec["resourceClaims"]; ok {
		t.Fatalf("device-plugin deployment must not have resourceClaims: %v", spec)
	}
	if _, ok := resources["claims"]; ok {
		t.Fatalf("device-plugin container must not have resource claims: %v", resources)
	}
}

func TestRenderDeployment_DevicePlugin(t *testing.T) {
	p := deployBaseProfile()
	b, err := RenderDeployment(p, DeploymentOptions{Name: "dp-serve", Namespace: "tau"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	doc := decodeOne(t, b)
	spec := getPath(t, doc, "spec", "template", "spec").(map[string]any)
	if _, ok := spec["resourceClaims"]; ok {
		t.Fatalf("device-plugin deployment must not have pod resourceClaims: %v", spec["resourceClaims"])
	}
	c := spec["containers"].([]any)[0].(map[string]any)
	resources := c["resources"].(map[string]any)
	if _, ok := resources["claims"]; ok {
		t.Fatalf("device-plugin container must not have resources.claims: %v", resources)
	}
	reqs := resources["requests"].(map[string]any)
	lims := resources["limits"].(map[string]any)
	if reqs["nvidia.com/gpu"] != int(1) && reqs["nvidia.com/gpu"] != int64(1) {
		t.Fatalf("expected nvidia.com/gpu=1 in requests: %v", reqs)
	}
	if lims["nvidia.com/gpu"] != int(1) && lims["nvidia.com/gpu"] != int64(1) {
		t.Fatalf("expected nvidia.com/gpu=1 in limits: %v", lims)
	}
}

func TestRenderDeployment_ImageOverride(t *testing.T) {
	p := deployBaseProfile()
	o := DeploymentOptions{Name: "x", Namespace: "tau", Image: "override:v1"}
	b, _ := RenderDeployment(p, o)
	doc := decodeOne(t, b)
	containers := getPath(t, doc, "spec", "template", "spec", "containers").([]any)
	c := containers[0].(map[string]any)
	if c["image"] != "override:v1" {
		t.Fatalf("image override failed: %v", c["image"])
	}
}

func TestRenderDeployment_KueueOptOut(t *testing.T) {
	p := deployBaseProfile()
	no := false
	o := DeploymentOptions{Name: "x", Namespace: "tau", KueueManaged: &no}
	b, _ := RenderDeployment(p, o)
	doc := decodeOne(t, b)

	labels := getPath(t, doc, "metadata", "labels").(map[string]any)
	if _, ok := labels["kueue.x-k8s.io/queue-name"]; ok {
		t.Fatal("KueueManaged=false should not add queue-name label")
	}
	podLabels := getPath(t, doc, "spec", "template", "metadata", "labels").(map[string]any)
	if _, ok := podLabels["kueue.x-k8s.io/managed"]; ok {
		t.Fatal("KueueManaged=false should not add managed label")
	}
	tmplMeta := getPath(t, doc, "spec", "template", "metadata").(map[string]any)
	annotations := tmplMeta["annotations"].(map[string]any)
	if _, ok := annotations["kueue.x-k8s.io/pod-suspending-parent"]; ok {
		t.Fatal("KueueManaged=false should not emit Kueue pod-suspending-parent annotation")
	}
	if annotations[workloadmeta.AnnotationGPUContract] != "count=1,size=l" {
		t.Fatalf("GPU contract annotations should remain with KueueManaged=false: %v", annotations)
	}
}

func TestRenderDeployment_InitAndSidecar(t *testing.T) {
	p := deployBaseProfile()
	o := DeploymentOptions{
		Name:      "fish-speech-tts",
		Namespace: "tau",
		Image:     "my-registry/fish-api:latest",
		Ports:     []int{8080},
		InitContainers: []Container{
			{Name: "hf-download", Image: "curlimages/curl:8", Command: []string{"sh", "-c"}, Args: []string{"curl -o /models/... && echo done"}},
		},
		Sidecars: []Container{
			{Name: "tts-rvc", Image: "my-registry/rvc:latest", Ports: []int{9090}},
		},
	}
	b, err := RenderDeployment(p, o)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	doc := decodeOne(t, b)
	spec := getPath(t, doc, "spec", "template", "spec").(map[string]any)
	inits := spec["initContainers"].([]any)
	if len(inits) != 1 || inits[0].(map[string]any)["name"] != "hf-download" {
		t.Fatalf("init container not wired: %v", inits)
	}
	containers := spec["containers"].([]any)
	if len(containers) != 2 {
		t.Fatalf("want 2 containers (main + sidecar), got %d", len(containers))
	}
	if containers[0].(map[string]any)["name"] != "main" {
		t.Fatalf("first container should be main, got %v", containers[0])
	}
	if containers[1].(map[string]any)["name"] != "tts-rvc" {
		t.Fatalf("second container should be tts-rvc, got %v", containers[1])
	}
}

func TestRenderDeployment_Volumes(t *testing.T) {
	p := deployBaseProfile()
	o := DeploymentOptions{
		Name:      "x",
		Namespace: "tau",
		Image:     "img:v",
		VolumeMounts: []VolumeMount{
			{Name: "data", MountPath: "/data"},
			{Name: "shm", MountPath: "/dev/shm"},
		},
		Volumes: []Volume{
			{Name: "data", PVC: "blob-training"},
			{Name: "shm", EmptyDir: true},
			{Name: "creds", Secret: "hf-token"},
			{Name: "code", ConfigMap: "autoresearch-code"},
		},
	}
	b, err := RenderDeployment(p, o)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	doc := decodeOne(t, b)
	spec := getPath(t, doc, "spec", "template", "spec").(map[string]any)
	vols := spec["volumes"].([]any)
	if len(vols) != 4 {
		t.Fatalf("want 4 volumes, got %d", len(vols))
	}
	// Check each kind is wired
	want := map[string]string{
		"data":  "persistentVolumeClaim",
		"shm":   "emptyDir",
		"creds": "secret",
		"code":  "configMap",
	}
	for _, v := range vols {
		m := v.(map[string]any)
		name := m["name"].(string)
		expectKey := want[name]
		if _, ok := m[expectKey]; !ok {
			t.Fatalf("volume %q missing %q key: %v", name, expectKey, m)
		}
	}
	// VolumeMounts
	containers := spec["containers"].([]any)
	c := containers[0].(map[string]any)
	vms := c["volumeMounts"].([]any)
	if len(vms) != 2 {
		t.Fatalf("want 2 volumeMounts, got %d", len(vms))
	}
}

func TestRenderDeployment_SecretEnvAndRuntimePipContract(t *testing.T) {
	p := deployBaseProfile()
	out, err := RenderDeployment(p, DeploymentOptions{
		Name:      "captioner2",
		Namespace: "tau",
		Image:     "acr.io/captioner2:v1",
		EnvVars: []envspec.Var{
			envspec.Secret("HUGGING_FACE_HUB_TOKEN", "hf-token", "token"),
		},
	})
	if err != nil {
		t.Fatalf("render secret env: %v", err)
	}
	doc := decodeOne(t, out)
	containers := getPath(t, doc, "spec", "template", "spec", "containers").([]any)
	env := containers[0].(map[string]any)["env"].([]any)
	var found bool
	for _, item := range env {
		m := item.(map[string]any)
		if m["name"] == "HUGGING_FACE_HUB_TOKEN" {
			found = true
			ref := m["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
			if ref["name"] != "hf-token" || ref["key"] != "token" {
				t.Fatalf("bad secret ref: %v", ref)
			}
		}
	}
	if !found {
		t.Fatalf("secret env missing: %v", env)
	}

	_, err = RenderDeployment(p, DeploymentOptions{
		Name:       "bad",
		Namespace:  "ray",
		Image:      "acr.io/app:v1",
		RuntimePip: []string{"transformers"},
	})
	if err == nil || !strings.Contains(err.Error(), "runtime.pip") {
		t.Fatalf("expected runtime.pip deployment error, got %v", err)
	}
}

func TestRenderDeployment_ProbesAndService(t *testing.T) {
	p := deployBaseProfile()
	o := DeploymentOptions{
		Name:      "stt-serving",
		Namespace: "tau",
		Image:     "my-registry/stt-api:latest",
		Ports:     []int{8000},
		ReadinessProbe: HTTPProbe{
			Path: "/health",
		},
		StartupProbe: HTTPProbe{
			Path:             "/health",
			FailureThreshold: 30,
		},
		LivenessProbe: HTTPProbe{
			Path: "/live",
		},
		ServicePort:       80,
		ServiceTargetPort: 8000,
	}
	b, err := RenderDeployment(p, o)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeAll(t, b)
	if len(docs) != 2 {
		t.Fatalf("want Deployment + Service docs, got %d:\n%s", len(docs), string(b))
	}
	deploy := docs[0]
	if deploy["kind"] != "Deployment" {
		t.Fatalf("first doc kind=%v want Deployment", deploy["kind"])
	}
	containers := getPath(t, deploy, "spec", "template", "spec", "containers").([]any)
	c := containers[0].(map[string]any)
	readiness := c["readinessProbe"].(map[string]any)
	startup := c["startupProbe"].(map[string]any)
	liveness := c["livenessProbe"].(map[string]any)
	for name, probe := range map[string]map[string]any{
		"readiness": readiness,
		"startup":   startup,
		"liveness":  liveness,
	} {
		httpGet := probe["httpGet"].(map[string]any)
		if httpGet["port"] != int(8000) && httpGet["port"] != int64(8000) {
			t.Fatalf("%s probe port=%v", name, httpGet["port"])
		}
	}
	if readiness["httpGet"].(map[string]any)["path"] != "/health" {
		t.Fatalf("readiness path not set: %v", readiness)
	}
	if startup["failureThreshold"] != int(30) && startup["failureThreshold"] != int64(30) {
		t.Fatalf("startup failureThreshold not set: %v", startup)
	}
	if liveness["httpGet"].(map[string]any)["path"] != "/live" {
		t.Fatalf("liveness path not set: %v", liveness)
	}

	svc := docs[1]
	if svc["kind"] != "Service" {
		t.Fatalf("second doc kind=%v want Service", svc["kind"])
	}
	if getPath(t, svc, "metadata", "name") != "stt-serving" {
		t.Fatalf("service name mismatch: %v", getPath(t, svc, "metadata", "name"))
	}
	selector := getPath(t, svc, "spec", "selector").(map[string]any)
	if selector["app"] != "stt-serving" {
		t.Fatalf("service selector mismatch: %v", selector)
	}
	ports := getPath(t, svc, "spec", "ports").([]any)
	if len(ports) != 1 {
		t.Fatalf("want 1 service port, got %d", len(ports))
	}
	port := ports[0].(map[string]any)
	if port["port"] != int(80) && port["port"] != int64(80) {
		t.Fatalf("service port mismatch: %v", port)
	}
	if port["targetPort"] != int(8000) && port["targetPort"] != int64(8000) {
		t.Fatalf("service targetPort mismatch: %v", port)
	}
}

func TestRenderDeployment_EnvStableOrder(t *testing.T) {
	p := deployBaseProfile()
	o := DeploymentOptions{
		Name:      "x",
		Namespace: "tau",
		Image:     "img:v",
		Env: map[string]string{
			"Z_VAR":   "last",
			"A_VAR":   "first",
			"M_VAR":   "middle",
			"ANOTHER": "secondish",
		},
	}
	b, _ := RenderDeployment(p, o)
	// Env must be emitted in deterministic lexical order. ASCII '_' is
	// 0x5F; 'N' is 0x4E, so "ANOTHER" < "A_VAR" byte-wise.
	s := string(b)
	posAnother := strings.Index(s, "name: ANOTHER")
	posAVar := strings.Index(s, "name: A_VAR")
	posM := strings.Index(s, "name: M_VAR")
	posZ := strings.Index(s, "name: Z_VAR")
	if !(posAnother < posAVar && posAVar < posM && posM < posZ) {
		t.Fatalf("env not sorted: ANOTHER=%d A_VAR=%d M=%d Z=%d", posAnother, posAVar, posM, posZ)
	}
}

func TestRenderDeployment_Validation(t *testing.T) {
	cases := []struct {
		name string
		o    DeploymentOptions
		want string
	}{
		{"no name", DeploymentOptions{Namespace: "x"}, "Name is required"},
		{"no namespace", DeploymentOptions{Name: "x"}, "Namespace is required"},
		{"negative replicas", DeploymentOptions{Name: "x", Namespace: "y", Replicas: -1}, "Replicas must be >= 0"},
		{"init missing image", DeploymentOptions{
			Name: "x", Namespace: "y",
			InitContainers: []Container{{Name: "i1"}},
		}, "image are required"},
		{"sidecar missing name", DeploymentOptions{
			Name: "x", Namespace: "y",
			Sidecars: []Container{{Image: "i"}},
		}, "image are required"},
		{"volume no source", DeploymentOptions{
			Name: "x", Namespace: "y",
			Volumes: []Volume{{Name: "v"}},
		}, "exactly one of"},
		{"volume multi source", DeploymentOptions{
			Name: "x", Namespace: "y",
			Volumes: []Volume{{Name: "v", EmptyDir: true, PVC: "p"}},
		}, "exactly one of"},
		{"probe path without slash", DeploymentOptions{
			Name: "x", Namespace: "y", Ports: []int{8000},
			ReadinessProbe: HTTPProbe{Path: "health"},
		}, "Path must start with /"},
		{"probe path without port", DeploymentOptions{
			Name: "x", Namespace: "y",
			ReadinessProbe: HTTPProbe{Path: "/health"},
		}, "requires a port"},
		{"negative service port", DeploymentOptions{
			Name: "x", Namespace: "y", ServicePort: -1,
		}, "ServicePort must be >= 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := RenderDeployment(deployBaseProfile(), c.o)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%q want substring %q", err.Error(), c.want)
			}
		})
	}
}

func TestRenderDeployment_NoImageNoProfileRuntime(t *testing.T) {
	p := profile.Profile{Name: "bare"}
	o := DeploymentOptions{Name: "x", Namespace: "y"}
	_, err := RenderDeployment(p, o)
	if err == nil || !strings.Contains(err.Error(), "no image") {
		t.Fatalf("expected 'no image' error, got %v", err)
	}
}

func TestRenderDeployment_Autoscaling_HPA(t *testing.T) {
	p := deployBaseProfile()
	o := DeploymentOptions{
		Name:              "autoscale-svc",
		Namespace:         "tau",
		Image:             "my-registry/serve:v1",
		Ports:             []int{8000},
		ServicePort:       80,
		ServiceTargetPort: 8000,
		Autoscaling: &AutoscalingOptions{
			MinReplicas:    2,
			MaxReplicas:    10,
			TargetQPS:      100,
			ScaleDownDelay: 600,
		},
	}
	b, err := RenderDeployment(p, o)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeAll(t, b)
	if len(docs) != 3 {
		t.Fatalf("want Deployment + Service + HPA (3 docs), got %d", len(docs))
	}
	deploy := docs[0]
	if deploy["kind"] != "Deployment" {
		t.Fatalf("doc[0] kind=%v want Deployment", deploy["kind"])
	}
	if getPath(t, deploy, "spec", "replicas") != nil {
		t.Fatalf("Deployment must omit spec.replicas when autoscaling is active")
	}

	svc := docs[1]
	if svc["kind"] != "Service" {
		t.Fatalf("doc[1] kind=%v want Service", svc["kind"])
	}

	hpa := docs[2]
	if hpa["kind"] != "HorizontalPodAutoscaler" {
		t.Fatalf("doc[2] kind=%v want HorizontalPodAutoscaler", hpa["kind"])
	}
	if hpa["apiVersion"] != "autoscaling/v2" {
		t.Fatalf("HPA apiVersion=%v want autoscaling/v2", hpa["apiVersion"])
	}

	ref := getPath(t, hpa, "spec", "scaleTargetRef").(map[string]any)
	if ref["apiVersion"] != "apps/v1" || ref["kind"] != "Deployment" || ref["name"] != "autoscale-svc" {
		t.Fatalf("HPA scaleTargetRef mismatch: %v", ref)
	}

	minR := getPath(t, hpa, "spec", "minReplicas")
	if minR != int(2) && minR != int64(2) {
		t.Fatalf("HPA minReplicas=%v want 2", minR)
	}
	maxR := getPath(t, hpa, "spec", "maxReplicas")
	if maxR != int(10) && maxR != int64(10) {
		t.Fatalf("HPA maxReplicas=%v want 10", maxR)
	}

	metrics := getPath(t, hpa, "spec", "metrics").([]any)
	if len(metrics) != 2 {
		t.Fatalf("want 2 metrics (CPU + QPS), got %d", len(metrics))
	}
	cpuMetric := metrics[0].(map[string]any)
	if cpuMetric["type"] != "Resource" {
		t.Fatalf("first metric type=%v want Resource", cpuMetric["type"])
	}
	qpsMetric := metrics[1].(map[string]any)
	if qpsMetric["type"] != "Pods" {
		t.Fatalf("second metric type=%v want Pods", qpsMetric["type"])
	}
	podMetric := qpsMetric["pods"].(map[string]any)
	metricName := podMetric["metric"].(map[string]any)["name"]
	if metricName != "http_requests_per_second" {
		t.Fatalf("QPS metric name=%v want http_requests_per_second", metricName)
	}

	scaleDown := getPath(t, hpa, "spec", "behavior", "scaleDown", "stabilizationWindowSeconds")
	if scaleDown != int(600) && scaleDown != int64(600) {
		t.Fatalf("scaleDown stabilization=%v want 600", scaleDown)
	}
}

func TestRenderDeployment_Autoscaling_CPUOnly(t *testing.T) {
	p := deployBaseProfile()
	o := DeploymentOptions{
		Name:      "cpu-scale",
		Namespace: "tau",
		Autoscaling: &AutoscalingOptions{
			MinReplicas: 1,
			MaxReplicas: 5,
		},
	}
	b, err := RenderDeployment(p, o)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	docs := decodeAll(t, b)
	if len(docs) != 2 {
		t.Fatalf("want Deployment + HPA (2 docs, no Service), got %d", len(docs))
	}
	hpa := docs[1]
	if hpa["kind"] != "HorizontalPodAutoscaler" {
		t.Fatalf("doc[1] kind=%v want HorizontalPodAutoscaler", hpa["kind"])
	}
	metrics := getPath(t, hpa, "spec", "metrics").([]any)
	if len(metrics) != 1 {
		t.Fatalf("want 1 metric (CPU only, no QPS), got %d", len(metrics))
	}
	if metrics[0].(map[string]any)["type"] != "Resource" {
		t.Fatalf("expected CPU Resource metric only")
	}
}

func TestRenderDeployment_Autoscaling_Validation(t *testing.T) {
	cases := []struct {
		name string
		a    AutoscalingOptions
		want string
	}{
		{"max zero", AutoscalingOptions{MaxReplicas: 0}, "MaxReplicas must be > 0"},
		{"max negative", AutoscalingOptions{MaxReplicas: -1}, "MaxReplicas must be > 0"},
		{"min negative", AutoscalingOptions{MinReplicas: -1, MaxReplicas: 5}, "MinReplicas must be >= 0"},
		{"min > max", AutoscalingOptions{MinReplicas: 10, MaxReplicas: 5}, "MinReplicas must be <= MaxReplicas"},
		{"target qps negative", AutoscalingOptions{MinReplicas: 1, MaxReplicas: 5, TargetQPS: -1}, "TargetQPS must be >= 0"},
		{"scale down negative", AutoscalingOptions{MinReplicas: 1, MaxReplicas: 5, ScaleDownDelay: -1}, "ScaleDownDelay must be >= 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := RenderDeployment(deployBaseProfile(), DeploymentOptions{
				Name: "x", Namespace: "y", Autoscaling: &c.a,
			})
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err=%q want substring %q", err.Error(), c.want)
			}
		})
	}
	t.Run("replicas and autoscaling mutually exclusive", func(t *testing.T) {
		_, err := RenderDeployment(deployBaseProfile(), DeploymentOptions{
			Name: "x", Namespace: "y", Replicas: 3,
			Autoscaling: &AutoscalingOptions{MinReplicas: 1, MaxReplicas: 5},
		})
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("expected mutual exclusion error, got: %v", err)
		}
	})
}

// TestRenderDeployment_ProfileWithoutQueue_IsRejected pins the fail-closed
// half of the Kueue contract.
//
// Kueue's Pod webhook initializes suspend from the pod-suspending-parent
// annotation, so the queue-name check never runs for an annotated pod and the
// scheduling gate is applied unconditionally. Such a pod is not degraded but
// unrecoverable: gated forever, with no Workload object for an operator to
// find (#1317). Refuse to render it.
func TestRenderDeployment_ProfileWithoutQueue_IsRejected(t *testing.T) {
	p := deployBaseProfile()
	p.Queue = ""
	_, err := RenderDeployment(p, DeploymentOptions{Name: "x", Namespace: "tau"})
	if err == nil {
		t.Fatal("Kueue-managed render without a LocalQueue must fail, not emit a permanently-gated Deployment")
	}
	if !strings.Contains(err.Error(), "LocalQueue is required") {
		t.Fatalf("error should name the missing LocalQueue, got: %v", err)
	}
}
