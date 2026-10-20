// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gpuinventory

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeReader struct {
	raw  map[string][]byte
	errs map[string]error
}

func (f *fakeReader) read(key string) ([]byte, error) {
	if err := f.errs[key]; err != nil {
		return nil, err
	}
	if raw := f.raw[key]; raw != nil {
		return raw, nil
	}
	return []byte(`{"items":[]}`), nil
}

func (f *fakeReader) ListPods(context.Context, string) ([]byte, error) {
	return f.read("pods")
}

func (f *fakeReader) ListJobs(context.Context, string) ([]byte, error) {
	return f.read("jobs")
}

func (f *fakeReader) ListRayJobs(context.Context, string) ([]byte, error) {
	return f.read("rayJobs")
}

func (f *fakeReader) ListRayServices(context.Context, string) ([]byte, error) {
	return f.read("rayServices")
}

func (f *fakeReader) ListRayClusters(context.Context, string) ([]byte, error) {
	return f.read("rayClusters")
}

func (f *fakeReader) ListReplicaSets(context.Context, string) ([]byte, error) {
	return f.read("replicaSets")
}

func (f *fakeReader) ListDeployments(context.Context, string) ([]byte, error) {
	return f.read("deployments")
}

func (f *fakeReader) ListStatefulSets(context.Context, string) ([]byte, error) {
	return f.read("statefulSets")
}

func (f *fakeReader) ListDaemonSets(context.Context) ([]byte, error) {
	return f.read("daemonSets")
}

func (f *fakeReader) ListWorkloads(context.Context, string) ([]byte, error) {
	return f.read("workloads")
}

func TestInventoryCanonicalizesOwnerChains(t *testing.T) {
	controller := true
	objects := map[string][]map[string]any{
		"jobs": {
			ownerFixture("Job", "tau-job", "job-tau", "team-a", nil, tauLabels("queue-a")),
			ownerFixture("Job", "external-job", "job-external", "team-a", nil, map[string]string{queueLabel: "queue-a"}),
		},
		"rayJobs": {
			ownerFixture("RayJob", "ray-job", "rayjob-1", "team-a", nil, tauLabels("queue-a")),
		},
		"rayServices": {
			ownerFixture("RayService", "ray-service", "rayservice-1", "team-a", nil, tauLabels("queue-a")),
		},
		"rayClusters": {
			ownerFixture("RayCluster", "service-cluster", "raycluster-service", "team-a", []map[string]any{
				ownerRef("RayService", "ray-service", "rayservice-1", &controller),
			}, nil),
			ownerFixture("RayCluster", "job-cluster", "raycluster-job", "team-a", []map[string]any{
				ownerRef("RayJob", "ray-job", "rayjob-1", &controller),
			}, nil),
			ownerFixture("RayCluster", "standalone-cluster", "raycluster-standalone", "team-a", nil, tauLabels("queue-a")),
		},
		"replicaSets": {
			ownerFixture("ReplicaSet", "web-rs", "rs-1", "team-a", []map[string]any{
				ownerRef("Deployment", "web", "deployment-1", &controller),
			}, nil),
		},
		"deployments": {
			ownerFixture("Deployment", "web", "deployment-1", "team-a", nil, nil),
		},
		"statefulSets": {
			ownerFixture("StatefulSet", "database", "stateful-1", "team-a", nil, nil),
		},
		"daemonSets": {
			ownerFixture("DaemonSet", "monitor", "daemon-1", "team-a", nil, nil),
		},
	}
	pods := []map[string]any{
		podFixture("tau-job-pod", "pod-1", "team-a", "Running", "node-1", ownerRef("Job", "tau-job", "job-tau", &controller), gpuRequests("1")),
		podFixture("external-job-pod", "pod-2", "team-a", "Running", "node-1", ownerRef("Job", "external-job", "job-external", &controller), gpuRequests("1")),
		podFixture("ray-service-pod", "pod-3", "team-a", "Running", "node-1", ownerRef("RayCluster", "service-cluster", "raycluster-service", &controller), gpuRequests("1")),
		podFixture("ray-job-pod", "pod-4", "team-a", "Running", "node-1", ownerRef("RayCluster", "job-cluster", "raycluster-job", &controller), gpuRequests("1")),
		podFixture("ray-cluster-pod", "pod-5", "team-a", "Running", "node-1", ownerRef("RayCluster", "standalone-cluster", "raycluster-standalone", &controller), gpuRequests("1")),
		podFixture("deployment-pod", "pod-6", "team-a", "Running", "node-1", ownerRef("ReplicaSet", "web-rs", "rs-1", &controller), gpuRequests("1")),
		podFixture("stateful-pod", "pod-7", "team-a", "Running", "node-1", ownerRef("StatefulSet", "database", "stateful-1", &controller), gpuRequests("1")),
		podFixture("daemon-pod", "pod-8", "team-a", "Running", "node-1", ownerRef("DaemonSet", "monitor", "daemon-1", &controller), gpuRequests("1")),
		podFixture("standalone-pod", "pod-9", "team-a", "Pending", "", nil, gpuRequests("1")),
		podFixture("dangling-pod", "pod-10", "team-a", "Pending", "", ownerRef("Job", "missing-job", "missing-job-uid", &controller), gpuRequests("1")),
		podFixture("unknown-pod", "pod-11", "team-a", "Pending", "", ownerRef("Mystery", "mystery-owner", "mystery-uid", &controller), gpuRequests("1")),
	}
	reader := readerFixture(objects, pods)
	snapshot, err := Inventory(context.Background(), reader, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Summary.Pods != len(pods) || snapshot.Summary.RequestedFullGPUs != int64(len(pods)) {
		t.Fatalf("summary = %+v", snapshot.Summary)
	}
	want := map[string]string{
		"tau-job":            "Job/resolved",
		"external-job":       "Job/resolved",
		"ray-service":        "RayService/resolved",
		"ray-job":            "RayJob/resolved",
		"standalone-cluster": "RayCluster/standalone",
		"web":                "Deployment/resolved",
		"database":           "StatefulSet/resolved",
		"monitor":            "DaemonSet/resolved",
		"standalone-pod":     "Pod/standalone",
		"missing-job":        "Job/dangling",
		"mystery-owner":      "Mystery/unknown_kind",
	}
	for _, workload := range snapshot.Workloads {
		key := workload.Owner.Name
		got := workload.Owner.Kind + "/" + workload.Owner.Resolution
		if expected, ok := want[key]; !ok || got != expected {
			t.Fatalf("owner %q = %q, want %q", key, got, expected)
		}
		delete(want, key)
	}
	if len(want) != 0 {
		t.Fatalf("missing owners: %#v", want)
	}
	managed := workloadsByOwner(snapshot.Workloads)
	if !managed["tau-job"].Managed || managed["external-job"].Managed {
		t.Fatalf("managed states = tau %t external %t", managed["tau-job"].Managed, managed["external-job"].Managed)
	}
}

func TestInventoryTreatsTauServiceLabelsAsManaged(t *testing.T) {
	controller := true
	serveLabels := map[string]string{
		workloadmeta.LabelService: "inference",
		queueLabel:                "serve-queue",
		workloadmeta.LabelProfile: "serve-profile",
	}
	rayService := ownerFixture("RayService", "inference", "service-1", "team-a", nil, serveLabels)
	rayCluster := ownerFixture("RayCluster", "inference-cluster", "cluster-1", "team-a", []map[string]any{
		ownerRef("RayService", "inference", "service-1", &controller),
	}, serveLabels)
	pod := podFixture(
		"inference-worker",
		"pod-1",
		"team-a",
		"Running",
		"node-1",
		ownerRef("RayCluster", "inference-cluster", "cluster-1", &controller),
		gpuRequests("1"),
	)
	reader := readerFixture(map[string][]map[string]any{
		"rayServices": {rayService},
		"rayClusters": {rayCluster},
	}, []map[string]any{pod})

	for name, opts := range map[string]Options{
		"fleet":     {},
		"workspace": {Namespace: "team-a", LocalQueue: "serve-queue", ManagedOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot, err := Inventory(context.Background(), reader, opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Workloads) != 1 {
				t.Fatalf("workloads = %+v", snapshot.Workloads)
			}
			workload := snapshot.Workloads[0]
			if !workload.Managed || workload.Queue != "serve-queue" || workload.Profile != "serve-profile" ||
				workload.Owner.Kind != "RayService" || workload.Owner.Name != "inference" {
				t.Fatalf("workload = %+v", workload)
			}
		})
	}

	reader.errs["rayServices"] = apierrors.NewForbidden(
		schema.GroupResource{Group: "ray.io", Resource: "rayservices"},
		"",
		errors.New("sensitive authorization detail"),
	)
	for name, opts := range map[string]Options{
		"fleet-forbidden-service":     {},
		"workspace-forbidden-service": {Namespace: "team-a", LocalQueue: "serve-queue", ManagedOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot, err := Inventory(context.Background(), reader, opts)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Workloads) != 1 || !snapshot.Partial {
				t.Fatalf("snapshot = %+v", snapshot)
			}
			workload := snapshot.Workloads[0]
			if !workload.Managed || workload.Queue != "serve-queue" || workload.Profile != "serve-profile" ||
				workload.Owner.Kind != "RayCluster" || workload.Owner.Name != "inference-cluster" ||
				workload.Owner.Resolution != "source_unavailable" ||
				workload.Owner.RawOwnerKind != "RayService" || workload.Owner.RawOwnerName != "inference" {
				t.Fatalf("workload = %+v", workload)
			}
			sourceStates := map[string]string{}
			for _, source := range snapshot.Sources {
				sourceStates[source.Source] = source.State
			}
			if sourceStates["rayServices"] != "forbidden" {
				t.Fatalf("source states = %#v", sourceStates)
			}
		})
	}
}

func TestInventoryCanonicalizesRayJobSubmitterAndWorkersTogether(t *testing.T) {
	controller := true
	rayJob := ownerFixture("RayJob", "training", "rayjob-1", "team-a", nil, tauLabels("queue-a"))
	submitterJob := ownerFixture("Job", "training-submitter", "job-1", "team-a", []map[string]any{
		ownerRef("RayJob", "training", "rayjob-1", &controller),
	}, nil)
	cronChild := ownerFixture("Job", "scheduled-child", "job-cron", "team-a", []map[string]any{
		ownerRef("CronJob", "schedule", "cron-1", &controller),
	}, nil)
	rayCluster := ownerFixture("RayCluster", "training-cluster", "cluster-1", "team-a", []map[string]any{
		ownerRef("RayJob", "training", "rayjob-1", &controller),
	}, nil)
	pods := []map[string]any{
		podFixture("submitter", "pod-submitter", "team-a", "Running", "node-1", ownerRef("Job", "training-submitter", "job-1", &controller), gpuRequests("1")),
		podFixture("worker", "pod-worker", "team-a", "Running", "node-2", ownerRef("RayCluster", "training-cluster", "cluster-1", &controller), gpuRequests("2")),
		podFixture("cron-child", "pod-cron", "team-a", "Running", "node-3", ownerRef("Job", "scheduled-child", "job-cron", &controller), gpuRequests("1")),
	}
	reader := readerFixture(map[string][]map[string]any{
		"jobs":        {submitterJob, cronChild},
		"rayJobs":     {rayJob},
		"rayClusters": {rayCluster},
	}, pods)
	snapshot, err := Inventory(context.Background(), reader, Options{})
	if err != nil {
		t.Fatal(err)
	}
	workloads := workloadsByOwner(snapshot.Workloads)
	rayJobWorkload, ok := workloads["training"]
	if !ok || rayJobWorkload.Owner.Kind != "RayJob" || rayJobWorkload.PodCount != 2 ||
		rayJobWorkload.RequestedFullGPUs != 3 || len(rayJobWorkload.Nodes) != 2 {
		t.Fatalf("RayJob workload = %+v", rayJobWorkload)
	}
	cronWorkload, ok := workloads["scheduled-child"]
	if !ok || cronWorkload.Owner.Kind != "Job" || cronWorkload.Owner.Resolution != "resolved" ||
		cronWorkload.PodCount != 1 {
		t.Fatalf("CronJob child workload = %+v", cronWorkload)
	}
	if snapshot.Summary.Pods != 3 || snapshot.Summary.RequestedFullGPUs != 4 {
		t.Fatalf("summary = %+v", snapshot.Summary)
	}
}

func TestInventoryRetainsPartialOwnerResolutionStates(t *testing.T) {
	controller := true
	rayClusters := []map[string]any{
		ownerFixture("RayCluster", "cycle-a", "cycle-a", "team-a", []map[string]any{
			ownerRef("RayCluster", "cycle-b", "cycle-b", &controller),
		}, nil),
		ownerFixture("RayCluster", "cycle-b", "cycle-b", "team-a", []map[string]any{
			ownerRef("RayCluster", "cycle-a", "cycle-a", &controller),
		}, nil),
	}
	for index := 0; index < maxOwnerDepth+2; index++ {
		var refs []map[string]any
		if index < maxOwnerDepth+1 {
			refs = []map[string]any{
				ownerRef("RayCluster", "depth-"+string(rune('a'+index+1)), "depth-"+string(rune('a'+index+1)), &controller),
			}
		}
		rayClusters = append(rayClusters, ownerFixture(
			"RayCluster",
			"depth-"+string(rune('a'+index)),
			"depth-"+string(rune('a'+index)),
			"team-a",
			refs,
			nil,
		))
	}
	pods := []map[string]any{
		podFixture("forbidden-pod", "pod-forbidden", "team-a", "Running", "node-1", ownerRef("RayJob", "hidden-rayjob", "hidden-rayjob", &controller), gpuRequests("1")),
		podFixture("cycle-pod", "pod-cycle", "team-a", "Running", "node-1", ownerRef("RayCluster", "cycle-a", "cycle-a", &controller), gpuRequests("1")),
		podFixture("depth-pod", "pod-depth", "team-a", "Running", "node-1", ownerRef("RayCluster", "depth-a", "depth-a", &controller), gpuRequests("1")),
	}
	workload := ownerFixture("Workload", "hidden-workload", "hidden-workload", "team-a", []map[string]any{
		ownerRef("RayJob", "hidden-rayjob", "hidden-rayjob", &controller),
	}, tauLabels("queue-a"))
	workload["spec"] = map[string]any{"queueName": "queue-a"}
	reader := readerFixture(map[string][]map[string]any{
		"rayClusters": rayClusters,
		"workloads":   {workload},
	}, pods)
	reader.errs["rayJobs"] = apierrors.NewForbidden(
		schema.GroupResource{Group: "ray.io", Resource: "rayjobs"},
		"",
		errors.New("token-secret"),
	)
	reader.errs["rayServices"] = errors.New("generic-source-failure")
	snapshot, err := Inventory(context.Background(), reader, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Partial {
		t.Fatal("partial = false, want true")
	}
	sourceStates := map[string]string{}
	for _, source := range snapshot.Sources {
		sourceStates[source.Source] = source.State
	}
	if sourceStates["rayJobs"] != "forbidden" || sourceStates["rayServices"] != "unavailable" {
		t.Fatalf("source states = %#v", sourceStates)
	}
	workloads := workloadsByOwner(snapshot.Workloads)
	if workloads["hidden-rayjob"].Owner.Resolution != "source_unavailable" {
		t.Fatalf("forbidden owner = %+v", workloads["hidden-rayjob"].Owner)
	}
	if !workloads["hidden-rayjob"].Managed || workloads["hidden-rayjob"].Queue != "queue-a" {
		t.Fatalf("forbidden owner attribution = %+v", workloads["hidden-rayjob"])
	}
	if workloads["cycle-b"].Owner.Resolution != "cycle" || workloads["cycle-b"].Owner.RawOwnerName != "cycle-a" {
		t.Fatalf("cycle owner = %+v", workloads["cycle-b"].Owner)
	}
	depthFound := false
	for _, workload := range snapshot.Workloads {
		if workload.Owner.Resolution == "depth_limited" {
			depthFound = true
			if workload.Owner.RawOwnerName == "" {
				t.Fatalf("depth owner lost raw owner: %+v", workload.Owner)
			}
		}
	}
	if !depthFound {
		t.Fatalf("workloads = %+v, want depth-limited owner", snapshot.Workloads)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "token-secret") || strings.Contains(string(data), "forbidden:") {
		t.Fatalf("response leaked raw error: %s", data)
	}
}

func TestCalculatePodResourcesUsesSchedulingSemantics(t *testing.T) {
	pod := podObject{}
	pod.Spec.Containers = []podContainer{
		containerFixture(map[string]string{
			fullGPUResource:         "2",
			"nvidia.com/mig-1g.5gb": "1",
		}, map[string]string{
			fullGPUResource: "9",
		}, ""),
		containerFixture(nil, map[string]string{
			fullGPUResource: "1",
		}, ""),
		containerFixture(map[string]string{
			fullGPUResource: "0",
		}, map[string]string{
			fullGPUResource: "8",
		}, ""),
	}
	pod.Spec.InitContainers = []podContainer{
		containerFixture(map[string]string{
			fullGPUResource:         "1",
			"nvidia.com/mig-1g.5gb": "2",
		}, nil, "Always"),
		containerFixture(map[string]string{
			fullGPUResource: "5",
		}, nil, ""),
	}
	pod.Spec.Overhead = map[string]string{
		fullGPUResource:         "1",
		"nvidia.com/mig-1g.5gb": "1",
	}
	got := calculatePodResources(pod)
	if quantityValue(got.total, fullGPUResource) != 7 {
		t.Fatalf("full GPU request = %d, want 7", quantityValue(got.total, fullGPUResource))
	}
	if quantityValue(got.total, "nvidia.com/mig-1g.5gb") != 4 {
		t.Fatalf("MIG request = %d, want 4", quantityValue(got.total, "nvidia.com/mig-1g.5gb"))
	}
	if quantityValue(got.total, fullGPUResource) == quantityValue(got.total, "nvidia.com/mig-1g.5gb") {
		t.Fatal("MIG slices were equated with full GPUs")
	}
}

func TestInventoryAggregatesPendingBoundDRAAndInvalidQuantities(t *testing.T) {
	controller := true
	job := ownerFixture("Job", "train", "job-1", "team-a", nil, tauLabels("queue-a"))
	pending := podFixture("train-pending", "pod-pending", "team-a", "Pending", "", ownerRef("Job", "train", "job-1", &controller), gpuRequests("2"))
	running := podFixture("train-running", "pod-running", "team-a", "Running", "node-1", ownerRef("Job", "train", "job-1", &controller), map[string]any{
		"requests": map[string]string{
			fullGPUResource:            "1",
			"amd.com/gpu":              "2",
			"nvidia.com/mig-2g.10gb":   "3",
			"example.com/gpu-slice":    "4",
			"example.com/gpu-negative": "-2",
			"vendor.com/gpu-broken":    "not-a-quantity",
		},
	})
	dra := podFixture("train-dra", "pod-dra", "team-a", "Pending", "", ownerRef("Job", "train", "job-1", &controller), map[string]any{})
	dra["spec"].(map[string]any)["resourceClaims"] = []map[string]any{{"name": "gpu-claim"}}
	reader := readerFixture(map[string][]map[string]any{"jobs": {job}}, []map[string]any{pending, running, dra})
	snapshot, err := Inventory(context.Background(), reader, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workloads) != 1 {
		t.Fatalf("workloads = %d, want 1", len(snapshot.Workloads))
	}
	workload := snapshot.Workloads[0]
	if workload.PodCount != 3 || workload.PendingPods != 2 || workload.BoundPods != 1 || workload.RequestedFullGPUs != 5 ||
		workload.PendingFullGPUs != 2 || workload.BoundFullGPUs != 3 {
		t.Fatalf("workload = %+v", workload)
	}
	if !workload.CountUnknown || !hasDiagnostic(workload.Diagnostics, "dra_count_unknown") ||
		!hasDiagnostic(workload.Diagnostics, "invalid_gpu_quantity") {
		t.Fatalf("diagnostics = %+v", workload.Diagnostics)
	}
	resources := resourceMap(workload.Resources)
	if resources[fullGPUResource] != 3 || resources["amd.com/gpu"] != 2 ||
		resources["nvidia.com/mig-2g.10gb"] != 3 || resources["example.com/gpu-slice"] != 4 {
		t.Fatalf("resources = %#v", resources)
	}
	if snapshot.Summary.Pods != 3 || snapshot.Summary.CountUnknownPods != 1 ||
		snapshot.Summary.RequestedFullGPUs != 5 || snapshot.Summary.PendingFullGPUs != 2 ||
		snapshot.Summary.BoundFullGPUs != 3 {
		t.Fatalf("summary = %+v", snapshot.Summary)
	}
}

func TestInventoryWorkspaceFilterFailsClosed(t *testing.T) {
	controller := true
	objects := map[string][]map[string]any{
		"jobs": {
			ownerFixture("Job", "alpha", "alpha", "shared", nil, tauLabels("alpha-queue")),
			ownerFixture("Job", "beta", "beta", "shared", nil, tauLabels("beta-queue")),
			ownerFixture("Job", "external", "external", "shared", nil, map[string]string{queueLabel: "alpha-queue"}),
			ownerFixture("Job", "other-ns", "other-ns", "other", nil, tauLabels("alpha-queue")),
		},
	}
	pods := []map[string]any{
		podFixture("alpha-pod", "pod-alpha", "shared", "Running", "node-1", ownerRef("Job", "alpha", "alpha", &controller), gpuRequests("1")),
		podFixture("beta-pod", "pod-beta", "shared", "Running", "node-1", ownerRef("Job", "beta", "beta", &controller), gpuRequests("1")),
		podFixture("external-pod", "pod-external", "shared", "Running", "node-1", ownerRef("Job", "external", "external", &controller), gpuRequests("1")),
		podFixture("other-pod", "pod-other", "other", "Running", "node-1", ownerRef("Job", "other-ns", "other-ns", &controller), gpuRequests("1")),
	}
	reader := readerFixture(objects, pods)
	snapshot, err := Inventory(context.Background(), reader, Options{
		Namespace: "shared", LocalQueue: "alpha-queue", ManagedOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workloads) != 1 || snapshot.Workloads[0].Owner.Name != "alpha" {
		t.Fatalf("workloads = %+v", snapshot.Workloads)
	}
	for _, opts := range []Options{
		{Namespace: "shared", ManagedOnly: true},
		{LocalQueue: "alpha-queue", ManagedOnly: true},
	} {
		if _, err := Inventory(context.Background(), reader, opts); err == nil {
			t.Fatalf("accepted missing workspace scope: %+v", opts)
		}
	}
}

func TestInventoryUsesKueueOwnerMetadataAndFiltersTerminalPods(t *testing.T) {
	controller := true
	job := ownerFixture("Job", "train", "job-1", "team-a", nil, nil)
	workload := ownerFixture("Workload", "train-workload", "workload-1", "team-a", []map[string]any{
		ownerRef("Job", "train", "job-1", &controller),
	}, tauLabels("queue-a"))
	workload["spec"] = map[string]any{"queueName": "queue-a"}
	pods := []map[string]any{
		podFixture("active", "pod-active", "team-a", "Running", "node-1", ownerRef("Job", "train", "job-1", &controller), gpuRequests("1")),
		podFixture("done", "pod-done", "team-a", "Succeeded", "node-1", ownerRef("Job", "train", "job-1", &controller), gpuRequests("9")),
		podFixture("failed", "pod-failed", "team-a", "Failed", "node-1", ownerRef("Job", "train", "job-1", &controller), gpuRequests("9")),
	}
	reader := readerFixture(map[string][]map[string]any{
		"jobs":      {job},
		"workloads": {workload},
	}, pods)
	snapshot, err := Inventory(context.Background(), reader, Options{
		Namespace: "team-a", LocalQueue: "queue-a", ManagedOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Workloads) != 1 || !snapshot.Workloads[0].Managed ||
		snapshot.Workloads[0].Queue != "queue-a" || snapshot.Summary.Pods != 1 ||
		snapshot.Summary.RequestedFullGPUs != 1 {
		t.Fatalf("snapshot = %+v", snapshot)
	}
}

func TestInventoryPodReaderFailureIsFatal(t *testing.T) {
	reader := readerFixture(nil, nil)
	cause := errors.New("generic backend detail")
	reader.errs["pods"] = cause
	if _, err := Inventory(context.Background(), reader, Options{}); err == nil || !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
}

func TestInventoryPreservesForbiddenPodReaderCause(t *testing.T) {
	reader := readerFixture(nil, nil)
	cause := apierrors.NewForbidden(
		schema.GroupResource{Resource: "pods"},
		"",
		errors.New("sensitive backend detail"),
	)
	reader.errs["pods"] = cause
	_, err := Inventory(context.Background(), reader, Options{})
	if err == nil || !apierrors.IsForbidden(err) || !errors.Is(err, cause) {
		t.Fatalf("error = %v", err)
	}
}

func TestInventoryJSONExcludesSensitivePodAndMetadataFields(t *testing.T) {
	controller := true
	job := ownerFixture("Job", "train", "job-1", "team-a", nil, map[string]string{
		workloadmeta.LabelManagedBy: workloadmeta.ManagedByValue,
		queueLabel:                  "queue-a",
		"secret-label":              "label-secret-sentinel",
	})
	job["metadata"].(map[string]any)["annotations"] = map[string]string{
		"secret-annotation": "annotation-secret-sentinel",
	}
	pod := podFixture("train-pod", "pod-1", "team-a", "Running", "node-1", ownerRef("Job", "train", "job-1", &controller), gpuRequests("1"))
	spec := pod["spec"].(map[string]any)
	spec["serviceAccountName"] = "service-account-secret-sentinel"
	spec["volumes"] = []map[string]any{{"secret": map[string]any{"secretName": "volume-secret-sentinel"}}}
	container := spec["containers"].([]map[string]any)[0]
	container["image"] = "image-secret-sentinel"
	container["command"] = []string{"command-secret-sentinel"}
	container["args"] = []string{"args-secret-sentinel"}
	container["env"] = []map[string]any{{"name": "TOKEN", "value": "env-secret-sentinel"}}
	pod["metadata"].(map[string]any)["labels"] = map[string]string{"pod-label": "pod-label-secret-sentinel"}
	pod["metadata"].(map[string]any)["annotations"] = map[string]string{"pod-annotation": "pod-annotation-secret-sentinel"}

	reader := readerFixture(map[string][]map[string]any{"jobs": {job}}, []map[string]any{pod})
	snapshot, err := Inventory(context.Background(), reader, Options{})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for _, sentinel := range []string{
		"label-secret-sentinel",
		"annotation-secret-sentinel",
		"service-account-secret-sentinel",
		"volume-secret-sentinel",
		"image-secret-sentinel",
		"command-secret-sentinel",
		"args-secret-sentinel",
		"env-secret-sentinel",
		"pod-label-secret-sentinel",
		"pod-annotation-secret-sentinel",
	} {
		if strings.Contains(string(data), sentinel) {
			t.Fatalf("JSON leaked %q: %s", sentinel, data)
		}
	}
}

func readerFixture(objects map[string][]map[string]any, pods []map[string]any) *fakeReader {
	reader := &fakeReader{raw: map[string][]byte{}, errs: map[string]error{}}
	reader.raw["pods"] = mustJSON(map[string]any{"items": pods})
	for key, values := range objects {
		reader.raw[key] = mustJSON(map[string]any{"items": values})
	}
	return reader
}

func ownerFixture(kind, name, uid, namespace string, refs []map[string]any, labels map[string]string) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       kind,
		"metadata": map[string]any{
			"name":            name,
			"namespace":       namespace,
			"uid":             uid,
			"labels":          labels,
			"ownerReferences": refs,
		},
	}
}

func ownerRef(kind, name, uid string, controller *bool) map[string]any {
	return map[string]any{
		"apiVersion": "v1",
		"kind":       kind,
		"name":       name,
		"uid":        uid,
		"controller": controller,
	}
}

func podFixture(name, uid, namespace, phase, node string, ref map[string]any, resources map[string]any) map[string]any {
	refs := []map[string]any{}
	if ref != nil {
		refs = append(refs, ref)
	}
	return map[string]any{
		"metadata": map[string]any{
			"name":            name,
			"namespace":       namespace,
			"uid":             uid,
			"ownerReferences": refs,
		},
		"spec": map[string]any{
			"nodeName": node,
			"containers": []map[string]any{{
				"name":      "main",
				"resources": resources,
			}},
		},
		"status": map[string]any{
			"phase": phase,
			"conditions": []map[string]any{{
				"type":   "Ready",
				"status": map[bool]string{true: "True", false: "False"}[phase == "Running"],
			}},
		},
	}
}

func gpuRequests(count string) map[string]any {
	return map[string]any{"requests": map[string]string{fullGPUResource: count}}
}

func tauLabels(queue string) map[string]string {
	return map[string]string{
		workloadmeta.LabelManagedBy: workloadmeta.ManagedByValue,
		queueLabel:                  queue,
	}
}

func containerFixture(requests, limits map[string]string, restartPolicy string) podContainer {
	container := podContainer{RestartPolicy: restartPolicy}
	container.Resources.Requests = requests
	container.Resources.Limits = limits
	return container
}

func workloadsByOwner(workloads []Workload) map[string]Workload {
	out := make(map[string]Workload, len(workloads))
	for _, workload := range workloads {
		out[workload.Owner.Name] = workload
	}
	return out
}

func resourceMap(values []ResourceQuantity) map[string]int64 {
	out := make(map[string]int64, len(values))
	for _, value := range values {
		out[value.Name] = value.Count
	}
	return out
}

func hasDiagnostic(values []Diagnostic, code string) bool {
	for _, value := range values {
		if value.Code == code {
			return true
		}
	}
	return false
}

func mustJSON(value any) []byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}
