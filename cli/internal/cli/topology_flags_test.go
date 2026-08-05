package cli

import (
	"context"
	"errors"
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
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{
			Name:         "azure.research.eval.gpu",
			ClusterQueue: "sample-cq",
		},
		Options: runtopology.Options{QueueName: "sample-eval"},
	}
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

	if err := validateRenderedQueue(context.Background(), runner, "ray", manifest, jobrender.Options{}, queueValidationPolicyFor(preset, false)); err != nil {
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
	preset := &runtopology.ResolvedPreset{
		Preset:  runtopology.Preset{Name: "azure.research.eval.gpu"},
		Options: runtopology.Options{QueueName: "research-eval"},
	}
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

	err := validateRenderedQueue(context.Background(), runner, "ray", manifest, jobrender.Options{}, queueValidationPolicyFor(preset, false))
	if err == nil {
		t.Fatal("expected missing LocalQueue to fail")
	}
	for _, want := range []string{"azure.research.eval.gpu", "LocalQueue \"research-eval\"", "namespace \"ray\"", "ask the platform owner to validate preset azure.research.eval.gpu"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing LocalQueue error missing %q: %v", want, err)
		}
	}
}

func TestValidateRenderedQueueNamesQueueOverrideWhenMissing(t *testing.T) {
	preset := &runtopology.ResolvedPreset{
		Preset:  runtopology.Preset{Name: "azure.research.training.l"},
		Options: runtopology.Options{QueueName: "research-training"},
	}
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

	err := validateRenderedQueue(context.Background(), runner, "ray", manifest, jobrender.Options{}, queueValidationPolicyFor(preset, false))
	if err == nil {
		t.Fatal("expected missing override LocalQueue to fail")
	}
	for _, want := range []string{"--queue override", "LocalQueue \"sample-training\"", "azure.research.training.l"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("missing override LocalQueue error missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "targets LocalQueue") {
		t.Fatalf("override error should not blame the preset target: %v", err)
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
	if err == nil || !strings.Contains(err.Error(), "policy.topology") || !strings.Contains(err.Error(), "Tau will not choose scientific placement semantics automatically") {
		t.Fatalf("expected actionable connected topology preflight error, got %v", err)
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
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{
			Name:         "azure.research.training.l",
			TopologyName: "default-node-topology",
		},
		Options: runtopology.Options{QueueName: "preset-queue"},
	}
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
					queueValidationPolicyFor(preset, true),
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
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{
			Name:         "azure.research.training.l",
			TopologyName: "default-node-topology",
		},
		Options: runtopology.Options{QueueName: "preset-queue"},
	}
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
				queueValidationPolicyFor(preset, true),
			)
			if err == nil || !strings.Contains(err.Error(), "does not provide topology") {
				t.Fatalf("expected effective selector conflict, got %v", err)
			}
		})
	}
}

func TestValidateRenderedWorkspaceQueueCountsIndexedJobGPUDemand(t *testing.T) {
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{
			Name:         "azure.research.training.xl",
			TopologyName: "default-node-topology",
		},
		Options: runtopology.Options{QueueName: "preset-queue"},
	}
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "workspace", "get", "localqueue.kueue.x-k8s.io", "workspace-jobqueue", "-o", "json"): `{"metadata":{"name":"workspace-jobqueue"},"spec":{"clusterQueue":"workspace-cq"}}`,
			fakeRawKey("get", "clusterqueue.kueue.x-k8s.io", "workspace-cq", "-o", "json"):                        `{"metadata":{"name":"workspace-cq"},"spec":{"resourceGroups":[{"flavors":[{"name":"nd-h200-v5","resources":[{"name":"nvidia.com/gpu","nominalQuota":"8"}]}]}]}}`,
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
		queueValidationPolicyFor(preset, true),
	)
	if err == nil || !strings.Contains(err.Error(), "workload requests 16") || !strings.Contains(err.Error(), "at most 8") {
		t.Fatalf("expected indexed Job capacity failure, got %v", err)
	}
}

func TestTopologyFlagsApplyToPreservesOverrideWarningOrder(t *testing.T) {
	// Mirrors the production path: placement intent arrives from the config's
	// policy block, and "changed" means the config set a non-empty value.
	dispatch := runDispatchOptions{lane: "large-memory", queue: "sample-large-memory"}
	flags := runJobTopologyFlags(dispatch)
	changed := func(flag string) bool { return runJobTopologyFieldSet(dispatch, flag) }
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{Name: "azure.research.training.l"},
		Options: runtopology.Options{
			Team:      "research",
			Lane:      "training",
			QueueName: "research-training",
		},
	}
	var opts jobrender.Options
	warnings, err := flags.applyWithChanged(&opts, preset, changed)
	if err != nil {
		t.Fatal(err)
	}
	if opts.Team != "sample" {
		t.Fatalf("team = %q, want sample", opts.Team)
	}
	want := []string{
		`warning: --lane="large-memory" overrides preset azure.research.training.l value "training"`,
		`warning: --queue="sample-large-memory" overrides preset azure.research.training.l queue "research-training"; inferred --team=sample so the Kueue LocalQueue and team intent stay consistent`,
	}
	if !reflect.DeepEqual(warnings, want) {
		t.Fatalf("warnings = %#v, want %#v", warnings, want)
	}
}

func TestTopologyFlagsWarnsAndNormalizesLegacyGPUClass(t *testing.T) {
	flags := topologyFlags{gpuClass: "a100-nvlink-80gb"}
	var opts jobrender.Options

	warnings, err := flags.applyWithChanged(&opts, nil, func(string) bool { return false })
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

func TestTopologyFlagsQueueOverrideDisablesPresetTASContract(t *testing.T) {
	dispatch := runDispatchOptions{queue: "sample-training"}
	flags := runJobTopologyFlags(dispatch)
	changed := func(flag string) bool { return runJobTopologyFieldSet(dispatch, flag) }
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{Name: "azure.research.training.l"},
		Options: runtopology.Options{
			QueueName: "jobqueue",
		},
		Annotations: map[string]string{
			workloadmeta.AnnotationKueueTopology: "default-node-topology",
		},
	}
	var opts jobrender.Options
	if _, err := flags.applyWithChanged(&opts, preset, changed); err != nil {
		t.Fatal(err)
	}
	if !opts.DisableKueueTopologyAnnotations {
		t.Fatal("queue override retained preset TAS podset annotations")
	}
	if got := opts.Annotations[workloadmeta.AnnotationKueueTopology]; got != "" {
		t.Fatalf("queue override retained preset topology metadata %q", got)
	}
}

func TestTopologyFlagsMatchingQueuePreservesPresetTASContract(t *testing.T) {
	dispatch := runDispatchOptions{queue: "jobqueue"}
	flags := runJobTopologyFlags(dispatch)
	changed := func(flag string) bool { return runJobTopologyFieldSet(dispatch, flag) }
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{Name: "azure.research.training.l"},
		Options: runtopology.Options{
			QueueName: "jobqueue",
		},
		Annotations: map[string]string{
			workloadmeta.AnnotationKueueTopology: "default-node-topology",
		},
	}
	var opts jobrender.Options
	if _, err := flags.applyWithChanged(&opts, preset, changed); err != nil {
		t.Fatal(err)
	}
	if opts.DisableKueueTopologyAnnotations {
		t.Fatal("matching preset queue disabled TAS podset annotations")
	}
	if got := opts.Annotations[workloadmeta.AnnotationKueueTopology]; got != "default-node-topology" {
		t.Fatalf("matching preset queue topology metadata=%q", got)
	}
}

func TestResolveAutoQueueRequiresLiveDiscoveryForExplicitAuto(t *testing.T) {
	opts := jobrender.Options{QueueName: "auto"}
	_, err := (topologyFlags{}).resolveAutoQueue(context.Background(), nil, "ray", &opts, nil, 1, nil, "client", false)
	if err == nil || !strings.Contains(err.Error(), "requires live Kueue queue discovery") {
		t.Fatalf("expected explicit auto queue to reject client dry-run, got %v", err)
	}

	opts = jobrender.Options{}
	if warnings, err := (topologyFlags{}).resolveAutoQueue(context.Background(), nil, "ray", &opts, nil, 1, nil, "client", true); err != nil || len(warnings) != 0 {
		t.Fatalf("implicit auto should not block client dry-run, warnings=%v err=%v", warnings, err)
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

	explicitQueue := runDispatchOptions{queue: "custom-dra"}
	changed := func(flag string) bool { return runJobTopologyFieldSet(explicitQueue, flag) }
	opts = jobrender.Options{QueueName: "custom-dra"}
	configureGPUQueueModeWithChanged("dra", &opts, changed)
	if opts.QueueName != "custom-dra" {
		t.Fatalf("explicit queue was overwritten: %+v", opts)
	}
}

func TestTopologyFlagsApplyManagedFlavorSelector(t *testing.T) {
	dispatch := runDispatchOptions{}
	flags := runJobTopologyFlags(dispatch)
	changed := func(flag string) bool { return runJobTopologyFieldSet(dispatch, flag) }
	preset := &runtopology.ResolvedPreset{
		Preset: runtopology.Preset{
			Name:           "azure.research.large-memory.2x",
			ResourceFlavor: "nd-h200-v5-dra",
		},
	}
	var opts jobrender.Options
	if _, err := flags.applyWithChanged(&opts, preset, changed); err != nil {
		t.Fatal(err)
	}
	if got := opts.NodeSelector[runtopology.ManagedGPUSeriesLabel]; got != "nd-h200-v5" {
		t.Fatalf("managed series selector = %q, want nd-h200-v5", got)
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
}
