// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"strings"
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestTauClusterReconcileMultiKueueReadiness(t *testing.T) {
	tests := []struct {
		name         string
		prerequisite MultiKueuePrerequisiteStatus
		wantReason   string
		wantStatus   metav1.ConditionStatus
	}{
		{
			name: "prerequisites not ready",
			prerequisite: MultiKueuePrerequisiteStatus{
				Message: "no active worker clusters",
			},
			wantReason: "PrerequisitesNotReady",
			wantStatus: metav1.ConditionFalse,
		},
		{
			name: "prerequisites ready",
			prerequisite: MultiKueuePrerequisiteStatus{
				Ready:   true,
				Message: "manager prerequisites are ready",
			},
			wantReason: "Ready",
			wantStatus: metav1.ConditionTrue,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			cluster := &tauv1alpha1.TauCluster{
				ObjectMeta: metav1.ObjectMeta{
					Name:       tauv1alpha1.TauClusterSingletonName,
					Generation: 5,
				},
				Spec: tauv1alpha1.TauClusterSpec{
					ManagementMode: tauv1alpha1.ClusterManagementModeObserve,
				},
			}
			baseClient := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(cluster).
				WithStatusSubresource(&tauv1alpha1.TauCluster{}).
				Build()
			reader := &countingMultiKueuePrerequisiteReader{status: test.prerequisite}
			reconciler := &TauClusterReconciler{
				Client:                  baseClient,
				MultiKueuePrerequisites: reader,
			}
			if _, err := reconciler.Reconcile(ctx, ctrl.Request{
				NamespacedName: types.NamespacedName{Name: cluster.Name},
			}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if reader.calls != 1 {
				t.Fatalf("prerequisite calls = %d, want 1", reader.calls)
			}

			var got tauv1alpha1.TauCluster
			if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, &got); err != nil {
				t.Fatalf("Get TauCluster: %v", err)
			}
			condition := findCondition(got.Status.Conditions, tauv1alpha1.ConditionMultiKueueReady)
			if condition == nil ||
				condition.Status != test.wantStatus ||
				condition.Reason != test.wantReason ||
				condition.ObservedGeneration != cluster.Generation {
				t.Fatalf("MultiKueueReady = %#v", condition)
			}
		})
	}
}

func TestMultiKueueReadinessFailsClosedWithoutReader(t *testing.T) {
	got, err := (&TauClusterReconciler{}).multiKueueReadinessCondition(context.Background(), 9)
	if err != nil {
		t.Fatalf("multiKueueReadinessCondition() error = %v", err)
	}
	if got.Status != metav1.ConditionFalse ||
		got.Reason != "PrerequisitesNotReady" ||
		got.ObservedGeneration != 9 ||
		!strings.Contains(got.Message, "reader is not configured") {
		t.Fatalf("MultiKueueReady = %#v", got)
	}
}

func TestKubernetesMultiKueuePrerequisites(t *testing.T) {
	tests := []struct {
		name        string
		objects     []client.Object
		wantReady   bool
		wantMessage string
	}{
		{
			name:        "no controller admission check",
			objects:     []client.Object{testManagerAdmissionCheck("ordinary", "example.com/other", true, "config-a")},
			wantMessage: "no AdmissionCheck uses controller",
		},
		{
			name: "controller identity ignores admission check name",
			objects: []client.Object{
				testManagerAdmissionCheck("dispatch-beta", multiKueueAdmissionCheckController, true, "config-a"),
				testMultiKueueConfig("config-a", "worker-a"),
				testMultiKueueCluster("worker-a", true),
			},
			wantReady:   true,
			wantMessage: `AdmissionCheck "dispatch-beta"`,
		},
		{
			name: "inactive worker fails closed",
			objects: []client.Object{
				testManagerAdmissionCheck("dispatch-beta", multiKueueAdmissionCheckController, true, "config-a"),
				testMultiKueueConfig("config-a", "worker-a"),
				testMultiKueueCluster("worker-a", false),
			},
			wantMessage: "no Active worker clusters",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(test.objects...).
				Build()
			got, err := (&KubernetesMultiKueuePrerequisites{Reader: reader}).Check(context.Background())
			if err != nil {
				t.Fatalf("Check() error = %v", err)
			}
			if got.Ready != test.wantReady || !strings.Contains(got.Message, test.wantMessage) {
				t.Fatalf("Check() = %#v, want ready=%t message containing %q", got, test.wantReady, test.wantMessage)
			}
		})
	}
}

type countingMultiKueuePrerequisiteReader struct {
	calls  int
	status MultiKueuePrerequisiteStatus
}

func (r *countingMultiKueuePrerequisiteReader) Check(context.Context) (MultiKueuePrerequisiteStatus, error) {
	r.calls++
	return r.status, nil
}

func testManagerAdmissionCheck(name, controllerName string, active bool, configName string) *unstructured.Unstructured {
	object := testKueueObject(admissionCheckGVK, name)
	object.Object["spec"] = map[string]any{
		"controllerName": controllerName,
		"parameters": map[string]any{
			"apiGroup": "kueue.x-k8s.io",
			"kind":     "MultiKueueConfig",
			"name":     configName,
		},
	}
	object.Object["status"] = map[string]any{"conditions": testActiveConditions(active)}
	return object
}

func testMultiKueueConfig(name string, clusters ...string) *unstructured.Unstructured {
	object := testKueueObject(multiKueueConfigGVK, name)
	values := make([]any, len(clusters))
	for i, cluster := range clusters {
		values[i] = cluster
	}
	object.Object["spec"] = map[string]any{"clusters": values}
	return object
}

func testMultiKueueCluster(name string, active bool) *unstructured.Unstructured {
	object := testKueueObject(multiKueueClusterGVK, name)
	object.Object["status"] = map[string]any{"conditions": testActiveConditions(active)}
	return object
}

func testActiveConditions(active bool) []any {
	status := "False"
	if active {
		status = "True"
	}
	return []any{map[string]any{"type": "Active", "status": status}}
}
