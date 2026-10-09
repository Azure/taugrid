// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package multisite validates independent workload execution across distinct
// authoritative TauGrid sites.
package multisite

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	e2e "github.com/Azure/taugrid/tests/e2e"
	"github.com/Azure/taugrid/tests/e2e/results"
)

const (
	jobA = "e2e-multisite-a"
	jobB = "e2e-multisite-b"
)

type hardwareSiteTarget struct {
	Name     string `json:"name"`
	Selector string `json:"selector"`
	Site     string `json:"site"`
}

func TestMain(m *testing.M) {
	code := m.Run()
	results.FlushAll()
	os.Exit(code)
}

func TestConcurrentIndependentGPUWorkloadsAcrossSites(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())

	namespace := requireEnv(t, "E2E_MULTISITE_NAMESPACE")
	queue := requireEnv(t, "E2E_MULTISITE_QUEUE")
	image := requireEnv(t, "RAY_E2E_IMAGE")
	siteAKey, siteAValue := parseSimpleSelector(t, requireEnv(t, "E2E_MULTISITE_SITE_A_SELECTOR"))
	siteBKey, siteBValue := parseSimpleSelector(t, requireEnv(t, "E2E_MULTISITE_SITE_B_SELECTOR"))

	client := tc.KubeClient()
	for _, job := range []*batchv1.Job{
		gpuJob(namespace, queue, jobA, image, siteAKey, siteAValue),
		gpuJob(namespace, queue, jobB, image, siteBKey, siteBValue),
	} {
		_, err := client.BatchV1().Jobs(namespace).Create(tc.Ctx(), job, metav1.CreateOptions{})
		require.NoError(t, err, "create Job %s", job.Name)
		jobName := job.Name
		t.Cleanup(func() {
			propagation := metav1.DeletePropagationForeground
			err := client.BatchV1().Jobs(namespace).Delete(tc.Ctx(), jobName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			})
			if err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("delete Job %s/%s: %v", namespace, jobName, err)
			}
		})
	}

	_, err := tc.WaitForWorkloadAdmitted(namespace, jobA, 2*time.Minute)
	require.NoError(t, err, "site A workload should be admitted")
	_, err = tc.WaitForWorkloadAdmitted(namespace, jobB, 2*time.Minute)
	require.NoError(t, err, "site B workload should be admitted")

	podA, podB := waitForConcurrentRunningPods(t, tc, namespace, 10*time.Minute)
	requirePodSite(t, tc, podA, siteAValue)
	requirePodSite(t, tc, podB, siteBValue)
	require.NotEqual(t, siteAValue, siteBValue, "multi-site workloads must target distinct sites")

	waitForJobComplete(t, tc, namespace, jobA, 5*time.Minute)
	waitForJobComplete(t, tc, namespace, jobB, 5*time.Minute)
}

func TestConcurrentGPUWorkloadsAcrossHardwareSites(t *testing.T) {
	tc := e2e.NewTestContext(t, context.Background())

	namespace := requireEnv(t, "E2E_MULTISITE_NAMESPACE")
	queuePrefix := requireEnv(t, "E2E_MULTISITE_QUEUE_PREFIX")
	image := requireEnv(t, "RAY_E2E_IMAGE")
	targets := requireHardwareSiteTargets(t)
	require.GreaterOrEqual(t, len(targets), 2, "at least two available hardware sites are required")

	jobSites := make(map[string]string, len(targets))
	for _, target := range targets {
		selectorKey, selectorValue := parseSimpleSelector(t, target.Selector)
		jobName := "e2e-multisite-" + dnsLabel(target.Name)
		queue := queuePrefix + "-" + dnsLabel(target.Name)
		jobSites[jobName] = target.Site

		job := gpuJob(namespace, queue, jobName, image, selectorKey, selectorValue)
		_, err := tc.KubeClient().BatchV1().Jobs(namespace).Create(tc.Ctx(), job, metav1.CreateOptions{})
		require.NoError(t, err, "create Job %s", jobName)
		t.Cleanup(func() {
			propagation := metav1.DeletePropagationForeground
			err := tc.KubeClient().BatchV1().Jobs(namespace).Delete(tc.Ctx(), jobName, metav1.DeleteOptions{
				PropagationPolicy: &propagation,
			})
			if err != nil && !apierrors.IsNotFound(err) {
				t.Errorf("delete Job %s/%s: %v", namespace, jobName, err)
			}
		})

		_, err = tc.WaitForWorkloadAdmitted(namespace, jobName, 2*time.Minute)
		require.NoError(t, err, "%s workload should be admitted", target.Name)
	}

	running := waitForConcurrentRunningJobPods(t, tc, namespace, jobSites, 10*time.Minute)
	observedSites := map[string]struct{}{}
	for jobName, pod := range running {
		expectedSite := jobSites[jobName]
		requirePodSite(t, tc, pod, expectedSite)
		observedSites[expectedSite] = struct{}{}
	}
	require.Len(t, observedSites, len(targets), "hardware targets must use distinct authoritative sites")

	for jobName := range jobSites {
		waitForJobComplete(t, tc, namespace, jobName, 5*time.Minute)
	}
}

func gpuJob(namespace, queue, name, image, selectorKey, selectorValue string) *batchv1.Job {
	suspend := true
	backoffLimit := int32(0)
	ttl := int32(300)

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"kueue.x-k8s.io/queue-name": queue,
			},
		},
		Spec: batchv1.JobSpec{
			Suspend:                 &suspend,
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"tau.azure.com/e2e-multisite": name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					NodeSelector: map[string]string{
						selectorKey: selectorValue,
					},
					Tolerations: []corev1.Toleration{
						{
							Key:      "nvidia.com/gpu",
							Operator: corev1.TolerationOpExists,
							Effect:   corev1.TaintEffectNoSchedule,
						},
						{
							Key:      "sku",
							Operator: corev1.TolerationOpEqual,
							Value:    "gpu",
							Effect:   corev1.TaintEffectNoSchedule,
						},
					},
					Containers: []corev1.Container{
						{
							Name:    "worker",
							Image:   image,
							Command: []string{"sh", "-c", "nvidia-smi --query-gpu=name --format=csv,noheader && sleep 180"},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:                    resource.MustParse("1"),
									corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceName("nvidia.com/gpu"): resource.MustParse("1"),
								},
							},
						},
					},
				},
			},
		},
	}
}

func waitForConcurrentRunningPods(t *testing.T, tc *e2e.TestContext, namespace string, timeout time.Duration) (*corev1.Pod, *corev1.Pod) {
	t.Helper()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		podA := runningJobPod(t, tc, namespace, jobA)
		podB := runningJobPod(t, tc, namespace, jobB)
		if podA != nil && podB != nil {
			return podA, podB
		}
		time.Sleep(2 * time.Second)
	}

	tc.DumpPods(namespace, "")
	tc.DumpEvents(namespace)
	t.Fatalf("Jobs %s and %s were not Running concurrently within %s", jobA, jobB, timeout)
	return nil, nil
}

func waitForConcurrentRunningJobPods(
	t *testing.T,
	tc *e2e.TestContext,
	namespace string,
	jobSites map[string]string,
	timeout time.Duration,
) map[string]*corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		running := make(map[string]*corev1.Pod, len(jobSites))
		for jobName := range jobSites {
			if pod := runningJobPod(t, tc, namespace, jobName); pod != nil {
				running[jobName] = pod
			}
		}
		if len(running) == len(jobSites) {
			return running
		}
		time.Sleep(2 * time.Second)
	}

	tc.DumpPods(namespace, "")
	tc.DumpEvents(namespace)
	t.Fatalf("%d hardware-site Jobs were not Running concurrently within %s", len(jobSites), timeout)
	return nil
}

func runningJobPod(t *testing.T, tc *e2e.TestContext, namespace, jobName string) *corev1.Pod {
	t.Helper()
	pods, err := tc.KubeClient().CoreV1().Pods(namespace).List(tc.Ctx(), metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	require.NoError(t, err, "list pods for Job %s", jobName)
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			return &pods.Items[i]
		}
	}
	return nil
}

func requirePodSite(t *testing.T, tc *e2e.TestContext, pod *corev1.Pod, expectedSite string) {
	t.Helper()
	node, err := tc.KubeClient().CoreV1().Nodes().Get(tc.Ctx(), pod.Spec.NodeName, metav1.GetOptions{})
	require.NoError(t, err, "get Node %s for Pod %s", pod.Spec.NodeName, pod.Name)
	require.Equal(t, expectedSite, node.Labels["tau.azure.com/site"],
		"Pod %s should run in authoritative site %s", pod.Name, expectedSite)
}

func waitForJobComplete(t *testing.T, tc *e2e.TestContext, namespace, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		job, err := tc.KubeClient().BatchV1().Jobs(namespace).Get(tc.Ctx(), name, metav1.GetOptions{})
		require.NoError(t, err, "get Job %s", name)
		for _, condition := range job.Status.Conditions {
			if condition.Type == batchv1.JobComplete && condition.Status == corev1.ConditionTrue {
				return
			}
			if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
				t.Fatalf("Job %s failed: %s", name, condition.Message)
			}
		}
		time.Sleep(2 * time.Second)
	}

	t.Fatalf("Job %s/%s did not complete within %s", namespace, name, timeout)
}

func parseSimpleSelector(t *testing.T, selector string) (string, string) {
	t.Helper()
	key, value, found := strings.Cut(selector, "=")
	require.True(t, found, "selector %q must use key=value form", selector)
	require.NotEmpty(t, key, "selector key must not be empty")
	require.NotEmpty(t, value, "selector value must not be empty")
	require.NotContains(t, key, ",", "selector %q must contain one equality expression", selector)
	require.NotContains(t, value, ",", "selector %q must contain one equality expression", selector)
	return key, value
}

func requireEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	require.NotEmpty(t, value, "%s is required", name)
	return value
}

func requireHardwareSiteTargets(t *testing.T) []hardwareSiteTarget {
	t.Helper()
	raw := requireEnv(t, "E2E_MULTISITE_TARGETS_JSON")
	var targets []hardwareSiteTarget
	require.NoError(t, json.Unmarshal([]byte(raw), &targets), "parse E2E_MULTISITE_TARGETS_JSON")
	require.NotEmpty(t, targets)

	names := map[string]struct{}{}
	sites := map[string]struct{}{}
	for _, target := range targets {
		require.NotEmpty(t, target.Name)
		require.NotEmpty(t, target.Site)
		parseSimpleSelector(t, target.Selector)
		_, duplicateName := names[target.Name]
		require.False(t, duplicateName, "duplicate hardware target %q", target.Name)
		_, duplicateSite := sites[target.Site]
		require.False(t, duplicateSite, "duplicate hardware site %q", target.Site)
		names[target.Name] = struct{}{}
		sites[target.Site] = struct{}{}
	}
	return targets
}

var nonDNSLabel = regexp.MustCompile(`[^a-z0-9-]+`)

func dnsLabel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = nonDNSLabel.ReplaceAllString(value, "-")
	value = strings.Trim(value, "-")
	if len(value) > 45 {
		value = value[:45]
	}
	return value
}

func TestParseSimpleSelector(t *testing.T) {
	key, value := parseSimpleSelector(t, "tau.azure.com/site=site-a")
	require.Equal(t, "tau.azure.com/site", key)
	require.Equal(t, "site-a", value)
}

func TestGPUJobContract(t *testing.T) {
	job := gpuJob("nightly", "queue", "site-a", "ray:latest", "tau.azure.com/site", "site-a")

	require.NotNil(t, job.Spec.Suspend)
	require.True(t, *job.Spec.Suspend)
	require.Equal(t, "queue", job.Labels["kueue.x-k8s.io/queue-name"])
	require.Equal(t, "site-a", job.Spec.Template.Spec.NodeSelector["tau.azure.com/site"])
	require.Equal(t, resource.MustParse("1"),
		job.Spec.Template.Spec.Containers[0].Resources.Requests[corev1.ResourceName("nvidia.com/gpu")])
	require.Contains(t, job.Spec.Template.Spec.Tolerations, corev1.Toleration{
		Key:      "nvidia.com/gpu",
		Operator: corev1.TolerationOpExists,
		Effect:   corev1.TaintEffectNoSchedule,
	})
	require.Contains(t, job.Spec.Template.Spec.Tolerations, corev1.Toleration{
		Key:      "sku",
		Operator: corev1.TolerationOpEqual,
		Value:    "gpu",
		Effect:   corev1.TaintEffectNoSchedule,
	})
}

func TestRequireHardwareSiteTargets(t *testing.T) {
	t.Setenv("E2E_MULTISITE_TARGETS_JSON", `[
	  {"name":"dgx-spark","selector":"example.com/node-pool=spark","site":"site-dgx"},
	  {"name":"h200","selector":"kueue.azure.com/gpu-series=nd-h200-v5","site":"site-h200"}
	]`)
	targets := requireHardwareSiteTargets(t)
	require.Len(t, targets, 2)
	require.Equal(t, "dgx-spark", targets[0].Name)
	require.Equal(t, "site-h200", targets[1].Site)
}

func Example_gpuJob() {
	job := gpuJob("nightly", "queue", "site-a", "ray:latest", "tau.azure.com/site", "site-a")
	fmt.Println(job.Spec.Template.Spec.NodeSelector["tau.azure.com/site"])
	// Output: site-a
}
