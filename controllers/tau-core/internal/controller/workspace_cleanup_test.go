// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestWorkspacePreservesRoutingForJobBeforeWorkloadCreation(t *testing.T) {
	for _, lifecycle := range []string{"delete", "retarget"} {
		t.Run(lifecycle, func(t *testing.T) {
			ctx := context.Background()
			workspace := testWorkspace("aurora")
			workspace.Finalizers = []string{workspaceFinalizer}
			workspace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
			now := metav1.Now()
			if lifecycle == "delete" {
				workspace.DeletionTimestamp = &now
			}
			workspace.Spec.Target.CreateNamespace = false
			workspace.Spec.Queue = "jobqueue"
			workspace.Status.Target.ResolvedNamespace = workspace.Name
			namespace := testNamespace(workspace.Name)
			namespace.Labels = workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue)
			account := testServiceAccount(namespace.Name, workspace.Spec.WorkloadIdentity.ServiceAccountName, workspace.Name)
			account.Annotations[annotationAzureWIClientID] = workspace.Spec.WorkloadIdentity.ClientID
			newNamespace := testNamespace("new-target")
			newNamespace.Labels = map[string]string{labelWorkspace: workspace.Name}
			newNamespace.Annotations = map[string]string{annotationOwnerUID: string(workspace.UID)}
			if lifecycle == "retarget" {
				workspace.Spec.Target.Namespace = newNamespace.Name
			}
			job := &batchv1.Job{
				ObjectMeta: metav1.ObjectMeta{
					Name: "training", Namespace: namespace.Name, UID: types.UID("training-job-uid"),
					Labels: map[string]string{"kueue.x-k8s.io/queue-name": "jobqueue"},
				},
				Spec: batchv1.JobSpec{
					Suspend: ptr.To(true),
					Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
						ServiceAccountName: account.Name,
						RestartPolicy:      corev1.RestartPolicyNever,
						Containers:         []corev1.Container{{Name: "trainer", Image: "example.invalid/trainer"}},
					}},
				},
			}
			localQueue := testLocalQueue(namespace.Name, "jobqueue", "jobqueue")
			localQueue.SetAnnotations(nil)
			clusterQueue := clusterQueueWithUID("jobqueue", "jobqueue-uid")
			clusterQueue.Object["spec"] = map[string]any{"namespaceSelector": map[string]any{
				"matchLabels": map[string]any{labelWorkspace: workspace.Name},
			}}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).
				WithObjects(workspace, namespace, newNamespace, account, job, localQueue, clusterQueue).
				WithStatusSubresource(&tauv1alpha1.TauWorkspace{}).Build()
			reconciler := newTestWorkspaceReconciler(c)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(workspace)}
			for range 3 {
				_, err := reconciler.Reconcile(ctx, request)
				if lifecycle == "delete" && (err == nil ||
					!strings.Contains(err.Error(), "before removing namespace routing metadata")) {
					t.Fatalf("Reconcile() = %v, want source Job drain wait", err)
				}
				if lifecycle == "retarget" && err != nil {
					t.Fatal(err)
				}
				var waiting tauv1alpha1.TauWorkspace
				if err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &waiting); err != nil {
					t.Fatal(err)
				}
				if waiting.Status.Target.ResolvedNamespace != namespace.Name {
					t.Fatal("resolved target advanced before source Job drain")
				}
				if err := c.Get(ctx, client.ObjectKeyFromObject(account), &corev1.ServiceAccount{}); err != nil {
					t.Fatalf("execution identity removed before source Job drain: %v", err)
				}
				var retained corev1.Namespace
				if err := c.Get(ctx, client.ObjectKeyFromObject(namespace), &retained); err != nil {
					t.Fatal(err)
				}
				if retained.Labels[labelWorkspace] != workspace.Name {
					t.Fatal("source Job lost namespace routing metadata")
				}
			}
			workload := newQueueObject(workloadGVK)
			workload.SetName("job-training")
			workload.SetNamespace(namespace.Name)
			workload.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID,
			}})
			workload.Object["spec"] = map[string]any{"queueName": localQueue.GetName()}
			if err := c.Create(ctx, workload); err != nil {
				t.Fatal(err)
			}
			if _, err := reconciler.Reconcile(ctx, request); lifecycle == "delete" && err == nil {
				t.Fatal("workspace stopped draining after Kueue created the Workload")
			}
			workload.Object["status"] = map[string]any{"conditions": []any{
				map[string]any{"type": "Finished", "status": "True"},
			}}
			if err := c.Update(ctx, workload); err != nil {
				t.Fatal(err)
			}
			for range 3 {
				if _, err := reconciler.Reconcile(ctx, request); err != nil {
					t.Fatalf("Reconcile() after drain = %v", err)
				}
			}
			if err := c.Get(ctx, client.ObjectKeyFromObject(account), &corev1.ServiceAccount{}); !apierrors.IsNotFound(err) {
				t.Fatalf("execution account survived drain: %v", err)
			}
			var finished tauv1alpha1.TauWorkspace
			err := c.Get(ctx, client.ObjectKeyFromObject(workspace), &finished)
			if lifecycle == "delete" && !apierrors.IsNotFound(err) {
				t.Fatalf("workspace survived completed source Job drain: %v", err)
			}
			if lifecycle == "retarget" && (err != nil || finished.Status.Target.ResolvedNamespace != newNamespace.Name) {
				t.Fatalf("retarget did not complete after source Job drain: %v, target=%q", err, finished.Status.Target.ResolvedNamespace)
			}
		})
	}
}

func TestNamespaceRoutingMetadataSourceJobs(t *testing.T) {
	for _, scenario := range []string{
		"suspended", "running", "complete", "failed", "false-completion",
		"unlabeled", "other-namespace", "missing-queue", "selector-independent",
		"workload-finished", "workload-wrong-uid", "workload-wrong-name",
		"job-list-error", "queue-read-error", "cluster-queue-read-error",
		"invalid-selector", "cluster-queue-uid-changed",
	} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			workspace := testWorkspace("aurora")
			workspace.Status.Queue = tauv1alpha1.WorkspaceQueueStatus{
				ClusterQueue: "jobqueue", ClusterQueueUID: "jobqueue-uid",
			}
			namespace := testNamespace(workspace.Name)
			namespace.Labels = workspaceNamespaceLabels(workspace.Name, "jobqueue")
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "training", Namespace: namespace.Name, UID: types.UID("training-job-uid"),
				Labels: map[string]string{"kueue.x-k8s.io/queue-name": "jobqueue"},
			}, Spec: batchv1.JobSpec{Suspend: ptr.To(true)}}
			localQueue := testLocalQueue(namespace.Name, "jobqueue", "jobqueue")
			localQueue.SetAnnotations(nil)
			clusterQueue := clusterQueueWithUID("jobqueue", "jobqueue-uid")
			clusterQueue.Object["spec"] = map[string]any{"namespaceSelector": map[string]any{
				"matchLabels": map[string]any{labelWorkspace: workspace.Name},
			}}
			workload := newQueueObject(workloadGVK)
			workload.SetName("job-training")
			workload.SetNamespace(namespace.Name)
			workload.SetOwnerReferences([]metav1.OwnerReference{{
				APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name, UID: job.UID,
			}})
			workload.Object["spec"] = map[string]any{"queueName": "jobqueue"}
			workload.Object["status"] = map[string]any{"conditions": []any{
				map[string]any{"type": "Finished", "status": "True"},
			}}
			wantInUse, wantError := true, false
			objects := []client.Object{namespace, job, localQueue, clusterQueue}
			switch scenario {
			case "running":
				job.Spec.Suspend = ptr.To(false)
			case "complete", "failed", "false-completion":
				conditionType, status := batchv1.JobComplete, corev1.ConditionTrue
				if scenario == "failed" {
					conditionType = batchv1.JobFailed
				}
				if scenario == "false-completion" {
					status = corev1.ConditionFalse
				} else {
					wantInUse = false
				}
				job.Status.Conditions = []batchv1.JobCondition{{Type: conditionType, Status: status}}
			case "unlabeled":
				job.Labels = nil
				wantInUse = false
			case "other-namespace":
				job.Namespace = "unrelated"
				wantInUse = false
			case "missing-queue":
				job.Labels["kueue.x-k8s.io/queue-name"] = "missing"
				wantInUse = false
			case "selector-independent":
				clusterQueue.Object["spec"] = map[string]any{"namespaceSelector": map[string]any{}}
				wantInUse = false
			case "workload-finished", "workload-wrong-uid", "workload-wrong-name":
				if scenario == "workload-finished" {
					wantInUse = false
				} else {
					owner := workload.GetOwnerReferences()[0]
					if scenario == "workload-wrong-uid" {
						owner.UID = types.UID("different-job-uid")
					} else {
						owner.Name = "different-job"
					}
					workload.SetOwnerReferences([]metav1.OwnerReference{owner})
				}
				objects = append(objects, workload)
			case "job-list-error", "queue-read-error", "cluster-queue-read-error":
				wantInUse, wantError = false, true
			case "invalid-selector":
				clusterQueue.Object["spec"] = map[string]any{"namespaceSelector": map[string]any{
					"matchExpressions": []any{map[string]any{"key": labelWorkspace, "operator": "Invalid"}},
				}}
				wantInUse, wantError = false, true
			case "cluster-queue-uid-changed":
				clusterQueue.SetUID(types.UID("replacement-uid"))
				wantInUse, wantError = false, true
			}
			baseClient := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objects...).Build()
			c := interceptor.NewClient(baseClient, interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if _, ok := list.(*batchv1.JobList); ok && scenario == "job-list-error" {
						return errors.New("source Jobs unavailable")
					}
					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					if queue, ok := obj.(*unstructured.Unstructured); ok &&
						(scenario == "queue-read-error" && queue.GroupVersionKind() == localQueueGVK ||
							scenario == "cluster-queue-read-error" && queue.GroupVersionKind() == clusterQueueGVK) {
						return errors.New("queue unavailable")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			inUse, message, err := newTestWorkspaceReconciler(c).namespaceRoutingMetadataInUse(ctx, workspace, namespace)
			if inUse != wantInUse || (err != nil) != wantError {
				t.Fatalf("namespaceRoutingMetadataInUse() = %v, %q, %v; want inUse=%v error=%v", inUse, message, err, wantInUse, wantError)
			}
			if inUse && !strings.Contains(message, "Job "+client.ObjectKeyFromObject(job).String()) {
				t.Fatalf("drain message does not identify the source Job: %q", message)
			}
			for _, queue := range []*unstructured.Unstructured{localQueue, clusterQueue} {
				var got unstructured.Unstructured
				got.SetGroupVersionKind(queue.GroupVersionKind())
				if err := baseClient.Get(ctx, client.ObjectKeyFromObject(queue), &got); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got.Object, queue.Object) {
					t.Fatalf("external queue was mutated: %#v", got.Object)
				}
			}
		})
	}
}
