// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"reflect"
	"strings"
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	"github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestQueueTopologyOptInPreservesQueueAndOriginalFlavor(t *testing.T) {
	ctx := context.Background()
	queue, source := queueTopologyTestObjects()
	untouched := queue.DeepCopy()
	untouched.SetName("not-opted-in")
	untouched.SetLabels(nil)
	originalQueue := queue.DeepCopy()
	originalSource := source.DeepCopy()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, source, untouched).Build()
	recording := &resourceMutationRecordingClient{Client: c}
	reconciler := &TauClusterReconciler{Client: recording}

	managed, drifted, err := reconciler.reconcileOptedInQueueTopologies(ctx, true)
	if err != nil || drifted || len(managed) != 1 {
		t.Fatalf("reconcile = (%v, %v, %v), want one managed flavor without drift", managed, drifted, err)
	}
	got := newQueueObject(clusterQueueGVK)
	if err := c.Get(ctx, client.ObjectKeyFromObject(queue), got); err != nil {
		t.Fatal(err)
	}
	replacement := managed[0].Name
	groups, _, _ := unstructured.NestedSlice(originalQueue.Object, "spec", "resourceGroups")
	group := groups[0].(map[string]any)
	flavors := group["flavors"].([]any)
	flavors[1].(map[string]any)["name"] = replacement
	if err := unstructured.SetNestedSlice(originalQueue.Object, groups, "spec", "resourceGroups"); err != nil {
		t.Fatal(err)
	}
	if err := replaceTopologyAdmissionCheckFlavors(originalQueue, map[string]string{source.GetName(): replacement}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Object["spec"], originalQueue.Object["spec"]) ||
		!reflect.DeepEqual(got.GetLabels(), originalQueue.GetLabels()) {
		t.Fatalf("queue changed beyond GPU flavor reference: %#v", got.Object)
	}
	copy := newQueueObject(resourceFlavorGVK)
	if err := c.Get(ctx, client.ObjectKey{Name: replacement}, copy); err != nil {
		t.Fatal(err)
	}
	wantSpec, _, _ := unstructured.NestedMap(originalSource.Object, "spec")
	wantSpec["topologyName"] = tauGPUNodeTopologyName
	if !reflect.DeepEqual(copy.Object["spec"], wantSpec) || copy.GetLabels()[labelManagedBy] != labelManagedByValue {
		t.Fatalf("topology copy did not preserve original placement spec: %#v", copy.Object)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(source), source); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(source.Object["spec"], originalSource.Object["spec"]) {
		t.Fatalf("original flavor was modified: %#v", source.Object)
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(untouched), got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Object["spec"], untouched.Object["spec"]) {
		t.Fatal("queue without opt-in was modified")
	}

	recording.mutations = nil
	if _, drifted, err := reconciler.reconcileOptedInQueueTopologies(ctx, true); err != nil || drifted {
		t.Fatalf("second reconcile drifted=%v, err=%v", drifted, err)
	}
	if len(recording.mutations) != 0 {
		t.Fatalf("idempotent reconcile mutated resources: %v", recording.mutations)
	}
}

func TestQueueTopologyMigrationGuards(t *testing.T) {
	tests := []struct {
		name   string
		change func(*unstructured.Unstructured)
		want   string
	}{
		{
			name: "malformed admission checks",
			change: func(q *unstructured.Unstructured) {
				q.Object["spec"].(map[string]any)["admissionChecksStrategy"] = map[string]any{
					"admissionChecks": []any{map[string]any{"name": "check", "onFlavors": "invalid"}},
				}
			},
			want: "onFlavors",
		},
		{
			name: "stale pause acknowledgment",
			change: func(q *unstructured.Unstructured) {
				q.SetGeneration(2)
				q.Object["status"] = map[string]any{"conditions": []any{
					map[string]any{"type": "Active", "status": "False", "reason": "Stopped", "observedGeneration": int64(1)},
				}}
			},
			want: "reason Stopped",
		},
		{
			name: "running queue",
			change: func(q *unstructured.Unstructured) {
				unstructured.RemoveNestedField(q.Object, "spec", "stopPolicy")
			},
			want: "spec.stopPolicy=Hold",
		},
		{
			name: "Kueue has not observed pause",
			change: func(q *unstructured.Unstructured) {
				unstructured.RemoveNestedField(q.Object, "status", "conditions")
			},
			want: "reason Stopped",
		},
		{
			name: "inactive for another reason",
			change: func(q *unstructured.Unstructured) {
				q.Object["status"] = map[string]any{"conditions": []any{
					map[string]any{"type": "Active", "status": "False", "reason": "FlavorNotFound"},
				}}
			},
			want: "reason Stopped",
		},
		{
			name: "reserved quota",
			change: func(q *unstructured.Unstructured) {
				q.Object["status"].(map[string]any)["flavorsReservation"] = []any{
					map[string]any{"name": "legacy-gpu", "resources": []any{
						map[string]any{"name": "cpu", "total": "1"},
					}},
				}
			},
			want: "active reservations",
		},
		{
			name: "malformed reservations",
			change: func(q *unstructured.Unstructured) {
				q.Object["status"].(map[string]any)["flavorsReservation"] = "invalid"
			},
			want: "reservations",
		},
		{
			name: "reserving workloads",
			change: func(q *unstructured.Unstructured) {
				q.Object["status"].(map[string]any)["reservingWorkloads"] = int64(1)
			},
			want: "reservingWorkloads",
		},
		{
			name: "admitted workloads",
			change: func(q *unstructured.Unstructured) {
				q.Object["status"].(map[string]any)["admittedWorkloads"] = int64(1)
			},
			want: "admittedWorkloads",
		},
		{
			name: "workspace managed queue",
			change: func(q *unstructured.Unstructured) {
				q.SetLabels(map[string]string{
					labelkeys.LabelUseGPUTopology: "true",
					labelManagedBy:                labelManagedByValue,
					labelWorkspace:                "research",
				})
			},
			want: "managed by TauWorkspace",
		},
		{
			name: "cohort quota authority",
			change: func(q *unstructured.Unstructured) {
				q.Object["spec"].(map[string]any)["cohort"] = "shared"
			},
			want: "cohort quota authority",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			queue, source := queueTopologyTestObjects()
			tt.change(queue)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, source).Build()
			recording := &resourceMutationRecordingClient{Client: c}
			_, drifted, err := (&TauClusterReconciler{Client: recording}).reconcileOptedInQueueTopologies(context.Background(), true)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !drifted {
				t.Fatalf("drifted=%v, err=%v, want %q", drifted, err, tt.want)
			}
			if len(recording.mutations) != 0 {
				t.Fatalf("blocked migration mutated resources: %v", recording.mutations)
			}
		})
	}
}

func TestQueueTopologyObserveOnlyAndDisabledOptIn(t *testing.T) {
	for _, label := range []string{"true", "false", ""} {
		t.Run("label="+label, func(t *testing.T) {
			queue, source := queueTopologyTestObjects()
			queue.SetLabels(map[string]string{labelkeys.LabelUseGPUTopology: label})
			unstructured.RemoveNestedField(queue.Object, "spec", "stopPolicy")
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, source).Build()
			recording := &resourceMutationRecordingClient{Client: c}
			_, drifted, err := (&TauClusterReconciler{Client: recording}).reconcileOptedInQueueTopologies(context.Background(), false)
			if err != nil || drifted != (label == "true") {
				t.Fatalf("drifted=%v, err=%v", drifted, err)
			}
			if len(recording.mutations) != 0 {
				t.Fatalf("observation mutated resources: %v", recording.mutations)
			}
		})
	}
}

func TestQueueTopologyAlreadyConfiguredNeedsNoPause(t *testing.T) {
	queue, source := queueTopologyTestObjects()
	unstructured.RemoveNestedField(queue.Object, "spec", "stopPolicy")
	source.Object["spec"].(map[string]any)["topologyName"] = tauGPUNodeTopologyName
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, source).Build()
	recording := &resourceMutationRecordingClient{Client: c}
	_, drifted, err := (&TauClusterReconciler{Client: recording}).reconcileOptedInQueueTopologies(context.Background(), true)
	if err != nil || drifted || len(recording.mutations) != 0 {
		t.Fatalf("already configured queue drifted=%v, err=%v, mutations=%v", drifted, err, recording.mutations)
	}
}

func TestQueueTopologyCopyConflictsDoNotSwitchQueue(t *testing.T) {
	for _, conflict := range []string{"ownership", "spec"} {
		t.Run(conflict, func(t *testing.T) {
			queue, source := queueTopologyTestObjects()
			copy, err := topologyFlavorCopy(source)
			if err != nil {
				t.Fatal(err)
			}
			if conflict == "ownership" {
				copy.SetLabels(nil)
			} else {
				copy.Object["spec"].(map[string]any)["topologyName"] = "other-topology"
			}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, source, copy).Build()
			recording := &resourceMutationRecordingClient{Client: c}
			_, drifted, err := (&TauClusterReconciler{Client: recording}).reconcileOptedInQueueTopologies(context.Background(), true)
			if err == nil || !drifted || !strings.Contains(err.Error(), "ownership conflict or immutable spec drift") {
				t.Fatalf("drifted=%v, err=%v", drifted, err)
			}
			if len(recording.mutations) != 0 {
				t.Fatalf("conflict mutated resources: %v", recording.mutations)
			}
		})
	}
}

func TestQueueTopologySharedCopiesAndOptOut(t *testing.T) {
	ctx := context.Background()
	queue, source := queueTopologyTestObjects()
	second := queue.DeepCopy()
	second.SetName("second")
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, second, source).Build()
	r := &TauClusterReconciler{Client: c}
	managed, _, err := r.reconcileOptedInQueueTopologies(ctx, true)
	if err != nil || len(managed) != 1 {
		t.Fatalf("shared copy managed=%v, err=%v", managed, err)
	}
	if managed, _, err = r.reconcileOptedInQueueTopologies(ctx, true); err != nil || len(managed) != 1 {
		t.Fatalf("shared copy second reconcile managed=%v, err=%v", managed, err)
	}
	for _, q := range []*unstructured.Unstructured{queue, second} {
		if err := c.Get(ctx, client.ObjectKeyFromObject(q), q); err != nil {
			t.Fatal(err)
		}
		q.SetLabels(nil)
		if err := c.Update(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	recording := &resourceMutationRecordingClient{Client: c}
	if _, _, err := (&TauClusterReconciler{Client: recording}).reconcileOptedInQueueTopologies(ctx, true); err != nil {
		t.Fatal(err)
	}
	if len(recording.mutations) != 0 {
		t.Fatalf("removing opt-in reverted migration: %v", recording.mutations)
	}
}

func TestQueueTopologyAdmissionCheckFlavorReferences(t *testing.T) {
	queue, source := queueTopologyTestObjects()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, source).Build()
	managed, _, err := (&TauClusterReconciler{Client: c}).reconcileOptedInQueueTopologies(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(queue), queue); err != nil {
		t.Fatal(err)
	}
	checks, _, err := unstructured.NestedSlice(queue.Object, "spec", "admissionChecksStrategy", "admissionChecks")
	if err != nil {
		t.Fatal(err)
	}
	want := []any{
		map[string]any{"name": "gpu-policy", "onFlavors": []any{managed[0].Name, "other-gpu"}},
		map[string]any{"name": "global-policy"},
	}
	if !reflect.DeepEqual(checks, want) {
		t.Fatalf("admission checks=%#v, want %#v", checks, want)
	}
}

func TestQueueTopologyConcurrentQueueChangeIsNotOverwritten(t *testing.T) {
	queue, source := queueTopologyTestObjects()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, source).Build()
	racing := &queueTopologyConcurrentUpdateClient{Client: c}
	_, drifted, err := (&TauClusterReconciler{Client: racing}).reconcileOptedInQueueTopologies(context.Background(), true)
	if !apierrors.IsConflict(err) || !drifted {
		t.Fatalf("concurrent update drifted=%v, err=%v", drifted, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(queue), queue); err != nil {
		t.Fatal(err)
	}
	groups, _, _ := unstructured.NestedSlice(queue.Object, "spec", "resourceGroups")
	flavors := groups[0].(map[string]any)["flavors"].([]any)
	if flavors[1].(map[string]any)["name"] != source.GetName() || queue.GetLabels()[labelkeys.LabelUseGPUTopology] != "false" {
		t.Fatalf("concurrent queue change was overwritten: %#v", queue.Object)
	}
}

type queueTopologyConcurrentUpdateClient struct {
	client.Client
}

func (c *queueTopologyConcurrentUpdateClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if obj.GetObjectKind().GroupVersionKind() == clusterQueueGVK {
		current := newQueueObject(clusterQueueGVK)
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(obj), current); err != nil {
			return err
		}
		labels := current.GetLabels()
		labels[labelkeys.LabelUseGPUTopology] = "false"
		current.SetLabels(labels)
		if err := c.Client.Update(ctx, current); err != nil {
			return err
		}
	}
	return c.Client.Update(ctx, obj, opts...)
}

func TestQueueTopologyQuotaEligibility(t *testing.T) {
	for _, tt := range []struct {
		name      string
		nominal   string
		wantCount int
		wantError bool
	}{
		{name: "positive GPU quota", nominal: "16", wantCount: 1},
		{name: "CPU flavor with zero GPU quota", nominal: "0"},
		{name: "invalid nominal", nominal: "invalid", wantError: true},
		{name: "negative nominal", nominal: "-1", wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			queue, source := queueTopologyTestObjects()
			groups := queue.Object["spec"].(map[string]any)["resourceGroups"].([]any)
			flavors := groups[0].(map[string]any)["flavors"].([]any)
			resources := flavors[1].(map[string]any)["resources"].([]any)
			resources[2].(map[string]any)["nominalQuota"] = tt.nominal
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue, source).Build()
			managed, _, err := (&TauClusterReconciler{Client: c}).reconcileOptedInQueueTopologies(context.Background(), true)
			if len(managed) != tt.wantCount || (err != nil) != tt.wantError {
				t.Fatalf("managed=%v, err=%v", managed, err)
			}
		})
	}
}

func TestQueueTopologyTauClusterReportsBlockedMigration(t *testing.T) {
	cluster := topologyTestCluster()
	queue, source := queueTopologyTestObjects()
	unstructured.RemoveNestedField(queue.Object, "spec", "stopPolicy")
	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster, queue, source).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cluster)}
	r := &TauClusterReconciler{Client: c}
	if _, err := r.Reconcile(context.Background(), request); err == nil || !strings.Contains(err.Error(), "spec.stopPolicy=Hold") {
		t.Fatalf("Reconcile error=%v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(cluster), cluster); err != nil {
		t.Fatal(err)
	}
	assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionQueuesReady, metav1.ConditionFalse)
	for _, condition := range cluster.Status.Conditions {
		if condition.Type == tauv1alpha1.ConditionQueuesReady && condition.Reason != "GPUQueueTopologyReconcileFailed" {
			t.Fatalf("unexpected queue condition: %#v", condition)
		}
	}
}

func TestQueueTopologyMissingSourceDoesNotMutate(t *testing.T) {
	queue, _ := queueTopologyTestObjects()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(queue).Build()
	recording := &resourceMutationRecordingClient{Client: c}
	_, _, err := (&TauClusterReconciler{Client: recording}).reconcileOptedInQueueTopologies(context.Background(), true)
	if !apierrors.IsNotFound(err) || len(recording.mutations) != 0 {
		t.Fatalf("err=%v, mutations=%v", err, recording.mutations)
	}
}

func queueTopologyTestObjects() (*unstructured.Unstructured, *unstructured.Unstructured) {
	queue := topologyTestClusterQueue("research")
	queue.SetLabels(map[string]string{labelkeys.LabelUseGPUTopology: "true", "operator": "test"})
	queue.Object["spec"].(map[string]any)["stopPolicy"] = "Hold"
	queue.Object["spec"].(map[string]any)["namespaceSelector"] = map[string]any{"matchLabels": map[string]any{"team": "research"}}
	queue.Object["spec"].(map[string]any)["admissionChecksStrategy"] = map[string]any{
		"admissionChecks": []any{
			map[string]any{"name": "gpu-policy", "onFlavors": []any{"legacy-gpu", "other-gpu"}},
			map[string]any{"name": "global-policy"},
		},
	}
	queue.Object["status"] = map[string]any{
		"conditions": []any{map[string]any{"type": "Active", "status": "False", "reason": "Stopped"}},
	}
	groups := queue.Object["spec"].(map[string]any)["resourceGroups"].([]any)
	group := groups[0].(map[string]any)
	cpu := group["flavors"].([]any)[0].(map[string]any)
	cpu["resources"].([]any)[2].(map[string]any)["borrowingLimit"] = "0"
	group["flavors"] = append(group["flavors"].([]any), map[string]any{
		"name": "legacy-gpu",
		"resources": []any{
			map[string]any{"name": "cpu", "nominalQuota": "100", "borrowingLimit": "20", "lendingLimit": "30"},
			map[string]any{"name": "memory", "nominalQuota": "1Ti"},
			map[string]any{"name": nvidiaGPUResourceName, "nominalQuota": "16", "borrowingLimit": "8", "lendingLimit": "4"},
		},
	})
	source := newQueueObject(resourceFlavorGVK)
	source.SetName("legacy-gpu")
	source.SetLabels(map[string]string{labelManagedBy: "another-operator"})
	source.Object["spec"] = map[string]any{
		"nodeLabels":   map[string]any{"gpu-family": "h200", "kubernetes.io/os": "linux"},
		"nodeTaints":   []any{map[string]any{"key": "sku", "value": "gpu", "effect": "NoSchedule"}},
		"tolerations":  []any{map[string]any{"key": "dedicated", "operator": "Equal", "value": "research", "effect": "NoSchedule"}},
		"topologyName": "hostname-only",
	}
	return queue, source
}
