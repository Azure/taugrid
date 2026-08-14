// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gpumonitoring

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"

	e2e "github.com/Azure/taugrid/tests/e2e"
)

const (
	gpuMonitoringRuntimeEnv       = "AI_RUNTIME_GPU_MONITORING_RUNTIME"
	gpuMonitoringNamespaceEnv     = "AI_RUNTIME_GPU_MONITORING_NAMESPACE"
	gpuMonitoringNodeExporterEnv  = "AI_RUNTIME_GPU_MONITORING_NODE_EXPORTER_CONTAINER"
	gpuMonitoringNodeTimeout      = 3 * time.Minute
	gpuMonitoringPodTimeout       = 8 * time.Minute
	gpuMonitoringProbeTimeout     = 6 * time.Minute
	gpuMonitoringLogTimeout       = 90 * time.Second
	gpuMonitoringPollInterval     = 5 * time.Second
	gpuMonitoringTerminationLimit = 512
)

type gpuMonitoringRuntimeCase struct {
	name                        string
	selector                    string
	expectNodeExporterContainer bool
}

func TestGPUMonitoringRuntimeOnGPUNode(t *testing.T) {
	e2e.SkipUnlessE2E(t)
	if os.Getenv(gpuMonitoringRuntimeEnv) != "1" {
		t.Skip("Skipping gpu-monitoring runtime e2e: set AI_RUNTIME_GPU_MONITORING_RUNTIME=1 to run")
	}

	cases := configuredGPUMonitoringRuntimeCases()
	if len(cases) == 0 {
		t.Skip("Skipping gpu-monitoring runtime e2e: no GPU monitoring selectors configured")
	}

	ctx := context.Background()
	tc := e2e.NewTestContext(t, ctx)
	if suffix := strings.TrimSpace(os.Getenv("E2E_TEST_NAME_SUFFIX")); suffix != "" {
		tc.RecordOutcomeAs(t.Name() + "/" + suffix)
	}
	kubeClient := tc.KubeClient()
	namespace := strings.TrimSpace(os.Getenv(gpuMonitoringNamespaceEnv))
	if namespace == "" {
		namespace = gpuMonitoringNamespace
	}
	_, err := kubeClient.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	require.NoError(t, err, "gpu-monitoring namespace %s should exist before runtime test", namespace)

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			runGPUMonitoringRuntimeCase(t, ctx, kubeClient, namespace, c)
		})
	}
}

func configuredGPUMonitoringRuntimeCases() []gpuMonitoringRuntimeCase {
	var out []gpuMonitoringRuntimeCase
	expectNodeExporterContainer := envBoolDefault(gpuMonitoringNodeExporterEnv, true)

	add := func(name, selectorEnv, fallbackSelectorEnv string) {
		selector := strings.TrimSpace(os.Getenv(selectorEnv))
		if selector == "" {
			selector = strings.TrimSpace(os.Getenv(fallbackSelectorEnv))
		}
		if selector == "" {
			return
		}
		out = append(out, gpuMonitoringRuntimeCase{
			name:                        name,
			selector:                    selector,
			expectNodeExporterContainer: expectNodeExporterContainer,
		})
	}

	add("a10", "AI_RUNTIME_GPU_MONITORING_A10_SELECTOR", "AI_RUNTIME_GPU_A10_SELECTOR")
	add("a100", "AI_RUNTIME_GPU_MONITORING_A100_SELECTOR", "AI_RUNTIME_GPU_A100_SELECTOR")
	add("h100", "AI_RUNTIME_GPU_MONITORING_H100_SELECTOR", "AI_RUNTIME_GPU_H100_SELECTOR")
	add("h200", "AI_RUNTIME_GPU_MONITORING_H200_SELECTOR", "AI_RUNTIME_GPU_H200_SELECTOR")
	add("gb200", "AI_RUNTIME_GPU_MONITORING_GB200_SELECTOR", "AI_RUNTIME_GPU_GB200_SELECTOR")
	add("gb300", "AI_RUNTIME_GPU_MONITORING_GB300_SELECTOR", "AI_RUNTIME_GPU_GB300_SELECTOR")

	return out
}

func TestConfiguredGPUMonitoringRuntimeCasesIncludesSupportedFamilies(t *testing.T) {
	for _, family := range []string{"A10", "A100", "H100", "H200", "GB200", "GB300"} {
		t.Setenv("AI_RUNTIME_GPU_MONITORING_"+family+"_SELECTOR", "gpu.example.com/family="+strings.ToLower(family))
	}

	got := make(map[string]gpuMonitoringRuntimeCase)
	for _, c := range configuredGPUMonitoringRuntimeCases() {
		got[c.name] = c
	}
	for _, family := range []string{"a10", "a100", "h100", "h200", "gb200", "gb300"} {
		require.Contains(t, got, family)
	}
}

func TestDCGMURLForMonitoringPodReadsCollectorConfigMap(t *testing.T) {
	const (
		namespace     = "gpu-monitoring"
		configMapName = "gpu-monitoring-gpu-metrics-collector-h200"
		wantURL       = "http://nvidia-dcgm-exporter.gpu-operator.svc:9400/metrics"
	)
	kubeClient := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: configMapName, Namespace: namespace},
		Data: map[string]string{
			"rules.yaml": "scrapeTargets:\n  - name: dcgm-exporter\n    url: " + wantURL + "\n",
		},
	})
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "gpu-monitoring-h200", Namespace: namespace},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "collector-config",
				VolumeSource: corev1.VolumeSource{
					ConfigMap: &corev1.ConfigMapVolumeSource{
						LocalObjectReference: corev1.LocalObjectReference{Name: configMapName},
					},
				},
			}},
		},
	}

	require.Equal(t, wantURL, dcgmURLForMonitoringPod(t, context.Background(), kubeClient, pod))
}

func runGPUMonitoringRuntimeCase(
	t *testing.T,
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	c gpuMonitoringRuntimeCase,
) {
	t.Helper()

	node := waitForGPUMonitoringNode(t, ctx, kubeClient, c.selector, gpuMonitoringNodeTimeout)
	t.Logf("Using node %q for gpu-monitoring runtime case %s (selector=%q)", node.Name, c.name, c.selector)

	pod := waitForGPUMonitoringPodOnNode(t, ctx, kubeClient, namespace, node.Name, gpuMonitoringPodTimeout)
	dcgmURL := dcgmURLForMonitoringPod(t, ctx, kubeClient, pod)
	requireNodeLocalDCGMEndpoint(t, ctx, kubeClient, dcgmURL)
	requireContainerReady(t, pod, "gpu-monitoring")
	if c.expectNodeExporterContainer {
		requireContainerReady(t, pod, "node-exporter")
	} else {
		requireContainerAbsent(t, pod, "node-exporter")
	}
	requireContainerReady(t, pod, "metrics-collector")

	logs, err := waitForContainerLogContains(ctx, kubeClient, namespace, pod.Name, "metrics-collector", []string{
		"loaded config",
		"starting gpu-metrics-collector",
	}, gpuMonitoringLogTimeout)
	require.NoError(t, err, "metrics-collector should load config and start on pod %s/%s; last logs:\n%s", namespace, pod.Name, logs)
	require.NotContains(t, logs, `"level":"ERROR","msg":"fatal"`, "metrics-collector should not log a fatal startup error")

	jobName := fmt.Sprintf("gpu-monitoring-probe-%s-%s", c.name, rand.String(5))
	job := newGPUMonitoringProbeJob(namespace, jobName, node.Name, dcgmURL)
	_, err = kubeClient.BatchV1().Jobs(namespace).Create(ctx, job, metav1.CreateOptions{})
	require.NoError(t, err, "create gpu-monitoring probe job %s/%s", namespace, jobName)
	t.Cleanup(func() {
		background := metav1.DeletePropagationBackground
		_ = kubeClient.BatchV1().Jobs(namespace).Delete(ctx, jobName, metav1.DeleteOptions{PropagationPolicy: &background})
	})

	probePod, err := waitForGPUMonitoringProbeJobSuccess(ctx, kubeClient, namespace, jobName, gpuMonitoringProbeTimeout)
	require.NoError(t, err, "wait for gpu-monitoring probe job %s/%s to complete", namespace, jobName)
	term := gpumonitoringTerminatedState(probePod)
	require.NotNil(t, term, "expected terminated state for probe pod %s", probePod.Name)
	require.EqualValues(t, 0, term.ExitCode, "probe pod %s should exit 0", probePod.Name)
	require.Contains(t, term.Message, "RESULT_NPD=PASS", "NPD Prometheus endpoint should be reachable on %s", node.Name)
	require.Contains(t, term.Message, "RESULT_NODE_EXPORTER=PASS", "node-exporter endpoint should be reachable on %s", node.Name)
	require.Contains(t, term.Message, "RESULT_DCGM=PASS", "configured DCGM endpoint should be reachable on %s", node.Name)
}

func waitForGPUMonitoringNode(
	t *testing.T,
	ctx context.Context,
	kubeClient kubernetes.Interface,
	selector string,
	timeout time.Duration,
) corev1.Node {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastSeen []string
	for time.Now().Before(deadline) {
		nodes, err := kubeClient.CoreV1().Nodes().List(ctx, metav1.ListOptions{LabelSelector: selector})
		require.NoError(t, err, "list nodes with selector %q", selector)

		lastSeen = lastSeen[:0]
		for _, node := range nodes.Items {
			alloc, ok := node.Status.Allocatable[corev1.ResourceName("nvidia.com/gpu")]
			allocStr := "<none>"
			if ok {
				allocStr = alloc.String()
				if alloc.Sign() > 0 {
					return node
				}
			}
			lastSeen = append(lastSeen, fmt.Sprintf("%s(gpu=%s)", node.Name, allocStr))
		}

		time.Sleep(gpuMonitoringPollInterval)
	}

	require.FailNowf(
		t,
		"no GPU allocatable node found",
		"selector=%q timeout=%s lastSeen=%s",
		selector,
		timeout.String(),
		strings.Join(lastSeen, ", "),
	)
	return corev1.Node{}
}

func waitForGPUMonitoringPodOnNode(
	t *testing.T,
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	nodeName string,
	timeout time.Duration,
) *corev1.Pod {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastSeen []string
	for time.Now().Before(deadline) {
		pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=gpu-monitoring",
		})
		require.NoError(t, err, "list gpu-monitoring pods in namespace %s", namespace)

		lastSeen = lastSeen[:0]
		for i := range pods.Items {
			pod := &pods.Items[i]
			if pod.Spec.NodeName != nodeName {
				continue
			}
			lastSeen = append(lastSeen, fmt.Sprintf("%s(phase=%s ready=%t sku=%s)",
				pod.Name,
				pod.Status.Phase,
				isGPUMonitoringPodReady(pod),
				pod.Labels["gpu-sku"],
			))
			if pod.Status.Phase == corev1.PodRunning && isGPUMonitoringPodReady(pod) {
				return pod
			}
		}

		time.Sleep(gpuMonitoringPollInterval)
	}

	require.FailNowf(
		t,
		"no ready gpu-monitoring pod found on node",
		"namespace=%s node=%s timeout=%s lastSeen=%s",
		namespace,
		nodeName,
		timeout.String(),
		strings.Join(lastSeen, ", "),
	)
	return nil
}

func requireNodeLocalDCGMEndpoint(
	t *testing.T,
	ctx context.Context,
	kubeClient kubernetes.Interface,
	rawURL string,
) {
	t.Helper()
	if rawURL == "" {
		return
	}

	endpoint, err := url.Parse(rawURL)
	require.NoError(t, err, "parse DCGM URL")
	host := strings.TrimSuffix(strings.ToLower(endpoint.Hostname()), ".")
	ip := net.ParseIP(host)
	if host == "localhost" || (ip != nil && ip.IsLoopback()) {
		return
	}

	parts := strings.Split(host, ".")
	require.GreaterOrEqual(t, len(parts), 3, "non-loopback DCGM URL must use Kubernetes Service DNS")
	require.Equal(t, "svc", parts[2], "non-loopback DCGM URL must use Kubernetes Service DNS")

	service, err := kubeClient.CoreV1().Services(parts[1]).Get(ctx, parts[0], metav1.GetOptions{})
	require.NoError(t, err, "get DCGM Service %s/%s", parts[1], parts[0])
	require.NotNil(t, service.Spec.InternalTrafficPolicy, "DCGM Service must set internalTrafficPolicy")
	require.Equal(t, corev1.ServiceInternalTrafficPolicyLocal, *service.Spec.InternalTrafficPolicy,
		"DCGM Service must route only to node-local exporters")
}

func dcgmURLForMonitoringPod(
	t *testing.T,
	ctx context.Context,
	kubeClient kubernetes.Interface,
	pod *corev1.Pod,
) string {
	t.Helper()

	var configMapName string
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "collector-config" && volume.ConfigMap != nil {
			configMapName = volume.ConfigMap.Name
			break
		}
	}
	require.NotEmpty(t, configMapName, "pod %s/%s should reference a collector ConfigMap", pod.Namespace, pod.Name)

	configMap, err := kubeClient.CoreV1().ConfigMaps(pod.Namespace).Get(ctx, configMapName, metav1.GetOptions{})
	require.NoError(t, err, "get collector ConfigMap %s/%s", pod.Namespace, configMapName)
	raw, ok := configMap.Data["rules.yaml"]
	require.True(t, ok, "collector ConfigMap %s/%s should contain rules.yaml", pod.Namespace, configMapName)

	var config struct {
		ScrapeTargets []struct {
			Name string `yaml:"name"`
			URL  string `yaml:"url"`
		} `yaml:"scrapeTargets"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(raw), &config), "parse collector ConfigMap %s/%s", pod.Namespace, configMapName)
	for _, target := range config.ScrapeTargets {
		if target.Name == "dcgm-exporter" {
			require.NotEmpty(t, target.URL, "DCGM scrape target in %s/%s should have a URL", pod.Namespace, configMapName)
			return target.URL
		}
	}

	require.FailNowf(t, "DCGM scrape target missing", "collector ConfigMap %s/%s has no dcgm-exporter target", pod.Namespace, configMapName)
	return ""
}

func newGPUMonitoringProbeJob(namespace, jobName, nodeName, dcgmURL string) *batchv1.Job {
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name": "gpu-monitoring-probe",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			TTLSecondsAfterFinished: int32Ptr(600),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name": "gpu-monitoring-probe",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					HostNetwork:   true,
					DNSPolicy:     corev1.DNSClusterFirstWithHostNet,
					NodeName:      nodeName,
					Tolerations: []corev1.Toleration{
						{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoSchedule},
						{Operator: corev1.TolerationOpExists, Effect: corev1.TaintEffectNoExecute},
					},
					Containers: []corev1.Container{
						{
							Name:            "probe",
							Image:           "mcr.microsoft.com/azurelinux/base/core:3.0",
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command: []string{
								"/bin/bash",
								"-c",
								gpuMonitoringProbeScript,
							},
							Env: []corev1.EnvVar{
								{Name: "REQUIRE_DCGM", Value: "1"},
								{Name: "DCGM_URL", Value: dcgmURL},
							},
							TerminationMessagePolicy: corev1.TerminationMessageReadFile,
						},
					},
				},
			},
		},
	}
}

const gpuMonitoringProbeScript = `
set -u
tdnf install -y --quiet curl >/dev/null 2>&1 || tdnf install -y curl

RESULT_FILE=/tmp/gpu-monitoring-probe-results.txt
: > "$RESULT_FILE"
fail=0

check_endpoint() {
  name="$1"
  url="$2"
  pattern="$3"
  required="$4"

  out="$(curl -sS --max-time 10 "$url" 2>&1 || true)"
  if printf '%s\n' "$out" | grep -qE "$pattern"; then
    printf 'RESULT_%s=PASS\n' "$name" >> "$RESULT_FILE"
    return
  fi

  if [ "$required" = "1" ]; then
    printf 'RESULT_%s=FAIL\n' "$name" >> "$RESULT_FILE"
    printf 'DETAIL_%s=%s\n' "$name" "$(printf '%s' "$out" | head -c 200 | tr '\n' ' ')" >> "$RESULT_FILE"
    fail=1
  else
    printf 'RESULT_%s=SKIP\n' "$name" >> "$RESULT_FILE"
  fi
}

check_endpoint NPD http://localhost:20261/metrics '^(# HELP|# TYPE|problem_|node_problem_detector_)' 1
check_endpoint NODE_EXPORTER http://localhost:9100/metrics '^node_' 1
check_endpoint DCGM "$DCGM_URL" '^DCGM_' "$REQUIRE_DCGM"

cat "$RESULT_FILE"
cat "$RESULT_FILE" > /dev/termination-log
exit "$fail"
`

func waitForGPUMonitoringProbeJobSuccess(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	jobName string,
	timeout time.Duration,
) (*corev1.Pod, error) {
	deadline := time.Now().Add(timeout)
	var lastSucceeded, lastFailed int32

	for time.Now().Before(deadline) {
		job, err := kubeClient.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
		if err != nil {
			time.Sleep(gpuMonitoringPollInterval)
			continue
		}

		lastSucceeded = job.Status.Succeeded
		lastFailed = job.Status.Failed
		if job.Status.Succeeded > 0 {
			return getGPUMonitoringJobPod(ctx, kubeClient, namespace, jobName)
		}
		if job.Status.Failed > 0 {
			pod, err := getGPUMonitoringJobPod(ctx, kubeClient, namespace, jobName)
			if err == nil {
				if term := gpumonitoringTerminatedState(pod); term != nil {
					msg := term.Message
					if len(msg) > gpuMonitoringTerminationLimit {
						msg = msg[:gpuMonitoringTerminationLimit] + "..."
					}
					return nil, fmt.Errorf("probe job %s/%s failed (pod=%s exit=%d reason=%s message=%q)",
						namespace, jobName, pod.Name, term.ExitCode, term.Reason, msg)
				}
			}
			return nil, fmt.Errorf("probe job %s/%s failed", namespace, jobName)
		}

		time.Sleep(gpuMonitoringPollInterval)
	}

	return nil, fmt.Errorf(
		"timed out waiting for probe job %s/%s to complete (succeeded=%d failed=%d)",
		namespace,
		jobName,
		lastSucceeded,
		lastFailed,
	)
}

func getGPUMonitoringJobPod(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	jobName string,
) (*corev1.Pod, error) {
	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err != nil {
		return nil, fmt.Errorf("list pods for job %s/%s: %w", namespace, jobName, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pods found for job %s/%s", namespace, jobName)
	}
	latest := pods.Items[0]
	for _, pod := range pods.Items[1:] {
		if pod.CreationTimestamp.After(latest.CreationTimestamp.Time) {
			latest = pod
		}
	}
	return &latest, nil
}

func waitForContainerLogContains(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	namespace string,
	podName string,
	container string,
	needles []string,
	timeout time.Duration,
) (string, error) {
	deadline := time.Now().Add(timeout)
	var lastLogs string

	for time.Now().Before(deadline) {
		logs, err := kubeClient.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
			Container: container,
			TailLines: int64Ptr(100),
		}).Do(ctx).Raw()
		if err == nil {
			lastLogs = string(logs)
			if containsAll(lastLogs, needles) {
				return lastLogs, nil
			}
		}
		time.Sleep(gpuMonitoringPollInterval)
	}
	return lastLogs, fmt.Errorf("timed out waiting for container %s logs to contain %q", container, needles)
}

func requireContainerReady(t *testing.T, pod *corev1.Pod, containerName string) {
	t.Helper()
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			require.True(t, status.Ready, "container %s in pod %s/%s should be ready", containerName, pod.Namespace, pod.Name)
			return
		}
	}
	require.Failf(t, "container not found", "container %s not found in pod %s/%s", containerName, pod.Namespace, pod.Name)
}

func requireContainerAbsent(t *testing.T, pod *corev1.Pod, containerName string) {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		require.NotEqual(t, containerName, c.Name, "container %s should not be present in pod %s/%s", containerName, pod.Namespace, pod.Name)
	}
}

func isGPUMonitoringPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady {
			return cond.Status == corev1.ConditionTrue
		}
	}
	return false
}

func gpumonitoringTerminatedState(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	if pod == nil || len(pod.Status.ContainerStatuses) == 0 {
		return nil
	}
	return pod.Status.ContainerStatuses[0].State.Terminated
}

func containsAll(s string, needles []string) bool {
	for _, needle := range needles {
		if !strings.Contains(s, needle) {
			return false
		}
	}
	return true
}

func envBool(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

func envBoolDefault(name string, defaultValue bool) bool {
	if strings.TrimSpace(os.Getenv(name)) == "" {
		return defaultValue
	}
	return envBool(name)
}

func int32Ptr(v int32) *int32 { return &v }

func int64Ptr(v int64) *int64 { return &v }
