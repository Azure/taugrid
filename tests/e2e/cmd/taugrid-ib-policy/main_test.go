// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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
		{name: "accounted-declared", resources: "  deviceClassMappings: []\n", declared: true, want: "DeclaredQuota"},
		{name: "block-declared", resources: "  quotaCheckStrategy: BlockUndeclared\n", declared: true, want: "DeclaredQuota"},
		{name: "unexcluded", resources: "  deviceClassMappings: []\n", errorText: "RDMA remains Kueue-accounted"},
		{name: "wrong-prefix", resources: "  excludeResourcePrefixes: [other/]\n", errorText: "RDMA remains Kueue-accounted"},
		{name: "misspelled-field", resources: "  excludedResourcePrefixes: [rdma/]\n", errorText: "RDMA remains Kueue-accounted"},
		{name: "ignore-undeclared", resources: "  quotaCheckStrategy: IgnoreUndeclared\n",
			gates: `{"msg":"Loaded feature gates","featureGates":{"QuotaCheckStrategy":true}}`, want: "IgnoreUndeclared"},
		{name: "ignore-declared", resources: "  quotaCheckStrategy: IgnoreUndeclared\n", declared: true,
			gates: `{"msg":"Loaded feature gates","featureGates":{"QuotaCheckStrategy":true}}`, want: "DeclaredQuota"},
		{name: "ignore-declared-no-gate-record", resources: "  quotaCheckStrategy: IgnoreUndeclared\n", declared: true, want: "DeclaredQuota"},
		{name: "ignore-declared-gate-disabled", resources: "  quotaCheckStrategy: IgnoreUndeclared\n", declared: true,
			gates: `{"msg":"Loaded feature gates","featureGates":{"QuotaCheckStrategy":false}}`, want: "DeclaredQuota"},
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

func TestDeclaredQuota(t *testing.T) {
	makeQuota := func(t *testing.T, name, cohort, nominal, borrowing string) quotaObject {
		t.Helper()
		object := `{"metadata":{"name":"` + name + `"},"spec":{"cohortName":"` + cohort + `","resourceGroups":[{
			"coveredResources":["rdma/rdma_shared_device_a"],"flavors":[{"name":"target-flavor","resources":[{
			"name":"rdma/rdma_shared_device_a","nominalQuota":` + nominal + `,"borrowingLimit":` + borrowing + `}]}]}]}}`
		var quota quotaObject
		require.NoError(t, json.Unmarshal([]byte(object), &quota))
		return quota
	}
	for _, test := range []struct {
		name        string
		nominal     string
		borrowing   string
		cohortQuota string
		mutate      func(*quotaObject, *quotaObject)
		want        string
	}{
		{name: "own-capacity", nominal: `"16"`, borrowing: `"0"`},
		{name: "bounded-direct-cohort", nominal: `"0"`, borrowing: `"16"`, cohortQuota: `"16"`},
		{name: "combined-capacity", nominal: `"8"`, borrowing: `"8"`, cohortQuota: `"8"`},
		{name: "unlimited-borrowing", nominal: `"0"`, borrowing: `null`, cohortQuota: `"16"`},
		{name: "too-small-borrowing", nominal: `"0"`, borrowing: `"15"`, cohortQuota: `"16"`, want: "lacks declared RDMA capacity"},
		{name: "zero-borrowing", nominal: `"0"`, borrowing: `"0"`, cohortQuota: `"16"`, want: "lacks declared RDMA capacity"},
		{name: "insufficient-cohort", nominal: `"0"`, borrowing: `"16"`, cohortQuota: `"15"`, want: "direct Cohort lacks"},
		{name: "zero-cohort", nominal: `"0"`, borrowing: `"16"`, cohortQuota: `"0"`, want: "direct Cohort lacks"},
		{name: "missing-cohort", nominal: `"0"`, borrowing: `"16"`, want: "read direct Cohort"},
		{name: "no-cohort-reference", nominal: `"0"`, borrowing: `"16"`, want: "lacks declared RDMA capacity",
			mutate: func(q, _ *quotaObject) { q.Spec.CohortName = "" }},
		{name: "missing-nominal", nominal: `null`, borrowing: `"16"`, want: "missing or duplicate"},
		{name: "negative-nominal", nominal: `"-1"`, borrowing: `"16"`, want: "nonnegative integer"},
		{name: "fractional-nominal", nominal: `"15500m"`, borrowing: `"16"`, want: "nonnegative integer"},
		{name: "negative-borrowing", nominal: `"0"`, borrowing: `"-1"`, want: "nonnegative integer"},
		{name: "fractional-borrowing", nominal: `"0"`, borrowing: `"16500m"`, want: "nonnegative integer"},
		{name: "wrong-resource", nominal: `"16"`, borrowing: `"0"`, want: "missing declared",
			mutate: func(q, _ *quotaObject) { q.Spec.ResourceGroups[0].Flavors[0].Resources[0].Name = "other/device" }},
		{name: "wrong-coverage", nominal: `"16"`, borrowing: `"0"`, want: "missing declared",
			mutate: func(q, _ *quotaObject) { q.Spec.ResourceGroups[0].CoveredResources = []string{"other/device"} }},
		{name: "wrong-flavor", nominal: `"16"`, borrowing: `"0"`, want: "missing declared",
			mutate: func(q, _ *quotaObject) { q.Spec.ResourceGroups[0].Flavors[0].Name = "other-flavor" }},
		{name: "duplicate-quota", nominal: `"16"`, borrowing: `"0"`, want: "missing or duplicate",
			mutate: func(q, _ *quotaObject) {
				q.Spec.ResourceGroups = append(q.Spec.ResourceGroups, q.Spec.ResourceGroups[0])
			}},
		{name: "wrong-cohort", nominal: `"0"`, borrowing: `"16"`, cohortQuota: `"16"`, want: "named direct root",
			mutate: func(_, c *quotaObject) { c.Metadata.Name = "other-cohort" }},
		{name: "ancestor-capacity", nominal: `"0"`, borrowing: `"16"`, cohortQuota: `"16"`, want: "hierarchical borrowing",
			mutate: func(_, c *quotaObject) { c.Spec.ParentName = "ancestor" }},
		{name: "cohort-wrong-flavor", nominal: `"0"`, borrowing: `"16"`, cohortQuota: `"16"`, want: "direct Cohort: missing declared",
			mutate: func(_, c *quotaObject) { c.Spec.ResourceGroups[0].Flavors[0].Name = "other-flavor" }},
		{name: "wrong-queue", nominal: `"16"`, borrowing: `"0"`, want: "does not match",
			mutate: func(q, _ *quotaObject) { q.Metadata.Name = "other-queue" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			queue := makeQuota(t, "selected-queue", "direct-cohort", test.nominal, test.borrowing)
			cohort := makeQuota(t, "direct-cohort", "", `"0"`, `null`)
			if test.cohortQuota != "" {
				cohort = makeQuota(t, "direct-cohort", "", test.cohortQuota, `null`)
			}
			if test.mutate != nil {
				test.mutate(&queue, &cohort)
			}
			require.NoError(t, os.WriteFile(filepath.Join(directory, "ib-target.json"),
				[]byte(`{"flavor":"target-flavor","cluster_queue":"selected-queue"}`), 0600))
			if test.cohortQuota != "" {
				data, err := json.Marshal(cohort)
				require.NoError(t, err)
				require.NoError(t, os.WriteFile(filepath.Join(directory, "ib-cohort.json"), data, 0600))
			}
			err := verifyQuota(directory, queue)
			if test.want != "" {
				require.ErrorContains(t, err, test.want)
			} else {
				require.NoError(t, err)
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

	queue := `{"metadata":{"name":"selected-queue"},"spec":{"resourceGroups":[{
		"coveredResources":["rdma/rdma_shared_device_a"],"flavors":[{"name":"target-flavor","resources":[{
		"name":"rdma/rdma_shared_device_a","nominalQuota":"16"}]}]}]}}`
	require.NoError(t, os.WriteFile(filepath.Join(directory, "ib-clusterqueue.json"), []byte(queue), 0600))
	require.NoError(t, os.WriteFile(logPath, configLog(t, "  deviceClassMappings: []\n"), 0600))
	_, err = verifyDirectory(directory)
	require.ErrorContains(t, err, "read hardware target")
	write("ib-target.json", map[string]string{"flavor": "target-flavor", "cluster_queue": "selected-queue"})
	result, err = verifyDirectory(directory)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"controller-pod": "DeclaredQuota"}, result)
	require.NoError(t, os.WriteFile(logPath, nil, 0600))
	_, err = verifyDirectory(directory)
	require.ErrorContains(t, err, "Configuration loaded record is unavailable")
	require.NoError(t, os.WriteFile(logPath, configLog(t, "  deviceClassMappings: []\n"), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(directory, "ib-clusterqueue.json"),
		[]byte(strings.ReplaceAll(queue, `"nominalQuota":"16"`, `"nominalQuota":"invalid"`)), 0600))
	_, err = verifyDirectory(directory)
	require.ErrorContains(t, err, "read selected ClusterQueue")
	require.NoError(t, os.WriteFile(filepath.Join(directory, "ib-clusterqueue.json"), []byte(queue), 0600))

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
