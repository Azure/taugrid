// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runhistory

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/taugrid/core/experiment"
	portalruns "github.com/Azure/taugrid/core/runs"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func TestKindLifecycleHistorySurvivesJobDeletion(t *testing.T) {
	if os.Getenv("TAU_RUNHISTORY_KIND_E2E") != "1" {
		t.Skip("set TAU_RUNHISTORY_KIND_E2E=1 to run against KUBECONFIG")
	}
	config, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	core, err := kubernetes.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	dynamicClient, err := dynamic.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	source := &KubernetesSource{core: core, dynamic: dynamicClient}
	store := &lifecycleStore{}
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000)

	for _, marker := range []struct {
		name, workspace, queue string
	}{
		{name: "marker-a", workspace: "workspace-a", queue: "queue-a"},
		{name: "marker-b", workspace: "workspace-b", queue: "queue-b"},
	} {
		namespace := "runhistory-" + marker.name + "-" + suffix
		if _, err := core.CoreV1().Namespaces().Create(context.Background(), &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		}, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = core.CoreV1().Namespaces().Delete(context.Background(), namespace, metav1.DeleteOptions{})
		})

		suspend := true
		job, err := core.BatchV1().Jobs(namespace).Create(context.Background(), &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{
				Name: marker.name,
				Labels: map[string]string{
					experiment.LabelRunID:        marker.name,
					workloadmeta.LabelWorkspace:  marker.workspace,
					"kueue.x-k8s.io/queue-name":  marker.queue,
					experiment.LabelWorkloadKind: experiment.WorkloadKindJob,
				},
			},
			Spec: batchv1.JobSpec{
				Suspend: &suspend,
				Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name: "marker", Image: "mcr.microsoft.com/azurelinux/base/core:3.0",
						Command: []string{"/bin/true"},
					}},
				}},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			t.Fatal(err)
		}
		now := metav1.Now()
		job.Status.Succeeded = 1
		job.Status.CompletionTime = &now
		job.Status.Conditions = []batchv1.JobCondition{{
			Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
			LastTransitionTime: now,
		}}
		if _, err := core.BatchV1().Jobs(namespace).UpdateStatus(context.Background(), job, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}

		reconciler := &Reconciler{
			Source: source, Writer: store, Cluster: "kind-runhistory",
			Now: func() time.Time { return now.Time },
		}
		if _, err := reconciler.Reconcile(context.Background(), namespace); err != nil {
			t.Fatalf("record %s: %v", marker.name, err)
		}
		if err := core.BatchV1().Jobs(namespace).Delete(context.Background(), marker.name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		jobs, err := source.ListJobs(context.Background(), namespace)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 0 {
			t.Fatalf("%s still exists after deletion", marker.name)
		}

		snapshot, err := portalruns.Board(context.Background(), emptyRunsReader{}, portalruns.Options{
			Namespace: namespace, Queue: marker.queue, History: store,
			HistoryScope: portalruns.HistoryScope{
				Cluster: "kind-runhistory", Namespace: namespace,
				LocalQueue: marker.queue, WorkspaceID: marker.workspace,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(snapshot.Runs) != 1 || snapshot.Runs[0].Name != marker.name {
			t.Fatalf("%s durable rows = %+v", marker.workspace, snapshot.Runs)
		}
	}
}
