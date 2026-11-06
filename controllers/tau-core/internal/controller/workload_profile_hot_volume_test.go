// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

const testHotTopologyKey = "topology.csi.example.com/node"

func TestWorkloadProfileHotVolumeReadiness(t *testing.T) {
	waitForFirstConsumer := storagev1.VolumeBindingWaitForFirstConsumer
	immediate := storagev1.VolumeBindingImmediate
	tests := []struct {
		name    string
		mutate  func([]client.Object) []client.Object
		status  metav1.ConditionStatus
		message string
	}{
		{
			name:    "covered pool node",
			status:  metav1.ConditionTrue,
			message: "StorageClass tau-hot reports free capacity on 1 of 2 profile nodes",
		},
		{
			name: "missing StorageClass",
			mutate: func(objects []client.Object) []client.Object {
				return omitHotVolumeObject(objects, "storageclass/tau-hot")
			},
			status:  metav1.ConditionFalse,
			message: "StorageClass tau-hot does not exist",
		},
		{
			name: "immediate binding",
			mutate: func(objects []client.Object) []client.Object {
				return append(omitHotVolumeObject(objects, "storageclass/tau-hot"), testHotStorageClass(&immediate))
			},
			status:  metav1.ConditionFalse,
			message: "must use volumeBindingMode WaitForFirstConsumer",
		},
		{
			name: "default binding",
			mutate: func(objects []client.Object) []client.Object {
				return append(omitHotVolumeObject(objects, "storageclass/tau-hot"), testHotStorageClass(nil))
			},
			status:  metav1.ConditionFalse,
			message: "must use volumeBindingMode WaitForFirstConsumer",
		},
		{
			name: "capacity for another StorageClass",
			mutate: func(objects []client.Object) []client.Object {
				capacity := testHotStorageCapacity("other", "gpu-1")
				capacity.StorageClassName = "local"
				return append(omitHotVolumeObject(objects, "csistoragecapacity/hot-gpu-1"), capacity)
			},
			status:  metav1.ConditionFalse,
			message: "no CSIStorageCapacity for StorageClass tau-hot reports free capacity on any of 2 profile nodes",
		},
		{
			name: "capacity without node topology",
			mutate: func(objects []client.Object) []client.Object {
				capacity := testHotStorageCapacity("gpu-1", "gpu-1")
				capacity.NodeTopology = nil
				return append(omitHotVolumeObject(objects, "csistoragecapacity/hot-gpu-1"), capacity)
			},
			status:  metav1.ConditionFalse,
			message: "reports free capacity on any of 2 profile nodes",
		},
		{
			name: "zero capacity on a pool node",
			mutate: func(objects []client.Object) []client.Object {
				capacity := testHotStorageCapacity("gpu-2", "gpu-2")
				capacity.Capacity = resource.NewQuantity(0, resource.BinarySI)
				return append(objects, capacity)
			},
			status:  metav1.ConditionTrue,
			message: "reports free capacity on 1 of 2 profile nodes",
		},
		{
			name: "only zero capacity",
			mutate: func(objects []client.Object) []client.Object {
				capacity := testHotStorageCapacity("gpu-1", "gpu-1")
				capacity.Capacity = resource.NewQuantity(0, resource.BinarySI)
				return append(omitHotVolumeObject(objects, "csistoragecapacity/hot-gpu-1"), capacity)
			},
			status:  metav1.ConditionFalse,
			message: "reports free capacity on any of 2 profile nodes",
		},
		{
			name: "capacity unset",
			mutate: func(objects []client.Object) []client.Object {
				capacity := testHotStorageCapacity("gpu-1", "gpu-1")
				capacity.Capacity = nil
				return append(omitHotVolumeObject(objects, "csistoragecapacity/hot-gpu-1"), capacity)
			},
			status:  metav1.ConditionFalse,
			message: "reports free capacity on any of 2 profile nodes",
		},
		{
			name: "capacity only outside the pool",
			mutate: func(objects []client.Object) []client.Object {
				return append(omitHotVolumeObject(objects, "csistoragecapacity/hot-gpu-1"), testHotStorageCapacity("cpu-1", "cpu-1"))
			},
			status:  metav1.ConditionFalse,
			message: "reports free capacity on any of 2 profile nodes",
		},
		{
			name: "no pool nodes",
			mutate: func(objects []client.Object) []client.Object {
				return omitHotVolumeObject(omitHotVolumeObject(objects, "node/gpu-1"), "node/gpu-2")
			},
			status:  metav1.ConditionFalse,
			message: "no nodes match the nodeLabels of the profile's ResourceFlavors",
		},
		{
			name: "missing ResourceFlavor",
			mutate: func(objects []client.Object) []client.Object {
				return omitHotVolumeObject(objects, "resourceflavor/h200")
			},
			status:  metav1.ConditionFalse,
			message: "hot volume nodes cannot be resolved until ResourceFlavors are ready",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			objects := hotVolumeProfileDependencies(&waitForFirstConsumer)
			if test.mutate != nil {
				objects = test.mutate(objects)
			}
			cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{testHotVolumeProfile()})
			reconciler := &TauClusterReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()}

			state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
			if err != nil {
				t.Fatalf("observeWorkloadProfiles() error = %v", err)
			}
			conditions := state.status.Profiles[0].Conditions
			hot := findCondition(conditions, profile.ConditionHotVolumeReady)
			if hot == nil || hot.Status != test.status || !strings.Contains(hot.Message, test.message) {
				t.Fatalf("HotVolumeReady = %#v, want %s containing %q", hot, test.status, test.message)
			}
			assertCondition(t, conditions, profile.ConditionReady, test.status)
		})
	}
}

func TestWorkloadProfileWithoutHotVolumeOmitsCondition(t *testing.T) {
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{testWorkloadProfile("research", []string{"team-a"})})
	reconciler := &TauClusterReconciler{Client: fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(validProfileDependencies("research", "team-a")...).
		Build()}

	cluster.Status.WorkloadProfiles.Profiles = []profile.ResolvedWorkloadProfile{{
		WorkloadProfile: profile.WorkloadProfile{Name: "research"},
		Conditions: []metav1.Condition{{
			Type: profile.ConditionHotVolumeReady, Status: metav1.ConditionFalse, Reason: "HotVolumeNotReady",
		}},
	}}
	state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
	if err != nil {
		t.Fatalf("observeWorkloadProfiles() error = %v", err)
	}
	conditions := state.status.Profiles[0].Conditions
	assertConditionAbsent(t, conditions, profile.ConditionHotVolumeReady)
	assertCondition(t, conditions, profile.ConditionReady, metav1.ConditionTrue)
}

func TestInvalidHotVolumeProfileSkipsHotVolumeObservation(t *testing.T) {
	workloadProfile := testHotVolumeProfile()
	workloadProfile.HotVolume.DefaultSize = "4Ti"
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{workloadProfile})
	reconciler := &TauClusterReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}

	state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
	if err != nil {
		t.Fatalf("observeWorkloadProfiles() error = %v", err)
	}
	conditions := state.status.Profiles[0].Conditions
	assertCondition(t, conditions, profile.ConditionHotVolumeReady, metav1.ConditionUnknown)
	assertCondition(t, conditions, profile.ConditionReady, metav1.ConditionFalse)
}

func TestStorageCapacityCoveragePredicateIgnoresCapacityOnlyUpdates(t *testing.T) {
	predicate := storageCapacityCoveragePredicate()
	old := testHotStorageCapacity("gpu-1", "gpu-1")
	if !predicate.Create(event.CreateEvent{Object: old}) || !predicate.Delete(event.DeleteEvent{Object: old}) {
		t.Fatal("create and delete events must enqueue")
	}
	capacityOnly := old.DeepCopy()
	capacityOnly.Capacity = resource.NewQuantity(1, resource.BinarySI)
	if predicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: capacityOnly}) {
		t.Fatal("capacity-only update enqueued")
	}
	exhausted := old.DeepCopy()
	exhausted.Capacity = resource.NewQuantity(0, resource.BinarySI)
	if !predicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: exhausted}) {
		t.Fatal("capacity exhaustion did not enqueue")
	}
	if !predicate.Update(event.UpdateEvent{ObjectOld: exhausted, ObjectNew: old}) {
		t.Fatal("capacity recovery did not enqueue")
	}
	unset := old.DeepCopy()
	unset.Capacity = nil
	if predicate.Update(event.UpdateEvent{ObjectOld: exhausted, ObjectNew: unset}) {
		t.Fatal("zero to unset capacity enqueued")
	}
	moved := testHotStorageCapacity("gpu-1", "gpu-2")
	if !predicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: moved}) {
		t.Fatal("node topology update did not enqueue")
	}
	reclassed := old.DeepCopy()
	reclassed.StorageClassName = "local"
	if !predicate.Update(event.UpdateEvent{ObjectOld: old, ObjectNew: reclassed}) {
		t.Fatal("storage class update did not enqueue")
	}
}

func testHotVolumeProfile() profile.WorkloadProfile {
	workloadProfile := testWorkloadProfile("research", []string{"team-a"})
	workloadProfile.HotVolume = &profile.HotVolume{StorageClassName: "tau-hot", DefaultSize: "1Ti", MaxSize: "2Ti"}
	return workloadProfile
}

func hotVolumeProfileDependencies(bindingMode *storagev1.VolumeBindingMode) []client.Object {
	objects := validProfileDependencies("research", "team-a")
	objects = omitProfileDependency(objects, "resourceflavor/h200")
	objects = omitProfileDependency(objects, "resourceflavor/a100")
	h200 := testResourceFlavor("h200", "gpu-topology")
	h200.Object["spec"].(map[string]any)["nodeLabels"] = map[string]any{"gpu": "h200"}
	a100 := testResourceFlavor("a100", "")
	a100.Object["spec"] = map[string]any{"nodeLabels": map[string]any{"gpu": "a100"}}
	return append(objects, h200, a100,
		testHotNode("gpu-1", map[string]string{"gpu": "h200"}),
		testHotNode("gpu-2", map[string]string{"gpu": "a100"}),
		testHotNode("cpu-1", nil),
		testHotStorageClass(bindingMode),
		testHotStorageCapacity("gpu-1", "gpu-1"),
	)
}

func testHotNode(name string, extra map[string]string) *corev1.Node {
	nodeLabels := map[string]string{testHotTopologyKey: name}
	for key, value := range extra {
		nodeLabels[key] = value
	}
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: nodeLabels}}
}

func testHotStorageClass(bindingMode *storagev1.VolumeBindingMode) *storagev1.StorageClass {
	return &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "tau-hot"},
		Provisioner:       "hot.csi.example.com",
		VolumeBindingMode: bindingMode,
	}
}

func testHotStorageCapacity(name, node string) *storagev1.CSIStorageCapacity {
	return &storagev1.CSIStorageCapacity{
		ObjectMeta:       metav1.ObjectMeta{Name: "hot-" + name, Namespace: "tau-system"},
		StorageClassName: "tau-hot",
		NodeTopology:     &metav1.LabelSelector{MatchLabels: map[string]string{testHotTopologyKey: node}},
		Capacity:         resource.NewQuantity(3<<40, resource.BinarySI),
	}
}

func omitHotVolumeObject(objects []client.Object, omit string) []client.Object {
	out := make([]client.Object, 0, len(objects))
	for _, object := range objects {
		key := objectKey(object)
		switch typed := object.(type) {
		case *corev1.Node:
			key = "node/" + typed.Name
		case *storagev1.StorageClass:
			key = "storageclass/" + typed.Name
		case *storagev1.CSIStorageCapacity:
			key = "csistoragecapacity/" + typed.Name
		}
		if key != omit {
			out = append(out, object)
		}
	}
	return out
}
