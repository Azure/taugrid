// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	runtopology "github.com/Azure/taugrid/core/topology"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type fakeRawRunner struct {
	outputs map[string]string
	errors  map[string]error
	calls   [][]string
}

func (f *fakeRawRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	k := fakeRawKey(args...)
	if err := f.errors[k]; err != nil {
		return "", err
	}
	out, ok := f.outputs[k]
	if !ok {
		return "", errors.New("unexpected kubectl args: " + strings.Join(args, " "))
	}
	return out, nil
}

func fakeRawKey(args ...string) string {
	return strings.Join(args, "\x00")
}

func TestValidateRenderedQueueChecksEffectiveQueue(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "sample-eval", "-o", "json"): `{"metadata":{"name":"sample-eval"},"spec":{"clusterQueue":"sample-cq"}}`,
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "sample-cq", "-o", "json"):              `{"metadata":{"name":"sample-cq"},"spec":{"resourceGroups":[]}}`,
		},
		errors: map[string]error{},
	}
	manifest := []byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: eval-smoke
  labels:
    kueue.x-k8s.io/queue-name: sample-eval
`)

	if err := validateRenderedQueue(context.Background(), runner, "ray", manifest, jobrender.Options{}, queueValidationPolicy{}); err != nil {
		t.Fatalf("validateRenderedQueue should accept existing queue: %v", err)
	}
	wantCalls := [][]string{
		{"-n", "ray", "get", "localqueue.kueue.x-k8s.io", "sample-eval", "-o", "json"},
		{"get", "clusterqueue.kueue.x-k8s.io", "sample-cq", "-o", "json"},
	}
	if !reflect.DeepEqual(runner.calls, wantCalls) {
		t.Fatalf("kubectl calls=%v want %v", runner.calls, wantCalls)
	}
}

func TestValidateRenderedQueueFailsClearlyWhenMissing(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{},
		errors: map[string]error{
			fakeRawKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "research-eval", "-o", "json"): errors.New("not found"),
		},
	}
	manifest := []byte(`metadata:
  labels:
    kueue.x-k8s.io/queue-name: research-eval
`)

	err := validateRenderedQueue(context.Background(), runner, "ray", manifest, jobrender.Options{}, queueValidationPolicy{})
	if err == nil {
		t.Fatal("expected missing LocalQueue to fail")
	}
	for _, want := range []string{"LocalQueue \"research-eval\"", "namespace \"ray\"", "ask the platform owner"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing LocalQueue error missing %q: %v", want, err)
		}
	}
}

func TestValidateRenderedQueueNamesQueueOverrideWhenMissing(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{},
		errors: map[string]error{
			fakeRawKey("-n", "ray", "get", "localqueue.kueue.x-k8s.io", "sample-training", "-o", "json"): errors.New("not found"),
		},
	}
	manifest := []byte(`metadata:
  labels:
    kueue.x-k8s.io/queue-name: sample-training
`)

	err := validateRenderedQueue(context.Background(), runner, "ray", manifest, jobrender.Options{}, queueValidationPolicy{})
	if err == nil {
		t.Fatal("expected missing override LocalQueue to fail")
	}
	for _, want := range []string{"LocalQueue \"sample-training\"", "namespace \"ray\""} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing override LocalQueue error missing %q: %v", want, err)
		}
	}
}

func TestValidateRenderedQueueAcceptsExplicitTopologyOnTASOnlyFlavor(t *testing.T) {
	for _, tc := range []struct {
		name       string
		annotation string
		value      string
	}{
		{name: "independent", annotation: "kueue.x-k8s.io/podset-unconstrained-topology", value: "true"},
		{name: "single-node-nvlink", annotation: "kueue.x-k8s.io/podset-required-topology", value: "kubernetes.io/hostname"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRawRunner{
				outputs: map[string]string{
					fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): `{"metadata":{"name":"jobqueue"},"spec":{"clusterQueue":"workspace-cq"}}`,
					fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):              `{"metadata":{"name":"workspace-cq"},"spec":{"resourceGroups":[{"flavors":[{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]}]}]}}`,
					fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"):              `{"metadata":{"name":"nd-h200-v5"},"spec":{"topologyName":"default-node-topology"}}`,
				},
				errors: map[string]error{},
			}
			manifest := []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: jobqueue
spec:
  template:
    metadata:
      annotations:
        ` + tc.annotation + `: "` + tc.value + `"
    spec:
      containers:
      - resources:
          limits:
            nvidia.com/gpu: 1
`)
			if err := validateRenderedQueue(context.Background(), runner, "workspace", manifest, jobrender.Options{}, queueValidationPolicy{}); err != nil {
				t.Fatalf("explicit %s topology should pass connected preflight: %v", tc.name, err)
			}
		})
	}
}

func TestValidateRenderedQueueRejectsMissingTopologyOnTASOnlyFlavor(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): `{"metadata":{"name":"jobqueue"},"spec":{"clusterQueue":"workspace-cq"}}`,
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{"metadata":{"name":"workspace-cq"},"spec":{"resourceGroups":[{"flavors":[
				{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]},
				{"name":"tau-system","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
			]}]}}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): `{"metadata":{"name":"nd-h200-v5"},"spec":{"topologyName":"default-node-topology"}}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "tau-system", "-o", "json"): `{"metadata":{"name":"tau-system"},"spec":{"topologyName":"default-node-topology"}}`,
		},
		errors: map[string]error{},
	}

	manifest := []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: jobqueue
spec:
  template:
    spec:
      containers:
      - resources:
          limits:
            nvidia.com/gpu: 1
`)

	err := validateRenderedQueue(context.Background(), runner, "workspace", manifest, jobrender.Options{}, queueValidationPolicy{})
	if err == nil || !strings.Contains(err.Error(), "policy.topology") || !strings.Contains(err.Error(), runtopology.RequiredTopologyAnnotation) {
		t.Fatalf("expected actionable connected topology preflight error, got %v", err)
	}
}

func TestPrepareGeneratedQueueTopologyInjectsManagedFlavorRequirement(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): `{"metadata":{"name":"jobqueue"},"spec":{"clusterQueue":"workspace-cq"}}`,
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):              `{"metadata":{"name":"workspace-cq"},"spec":{"resourceGroups":[{"flavors":[{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"16"}]}]}]}}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): `{
				"metadata":{
					"name":"nd-h200-v5",
					"annotations":{"kueue.x-k8s.io/podset-required-topology":"kubernetes.io/hostname"}
				},
				"spec":{"topologyName":"default-node-topology"}
			}`,
		},
		errors: map[string]error{},
	}
	opts := jobrender.Options{QueueName: "jobqueue"}
	render := func() ([]byte, error) {
		annotation := ""
		if opts.RequiredTopology != "" {
			annotation = "\n      annotations:\n        " + runtopology.RequiredTopologyAnnotation + ": " + opts.RequiredTopology
		}
		return []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: jobqueue
spec:
  template:
    metadata:` + annotation + `
    spec:
      containers:
      - resources:
          limits:
            nvidia.com/gpu: 1
`), nil
	}
	initial, err := render()
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := prepareGeneratedQueueTopology(
		context.Background(), runner, "workspace", initial, &opts, queueValidationPolicy{}, render)
	if err != nil {
		t.Fatal(err)
	}
	if opts.RequiredTopology != "kubernetes.io/hostname" {
		t.Fatalf("resolved required topology=%q", opts.RequiredTopology)
	}
	contract, err := renderedQueueContractFromManifest(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !contract.TopologyRequest {
		t.Fatalf("generated workload still lacks topology request:\n%s", rendered)
	}
}

func TestPrepareGeneratedQueueTopologyReenablesAnnotationsAfterQueueOverride(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "custom-queue", "-o", "json"): `{"metadata":{"name":"custom-queue"},"spec":{"clusterQueue":"custom-cq"}}`,
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "custom-cq", "-o", "json"):                     `{"metadata":{"name":"custom-cq"},"spec":{"resourceGroups":[{"flavors":[{"name":"custom-gpu","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}]}]}}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "custom-gpu", "-o", "json"): `{
				"metadata":{
					"name":"custom-gpu",
					"annotations":{"kueue.x-k8s.io/podset-required-topology":"kubernetes.io/hostname"}
				},
				"spec":{"topologyName":"custom-topology"}
			}`,
		},
		errors: map[string]error{},
	}
	opts := jobrender.Options{
		QueueName:                       "custom-queue",
		DisableKueueTopologyAnnotations: true,
	}
	render := func() ([]byte, error) {
		annotation := ""
		if opts.RequiredTopology != "" && !opts.DisableKueueTopologyAnnotations {
			annotation = "\n      annotations:\n        " + runtopology.RequiredTopologyAnnotation + ": " + opts.RequiredTopology
		}
		return []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: custom-queue
spec:
  template:
    metadata:` + annotation + `
    spec:
      containers:
      - resources:
          limits:
            nvidia.com/gpu: 1
`), nil
	}
	initial, err := render()
	if err != nil {
		t.Fatal(err)
	}
	rendered, err := prepareGeneratedQueueTopology(
		context.Background(), runner, "workspace", initial, &opts, queueValidationPolicy{}, render)
	if err != nil {
		t.Fatal(err)
	}
	if opts.DisableKueueTopologyAnnotations {
		t.Fatal("managed queue requirement did not re-enable Kueue topology annotations")
	}
	contract, err := renderedQueueContractFromManifest(rendered)
	if err != nil {
		t.Fatal(err)
	}
	if !contract.TopologyRequest {
		t.Fatalf("queue override rerender still lacks topology request:\n%s", rendered)
	}
}

func TestRenderedQueueContractIgnoresTopologyOutsideGPUPodTemplate(t *testing.T) {
	manifest := []byte(`apiVersion: ray.io/v1
kind: RayJob
metadata:
  annotations:
    kueue.x-k8s.io/podset-unconstrained-topology: "true"
spec:
  rayClusterSpec:
    headGroupSpec:
      template:
        metadata:
          annotations:
            kueue.x-k8s.io/podset-unconstrained-topology: "true"
        spec:
          containers:
          - name: head
    workerGroupSpecs:
    - replicas: 1
      template:
        spec:
          containers:
          - name: worker
            resources:
              limits:
                nvidia.com/gpu: 1
`)
	contract, err := renderedQueueContractFromManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if contract.TopologyRequest {
		t.Fatal("workload-level or CPU-only pod annotations satisfied GPU topology intent")
	}
}

func TestRenderedQueueContractCollectsGPUTolerations(t *testing.T) {
	manifest := []byte(`apiVersion: batch/v1
kind: Job
spec:
  template:
    spec:
      tolerations:
      - key: sku
        operator: Equal
        value: gpu
        effect: NoSchedule
      containers:
      - resources:
          limits:
            nvidia.com/gpu: 1
`)
	contract, err := renderedQueueContractFromManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(contract.PodTolerations) != 1 || len(contract.PodTolerations[0]) != 1 {
		t.Fatalf("GPU pod tolerations = %#v", contract.PodTolerations)
	}
	got := contract.PodTolerations[0][0]
	if got.Key != "sku" || got.Operator != "Equal" || got.Value != "gpu" || got.Effect != "NoSchedule" {
		t.Fatalf("GPU pod toleration = %#v", got)
	}
}

func TestValidateRenderedWorkspaceQueueChecksTASForJobAndRay(t *testing.T) {
	manifests := map[string]struct {
		resourceName string
		manifest     []byte
	}{
		"managed-job": {
			resourceName: "nvidia.com/gpu",
			manifest: []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: workspace-jobqueue
spec:
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-unconstrained-topology: "true"
    spec:
      containers:
      - resources:
          limits:
            nvidia.com/gpu: 1
`),
		},
		"managed-ray": {
			resourceName: "nvidia.com/gpu",
			manifest: []byte(`apiVersion: ray.io/v1
kind: RayJob
metadata:
  labels:
    kueue.x-k8s.io/queue-name: workspace-jobqueue
spec:
  rayClusterSpec:
    workerGroupSpecs:
    - replicas: 2
      template:
        metadata:
          annotations:
            kueue.x-k8s.io/podset-unconstrained-topology: "true"
        spec:
          containers:
          - resources:
              limits:
                nvidia.com/gpu: 1
`),
		},
		"managed-job-mig": {
			resourceName: "nvidia.com/mig-1g.10gb",
			manifest: []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: workspace-jobqueue
spec:
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-unconstrained-topology: "true"
    spec:
      containers:
      - resources:
          limits:
            nvidia.com/mig-1g.10gb: 1
`),
		},
		"managed-ray-mig": {
			resourceName: "nvidia.com/mig-1g.10gb",
			manifest: []byte(`apiVersion: ray.io/v1
kind: RayJob
metadata:
  labels:
    kueue.x-k8s.io/queue-name: workspace-jobqueue
spec:
  rayClusterSpec:
    workerGroupSpecs:
    - replicas: 2
      template:
        metadata:
          annotations:
            kueue.x-k8s.io/podset-unconstrained-topology: "true"
        spec:
          containers:
          - resources:
              limits:
                nvidia.com/mig-1g.10gb: 1
`),
		},
	}
	for workload, workloadCase := range manifests {
		for _, tc := range []struct {
			name         string
			topologyName string
			wantErr      bool
		}{
			{name: "tas-capable", topologyName: "default-node-topology"},
			{name: "topology-free", wantErr: true},
		} {
			t.Run(workload+"/"+tc.name, func(t *testing.T) {
				queueFlavor := "nd-h200-v5"
				if tc.wantErr {
					queueFlavor = "topology-free"
				}
				runner := &fakeRawRunner{
					outputs: map[string]string{
						fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "workspace-jobqueue", "-o", "json"): `{"metadata":{"name":"workspace-jobqueue"},"spec":{"clusterQueue":"workspace-cq"}}`,
						fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):                        `{"metadata":{"name":"workspace-cq"},"spec":{"resourceGroups":[{"flavors":[{"name":"` + queueFlavor + `","resources":[{"name":"` + workloadCase.resourceName + `","nominalQuota":"8"}]}]}]}}`,
						fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", queueFlavor, "-o", "json"):                         `{"metadata":{"name":"` + queueFlavor + `"},"spec":{"topologyName":"` + tc.topologyName + `"}}`,
					},
					errors: map[string]error{},
				}
				opts := jobrender.Options{
					QueueName:       "workspace-jobqueue",
					GPUResourceName: "nvidia.com/gpu",
				}
				err := validateRenderedQueue(
					context.Background(),
					runner,
					"workspace",
					workloadCase.manifest,
					opts,
					queueValidationPolicy{TopologyName: "default-node-topology", CatalogTopologyContract: true},
				)
				if tc.wantErr {
					if err == nil || !strings.Contains(err.Error(), `does not provide topology "default-node-topology"`) {
						t.Fatalf("expected topology mismatch, got %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("validateRenderedQueue: %v", err)
				}
			})
		}
	}
}

func TestValidateRenderedWorkspaceQueueUsesEffectiveJobAndRayNodeSelectors(t *testing.T) {
	manifests := map[string][]byte{
		"managed-job": []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: workspace-jobqueue
spec:
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-unconstrained-topology: "true"
    spec:
      nodeSelector:
        kueue.azure.com/gpu-series: ndm-a100-v4
      containers:
      - resources:
          limits:
            nvidia.com/gpu: 1
`),
		"managed-ray": []byte(`apiVersion: ray.io/v1
kind: RayJob
metadata:
  labels:
    kueue.x-k8s.io/queue-name: workspace-jobqueue
spec:
  rayClusterSpec:
    workerGroupSpecs:
    - replicas: 2
      template:
        metadata:
          annotations:
            kueue.x-k8s.io/podset-unconstrained-topology: "true"
        spec:
          nodeSelector:
            kueue.azure.com/gpu-series: ndm-a100-v4
          containers:
          - resources:
              limits:
                nvidia.com/gpu: 1
`),
	}
	for workload, manifest := range manifests {
		t.Run(workload, func(t *testing.T) {
			runner := &fakeRawRunner{
				outputs: map[string]string{
					fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "workspace-jobqueue", "-o", "json"): `{"metadata":{"name":"workspace-jobqueue"},"spec":{"clusterQueue":"workspace-cq"}}`,
					fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"): `{"metadata":{"name":"workspace-cq"},"spec":{"resourceGroups":[{"flavors":[
						{"name":"a100-plain","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]},
						{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}
					]}]}}`,
					fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "a100-plain", "-o", "json"): `{"metadata":{"name":"a100-plain"},"spec":{"nodeLabels":{"kueue.azure.com/gpu-series":"ndm-a100-v4"}}}`,
					fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"): `{"metadata":{"name":"nd-h200-v5"},"spec":{"nodeLabels":{"kueue.azure.com/gpu-series":"nd-h200-v5"},"topologyName":"default-node-topology"}}`,
				},
				errors: map[string]error{},
			}
			opts := jobrender.Options{
				QueueName:       "workspace-jobqueue",
				GPUResourceName: "nvidia.com/gpu",
			}
			err := validateRenderedQueue(
				context.Background(),
				runner,
				"workspace",
				manifest,
				opts,
				queueValidationPolicy{TopologyName: "default-node-topology", CatalogTopologyContract: true},
			)
			if err == nil || !strings.Contains(err.Error(), "does not provide topology") {
				t.Fatalf("expected effective selector conflict, got %v", err)
			}
		})
	}
}

func TestValidateRenderedWorkspaceQueueCountsIndexedJobGPUDemand(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "workspace-jobqueue", "-o", "json"): `{"metadata":{"name":"workspace-jobqueue"},"spec":{"clusterQueue":"workspace-cq"}}`,
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):                        `{"metadata":{"name":"workspace-cq"},"spec":{"resourceGroups":[{"flavors":[{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}]}]}}`,
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "nd-h200-v5", "-o", "json"):                        `{"metadata":{"name":"nd-h200-v5"},"spec":{"topologyName":"default-node-topology"}}`,
		},
		errors: map[string]error{},
	}
	manifest := []byte(`apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: workspace-jobqueue
spec:
  parallelism: 2
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-unconstrained-topology: "true"
    spec:
      containers:
      - resources:
          limits:
            nvidia.com/gpu: 8
`)
	opts := jobrender.Options{
		QueueName:       "workspace-jobqueue",
		GPUResourceName: "nvidia.com/gpu",
	}
	err := validateRenderedQueue(
		context.Background(),
		runner,
		"workspace",
		manifest,
		opts,
		queueValidationPolicy{TopologyName: "default-node-topology", CatalogTopologyContract: true},
	)
	if err == nil || !strings.Contains(err.Error(), "workload requests 16") || !strings.Contains(err.Error(), "at most 8") {
		t.Fatalf("expected indexed Job capacity failure, got %v", err)
	}
}

func TestTopologyFlagsWarnsAndNormalizesLegacyGPUClass(t *testing.T) {
	flags := topologyFlags{gpuClass: "a100-nvlink-80gb"}
	var opts jobrender.Options

	warnings, err := flags.applyWithChanged(&opts, func(string) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	if opts.GPUClass != runtopology.GPUClassA10080GB {
		t.Fatalf("GPUClass=%q want %q", opts.GPUClass, runtopology.GPUClassA10080GB)
	}
	if len(warnings) != 1 ||
		!strings.Contains(warnings[0], `"a100-nvlink-80gb"`) ||
		!strings.Contains(warnings[0], `"`+runtopology.GPUClassA10080GB+`"`) {
		t.Fatalf("warnings=%#v", warnings)
	}
}

func TestResolveAutoQueueRequiresLiveDiscoveryForExplicitAuto(t *testing.T) {
	opts := jobrender.Options{QueueName: "auto"}
	_, err := (topologyFlags{}).resolveAutoQueueFromManifest(context.Background(), nil, "ray", &opts, nil, "client", true, false)
	if err == nil || !strings.Contains(err.Error(), "requires live Kueue queue discovery") {
		t.Fatalf("expected explicit auto queue to reject client dry-run, got %v", err)
	}

	opts = jobrender.Options{}
	if warnings, err := (topologyFlags{}).resolveAutoQueueFromManifest(context.Background(), nil, "ray", &opts, nil, "client", false, true); err != nil || len(warnings) != 0 {
		t.Fatalf("implicit auto should not block client dry-run, warnings=%v err=%v", warnings, err)
	}
}

func TestResolveAccessibleQueueNamespacePreservesAutoSentinel(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("get", "namespaces", "-l", "kueue.x-k8s.io/default-local-queue", "-o", "json"): `{"items":[{
				"metadata":{"name":"workspace","labels":{"kueue.x-k8s.io/default-local-queue":"jobqueue"}}
			}]}`,
			fakeRawKey("auth", "can-i", "create", "jobs.batch", "-n", "workspace"):                      "yes",
			fakeRawKey("auth", "can-i", "get", "localqueues.kueue.x-k8s.io", "-n", "workspace"):         "yes",
			fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): `{"metadata":{"name":"jobqueue"},"spec":{"clusterQueue":"jobqueue"}}`,
		},
		errors: map[string]error{},
	}
	namespace := ""
	opts := jobrender.Options{QueueName: "auto"}
	if _, err := resolveAccessibleQueueNamespace(context.Background(), runner, false, &namespace, &opts, "", "jobs.batch", false); err != nil {
		t.Fatal(err)
	}
	if namespace != "workspace" {
		t.Fatalf("namespace = %q, want workspace", namespace)
	}
	if opts.QueueName != "auto" {
		t.Fatalf("queue = %q, want auto sentinel preserved for capacity discovery", opts.QueueName)
	}

	namespace = ""
	opts = jobrender.Options{}
	if _, err := resolveAccessibleQueueNamespace(context.Background(), runner, false, &namespace, &opts, "", "jobs.batch", true); err != nil {
		t.Fatal(err)
	}
	if opts.QueueName != "" {
		t.Fatalf("implicit auto queue = %q, want empty sentinel preserved for capacity discovery", opts.QueueName)
	}
}

func TestResolveAutoQueueUsesRenderedSchedulingContract(t *testing.T) {
	const migResource = "nvidia.com/mig-1g.10gb"
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "workspace", "get", "localqueues.kueue.x-k8s.io", "-o", "json"): `{"items":[{
				"metadata":{"name":"jobqueue"},"spec":{"clusterQueue":"jobqueue"}
			}]}`,
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "jobqueue", "-o", "json"): fmt.Sprintf(`{
				"metadata":{"name":"jobqueue"},
				"spec":{"resourceGroups":[{"coveredResources":["%s"],"flavors":[{"name":"opaque-a100","resources":[{"name":"%s","nominalQuota":"1"}]}]}]}
			}`, migResource, migResource),
			fakeRawKey("get", "resourceflavor.kueue.x-k8s.io", "opaque-a100", "-o", "json"): fmt.Sprintf(`{
				"metadata":{"name":"opaque-a100"},
				"spec":{
					"nodeLabels":{"%s":"a100-80gb","nvidia.com/mig.config":"all-1g.10gb"},
					"nodeTaints":[{"key":"sku","value":"gpu","effect":"NoSchedule"}],
					"topologyName":"default-node-topology"
				}
			}`, workloadmeta.LabelGPUClass),
		},
		errors: map[string]error{},
	}
	opts := jobrender.Options{QueueName: "auto"}
	rendered := []byte(fmt.Sprintf(`
apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: auto
    %s: a100-80gb
spec:
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-unconstrained-topology: "true"
    spec:
      nodeSelector:
        nvidia.com/mig.config: all-1g.10gb
      tolerations:
        - key: sku
          value: gpu
          effect: NoSchedule
      containers:
        - name: worker
          resources:
            limits:
              %s: 1
`, workloadmeta.LabelGPUClass, migResource))
	warnings, err := (topologyFlags{}).resolveAutoQueueFromManifest(
		context.Background(), runner, "workspace", &opts, rendered, "", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if opts.QueueName != "jobqueue" {
		t.Fatalf("queue = %q, want jobqueue", opts.QueueName)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "opaque-a100") {
		t.Fatalf("warnings = %v, want selected flavor", warnings)
	}
}

func TestRenderedQueueContractReadsCanonicalGPUClassMetadata(t *testing.T) {
	contract, err := renderedQueueContractFromManifest([]byte(fmt.Sprintf(`
apiVersion: batch/v1
kind: Job
metadata:
  labels:
    kueue.x-k8s.io/queue-name: jobqueue
    %s: a100-nvlink-80gb
spec:
  template:
    spec:
      containers:
        - resources:
            limits:
              nvidia.com/gpu: 1
`, workloadmeta.LabelGPUClass)))
	if err != nil {
		t.Fatal(err)
	}
	if contract.GPUClass != runtopology.GPUClassA10080GB {
		t.Fatalf("GPUClass=%q, want %q", contract.GPUClass, runtopology.GPUClassA10080GB)
	}
}

func TestDRAQueueModeUsesParallelQueueUnlessExplicitlyOverridden(t *testing.T) {
	noQueue := runDispatchOptions{}
	unchanged := func(flag string) bool { return runJobTopologyFieldSet(noQueue, flag) }

	opts := jobrender.Options{QueueName: runtopology.SharedGPUQueueName}
	configureGPUQueueModeWithChanged("dra", &opts, unchanged)
	if opts.QueueName != runtopology.SharedDRAQueueName || opts.GPUResourceName != "gpu.nvidia.com" {
		t.Fatalf("DRA options = %+v", opts)
	}
	if !opts.DisableKueueTopologyAnnotations {
		t.Fatal("DRA mode must disable TAS annotations")
	}

	explicitQueue := runDispatchOptions{runPlacement: runPlacement{queue: "custom-dra"}}
	changed := func(flag string) bool { return runJobTopologyFieldSet(explicitQueue, flag) }
	opts = jobrender.Options{QueueName: "custom-dra"}
	configureGPUQueueModeWithChanged("dra", &opts, changed)
	if opts.QueueName != "custom-dra" {
		t.Fatalf("explicit queue was overwritten: %+v", opts)
	}
}

func TestRenderedQueueContractCountsRayWorkerReplicas(t *testing.T) {
	manifest := []byte(`apiVersion: ray.io/v1
kind: RayJob
metadata:
  labels:
    kueue.x-k8s.io/queue-name: research-training
spec:
  rayClusterSpec:
    headGroupSpec:
      template:
        spec:
          containers:
            - name: ray-head
              resources:
                requests:
                  nvidia.com/gpu: 4
    workerGroupSpecs:
      - replicas: 9
        template:
          spec:
            containers:
              - name: ray-worker
                resources:
                  requests:
                    nvidia.com/gpu: 4
`)
	contract, err := renderedQueueContractFromManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if contract.QueueName != "research-training" {
		t.Fatalf("queue=%q", contract.QueueName)
	}
	if contract.GPUCount != 40 {
		t.Fatalf("gpu count=%d, want 40", contract.GPUCount)
	}
	if contract.TopologyRequest {
		t.Fatal("manifest without policy.topology reported a TAS request")
	}
}

func TestRenderedQueueContractCountsDRAClaims(t *testing.T) {
	manifest := []byte(`apiVersion: ray.io/v1
kind: RayJob
metadata:
  labels:
    kueue.x-k8s.io/queue-name: jobqueue-dra
spec:
  rayClusterSpec:
    headGroupSpec:
      template:
        spec:
          resourceClaims:
            - name: gpu
              resourceClaimTemplateName: full-gpu
    workerGroupSpecs:
      - replicas: 2
        template:
          spec:
            resourceClaims:
              - name: gpu
                resourceClaimTemplateName: ds-8gpus
`)
	contract, err := renderedQueueContractFromManifest(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if contract.GPUCount != 17 {
		t.Fatalf("DRA GPU count = %d, want 17", contract.GPUCount)
	}
	if contract.GPUResourceName != "gpu.nvidia.com" {
		t.Fatalf("DRA GPU resource = %q, want gpu.nvidia.com", contract.GPUResourceName)
	}
	if contract.TopologyRequest {
		t.Fatal("DRA manifest without TAS annotations reported a topology request")
	}
}
