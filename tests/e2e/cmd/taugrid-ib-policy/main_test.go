// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func configLog(t *testing.T, resources string) []byte {
	t.Helper()
	record, err := json.Marshal(map[string]string{
		"msg": "Configuration loaded",
		"config": "apiVersion: config.kueue.x-k8s.io/v1beta2\nkind: Configuration\n" +
			"resources:\n" + resources,
	})
	require.NoError(t, err)
	return append(record, '\n')
}

func TestAccountingPolicy(t *testing.T) {
	for _, test := range []struct {
		name      string
		resources string
		gates     string
		declared  bool
		want      string
		errorText string
	}{
		{name: "excluded", resources: "  excludeResourcePrefixes: [rdma/]\n", want: "excludeResourcePrefixes"},
		{name: "excluded-declared", resources: "  excludeResourcePrefixes: [rdma/]\n", declared: true, want: "excludeResourcePrefixes"},
		{name: "unexcluded", resources: "  deviceClassMappings: []\n", errorText: "RDMA remains Kueue-accounted"},
		{name: "wrong-prefix", resources: "  excludeResourcePrefixes: [other/]\n", errorText: "RDMA remains Kueue-accounted"},
		{name: "misspelled-field", resources: "  excludedResourcePrefixes: [rdma/]\n", errorText: "RDMA remains Kueue-accounted"},
		{name: "ignore-undeclared", resources: "  quotaCheckStrategy: IgnoreUndeclared\n",
			gates: `{"msg":"Loaded feature gates","featureGates":{"QuotaCheckStrategy":true}}`, want: "IgnoreUndeclared"},
		{name: "ignore-declared", resources: "  quotaCheckStrategy: IgnoreUndeclared\n", declared: true,
			gates: `{"msg":"Loaded feature gates","featureGates":{"QuotaCheckStrategy":true}}`, errorText: "RDMA remains Kueue-accounted"},
		{name: "ignore-disabled", resources: "  quotaCheckStrategy: IgnoreUndeclared\n",
			gates: `{"msg":"Loaded feature gates","featureGates":{"QuotaCheckStrategy":false}}`, errorText: "RDMA remains Kueue-accounted"},
		{name: "ignore-gate-unverified", resources: "  quotaCheckStrategy: IgnoreUndeclared\n", errorText: "feature gate is unavailable"},
	} {
		t.Run(test.name, func(t *testing.T) {
			logs := append(configLog(t, test.resources), []byte(test.gates+"\n")...)
			policy, err := accountingPolicy(logs, test.declared)
			if test.errorText != "" {
				require.ErrorContains(t, err, test.errorText)
			} else {
				require.NoError(t, err)
				require.Equal(t, test.want, policy)
			}
		})
	}
}

func TestMissingEffectiveConfiguration(t *testing.T) {
	for _, logs := range [][]byte{
		nil,
		[]byte(`{"msg":"controller ready"}`),
		[]byte(`{"msg":"Configuration loaded","config":"["}`),
		[]byte(`{"msg":"Configuration loaded","config":""}`),
		append(configLog(t, "  excludeResourcePrefixes: [rdma/]\n"), configLog(t, "  deviceClassMappings: []\n")...),
	} {
		_, err := accountingPolicy(logs, false)
		require.Error(t, err)
	}
}

func TestVerifyCurrentProcesses(t *testing.T) {
	directory := t.TempDir()
	write := func(name string, object any) {
		data, err := json.Marshal(object)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(filepath.Join(directory, name), data, 0600))
	}
	replicas := int32(1)
	deployment := appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3, UpdatedReplicas: 1, AvailableReplicas: 1,
		},
	}
	pods := corev1.PodList{Items: []corev1.Pod{{
		ObjectMeta: metav1.ObjectMeta{Name: "controller-pod", UID: "original-pod"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "manager", Ready: true, ContainerID: "containerd://original",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}}}
	write("ib-kueue-deployment.json", deployment)
	write("ib-kueue-pods.json", pods)
	write("ib-kueue-pods-after.json", pods)
	write("ib-clusterqueue.json", map[string]any{"spec": map[string]any{"resourceGroups": []any{}}})
	logPath := filepath.Join(directory, "controller-pod.log")
	require.NoError(t, os.WriteFile(logPath, configLog(t, "  excludeResourcePrefixes: [rdma/]\n"), 0600))
	result, err := verifyDirectory(directory)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"controller-pod": "excludeResourcePrefixes"}, result)

	after := pods.DeepCopy()
	after.Items[0].Status.ContainerStatuses[0].RestartCount++
	write("ib-kueue-pods-after.json", after)
	_, err = verifyDirectory(directory)
	require.ErrorContains(t, err, "processes changed")
	write("ib-kueue-pods-after.json", pods)
	require.NoError(t, os.Remove(logPath))
	_, err = verifyDirectory(directory)
	require.ErrorContains(t, err, "startup logs")

	deployment.Status.UpdatedReplicas = 0
	write("ib-kueue-deployment.json", deployment)
	_, err = verifyDirectory(directory)
	require.ErrorContains(t, err, "rollout is not stable")
}
