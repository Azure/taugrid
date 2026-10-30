package main

import (
	"context"
	"flag"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	profileName = "gpu-a100-8"
	queueName   = "gpu-a100"
	flavorName  = "a100-scale"
)

func runSubmit(ctx context.Context, c *clients, args []string) error {
	fs := flag.NewFlagSet("submit", flag.ExitOnError)
	name := fs.String("name", "train", "run name")
	ns := fs.String("n", "team-a", "workspace namespace")
	workers := fs.Int64("workers", 2, "worker pods")
	gpus := fs.Int64("gpus", 8, "GPUs per worker")
	run := fs.Duration("run", 20*time.Second, "simulated training time once Running")
	_ = fs.Parse(args)

	requests := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("4"),
		corev1.ResourceMemory: resource.MustParse("16Gi"),
		"nvidia.com/gpu":      *resource.NewQuantity(*gpus, resource.DecimalSI),
	}
	if err := preflight(ctx, c, *ns, *workers, requests); err != nil {
		return err
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      *name,
			Namespace: *ns,
			Labels: map[string]string{
				labelQueue:   queueName,
				labelProfile: profileName,
			},
		},
		Spec: batchv1.JobSpec{
			Suspend:                 ptr(true), // Kueue unsuspends on admission
			Parallelism:             ptr(int32(*workers)),
			Completions:             ptr(int32(*workers)),
			CompletionMode:          ptr(batchv1.IndexedCompletion),
			BackoffLimit:            ptr(int32(0)),
			TTLSecondsAfterFinished: ptr(int32(20)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
					"pod-complete.stage.kwok.x-k8s.io/delay":        run.String(),
					// KWOK picks the delay uniformly in [delay, jitter-delay].
					"pod-complete.stage.kwok.x-k8s.io/jitter-delay": (*run + 2*time.Second).String(),
				}},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:      "trainer",
						Image:     "ghcr.io/example/trainer:demo", // never pulled: KWOK simulates the container
						Resources: corev1.ResourceRequirements{Requests: requests, Limits: corev1.ResourceList{"nvidia.com/gpu": requests["nvidia.com/gpu"]}},
					}},
				},
			},
		},
	}
	if _, err := c.core.BatchV1().Jobs(*ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return err
	}
	fmt.Printf("submitted %s/%s: %d worker(s) x %d GPU, profile %s, LocalQueue %s (suspended until Kueue admits)\n",
		*ns, *name, *workers, *gpus, profileName, queueName)
	return nil
}

// preflight is layer 1 of the bounding design: refuse to submit if the profile's
// capacity wiring isn't ready, or if the gang can never fit the node pool.
func preflight(ctx context.Context, c *clients, ns string, workers int64, req corev1.ResourceList) error {
	if err := provisioningReady(ctx, c, ns); err != nil {
		return fmt.Errorf("profile %s is not ProvisioningReady: %w", profileName, err)
	}
	fmt.Printf("preflight: profile %s ProvisioningReady=True\n", profileName)

	pool, err := loadPool(ctx, c)
	if err != nil {
		fmt.Printf("preflight: warning: can't read node pool limits (%v); skipping gang-fit check\n", err)
		return nil
	}
	need := pool.nodesFor(workers, req)
	if need > int64(pool.MaxNodes) {
		return fmt.Errorf("profile %s requests %d node(s); node pool %s max_count is %d.\n"+
			"       This request can never be provisioned.",
			profileName, need, pool.Name, pool.MaxNodes)
	}
	fmt.Printf("preflight: gang needs %d node(s) of %s (max %d): fits\n", need, pool.Name, pool.MaxNodes)
	return nil
}

// provisioningReady is what the profile observer checks for capacity.provisioning: Required.
func provisioningReady(ctx context.Context, c *clients, ns string) error {
	lq, err := c.dyn.Resource(localQueueGVR).Namespace(ns).Get(ctx, queueName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("LocalQueue %s/%s: %w", ns, queueName, err)
	}
	cqName, _, _ := unstructured.NestedString(lq.Object, "spec", "clusterQueue")
	cq, err := c.dyn.Resource(clusterQueueGVR).Get(ctx, cqName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("ClusterQueue %s: %w", cqName, err)
	}
	checks, _, _ := unstructured.NestedSlice(cq.Object, "spec", "admissionChecksStrategy", "admissionChecks")
	for _, item := range checks {
		m, _ := item.(map[string]any)
		ac, err := c.dyn.Resource(admissionCheckGVR).Get(ctx, str(m["name"]), metav1.GetOptions{})
		if err != nil {
			continue
		}
		controller, _, _ := unstructured.NestedString(ac.Object, "spec", "controllerName")
		if controller != provisioningController || !isTrue(ac, "Active") {
			continue
		}
		onFlavors, _, _ := unstructured.NestedStringSlice(m, "onFlavors")
		if len(onFlavors) == 0 || contains(onFlavors, flavorName) {
			return nil
		}
	}
	return fmt.Errorf("ClusterQueue %s has no active %s check covering flavor %s", cqName, provisioningController, flavorName)
}

func ptr[T any](v T) *T { return &v }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
