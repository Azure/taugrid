// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

const rdmaResource = "rdma/rdma_shared_device_a"
const requiredRDMAUnits int64 = 16

type configuration struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Resources  struct {
		ExcludeResourcePrefixes []string `json:"excludeResourcePrefixes"`
		QuotaCheckStrategy      string   `json:"quotaCheckStrategy"`
	} `json:"resources"`
}

type quotaObject struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		CohortName     string `json:"cohortName"`
		ParentName     string `json:"parentName"`
		ResourceGroups []struct {
			CoveredResources []string `json:"coveredResources"`
			Flavors          []struct {
				Name      string `json:"name"`
				Resources []struct {
					Name           string             `json:"name"`
					NominalQuota   *resource.Quantity `json:"nominalQuota"`
					BorrowingLimit *resource.Quantity `json:"borrowingLimit"`
				} `json:"resources"`
			} `json:"flavors"`
		} `json:"resourceGroups"`
	} `json:"spec"`
}

func (object quotaObject) rdmaQuota(flavor string) (int64, *resource.Quantity, error) {
	var nominal int64
	var borrowing *resource.Quantity
	found := false
	for _, group := range object.Spec.ResourceGroups {
		covered := false
		for _, name := range group.CoveredResources {
			covered = covered || name == rdmaResource
		}
		if !covered {
			continue
		}
		for _, candidate := range group.Flavors {
			if candidate.Name != flavor {
				continue
			}
			for _, quota := range candidate.Resources {
				if quota.Name != rdmaResource {
					continue
				}
				if found || quota.NominalQuota == nil {
					return 0, nil, fmt.Errorf("missing or duplicate RDMA nominal quota for flavor %q", flavor)
				}
				value, exact := quota.NominalQuota.AsInt64()
				if !exact || value < 0 {
					return 0, nil, fmt.Errorf("RDMA nominal quota must be a nonnegative integer for flavor %q", flavor)
				}
				if quota.BorrowingLimit != nil {
					limit, exact := quota.BorrowingLimit.AsInt64()
					if !exact || limit < 0 {
						return 0, nil, fmt.Errorf("RDMA borrowing limit must be a nonnegative integer for flavor %q", flavor)
					}
				}
				nominal, borrowing, found = value, quota.BorrowingLimit, true
			}
		}
	}
	if !found {
		return 0, nil, fmt.Errorf("missing declared %s quota for target flavor %q", rdmaResource, flavor)
	}
	return nominal, borrowing, nil
}

func verifyQuota(directory string, queue quotaObject) error {
	var target struct {
		Flavor       string `json:"flavor"`
		ClusterQueue string `json:"cluster_queue"`
	}
	data, err := os.ReadFile(filepath.Join(directory, "ib-target.json"))
	if err != nil {
		return fmt.Errorf("read hardware target for RDMA quota verification: %w", err)
	}
	if err := json.Unmarshal(data, &target); err != nil {
		return fmt.Errorf("decode hardware target: %w", err)
	}
	if target.Flavor == "" || target.ClusterQueue == "" || queue.Metadata.Name != target.ClusterQueue {
		return fmt.Errorf("RDMA quota evidence does not match selected hardware target and ClusterQueue")
	}
	nominal, borrowing, err := queue.rdmaQuota(target.Flavor)
	if err != nil {
		return err
	}
	if nominal >= requiredRDMAUnits {
		return nil
	}
	needed := requiredRDMAUnits - nominal
	if queue.Spec.CohortName == "" || borrowing != nil && borrowing.CmpInt64(needed) < 0 {
		return fmt.Errorf("selected ClusterQueue lacks declared RDMA capacity for 16 workers")
	}
	data, err = os.ReadFile(filepath.Join(directory, "ib-cohort.json"))
	if err != nil {
		return fmt.Errorf("read direct Cohort RDMA capacity with existing read access: %w", err)
	}
	var cohort quotaObject
	if err := json.Unmarshal(data, &cohort); err != nil {
		return fmt.Errorf("decode direct Cohort RDMA capacity: %w", err)
	}
	if cohort.Metadata.Name != queue.Spec.CohortName || cohort.Spec.ParentName != "" {
		return fmt.Errorf("RDMA borrowing requires the named direct root Cohort; hierarchical borrowing is not supported by this preflight")
	}
	capacity, _, err := cohort.rdmaQuota(target.Flavor)
	if err != nil {
		return fmt.Errorf("direct Cohort: %w", err)
	}
	if capacity < needed {
		return fmt.Errorf("direct Cohort lacks declared RDMA capacity for 16 workers; sibling or ancestor quota is not inferred")
	}
	return nil
}

func accountingPolicy(logs []byte, declared bool) (string, error) {
	var config *configuration
	var strategyEnabled *bool
	scanner := bufio.NewScanner(strings.NewReader(string(logs)))
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	for scanner.Scan() {
		var record struct {
			Message      string          `json:"msg"`
			Config       string          `json:"config"`
			FeatureGates map[string]bool `json:"featureGates"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			if strings.Contains(scanner.Text(), "Configuration loaded") {
				return "", fmt.Errorf("decode effective Kueue configuration record: %w", err)
			}
			continue
		}
		switch record.Message {
		case "Configuration loaded":
			if config != nil {
				return "", fmt.Errorf("multiple effective Kueue configuration records")
			}
			config = &configuration{}
			if err := yaml.Unmarshal([]byte(record.Config), config); err != nil {
				return "", fmt.Errorf("decode loaded Kueue configuration YAML: %w", err)
			}
			if config.Kind != "Configuration" || config.APIVersion != "config.kueue.x-k8s.io/v1beta2" {
				return "", fmt.Errorf("missing or unsupported loaded Kueue configuration type")
			}
		case "Loaded feature gates":
			if enabled, found := record.FeatureGates["QuotaCheckStrategy"]; found {
				if strategyEnabled != nil {
					return "", fmt.Errorf("multiple effective QuotaCheckStrategy feature-gate records")
				}
				strategyEnabled = &enabled
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read Kueue startup logs: %w", err)
	}
	if config == nil {
		return "", fmt.Errorf("effective Kueue Configuration loaded record is unavailable; refusing to infer policy from a mutable ConfigMap")
	}
	for _, prefix := range config.Resources.ExcludeResourcePrefixes {
		if prefix != "" && strings.HasPrefix(rdmaResource, prefix) {
			return "excludeResourcePrefixes", nil
		}
	}
	switch config.Resources.QuotaCheckStrategy {
	case "IgnoreUndeclared":
		if !declared && strategyEnabled == nil {
			return "", fmt.Errorf("effective QuotaCheckStrategy feature gate is unavailable in current controller logs")
		}
		if !declared && *strategyEnabled {
			return "IgnoreUndeclared", nil
		}
	case "", "BlockUndeclared":
	default:
		return "", fmt.Errorf("unsupported Kueue quotaCheckStrategy %q", config.Resources.QuotaCheckStrategy)
	}
	if declared {
		return "DeclaredQuota", nil
	}
	return "", fmt.Errorf("RDMA remains Kueue-accounted without declared queue coverage; configure RDMA quota or a verified resource-exclusion policy")
}

func verifyDirectory(directory string) (map[string]string, error) {
	readJSON := func(name string, target any) error {
		data, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			return err
		}
		return json.Unmarshal(data, target)
	}
	var deployment appsv1.Deployment
	if err := readJSON("ib-kueue-deployment.json", &deployment); err != nil {
		return nil, fmt.Errorf("read Kueue Deployment: %w", err)
	}
	var pods corev1.PodList
	if err := readJSON("ib-kueue-pods.json", &pods); err != nil {
		return nil, fmt.Errorf("read current Kueue Pods: %w", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas < 1 ||
		deployment.Status.ObservedGeneration != deployment.Generation ||
		deployment.Status.UpdatedReplicas != *deployment.Spec.Replicas ||
		deployment.Status.AvailableReplicas != *deployment.Spec.Replicas ||
		len(pods.Items) != int(*deployment.Spec.Replicas) {
		return nil, fmt.Errorf("Kueue rollout is not stable; effective accounting policy cannot be verified")
	}
	var after corev1.PodList
	if err := readJSON("ib-kueue-pods-after.json", &after); err != nil {
		return nil, fmt.Errorf("read Kueue process identity recheck: %w", err)
	}
	processes := func(list corev1.PodList) map[string]string {
		identities := make(map[string]string, len(list.Items))
		for _, pod := range list.Items {
			for _, status := range pod.Status.ContainerStatuses {
				if status.Name == "manager" && status.Ready && status.State.Running != nil &&
					pod.DeletionTimestamp == nil && pod.UID != "" && status.ContainerID != "" {
					identities[pod.Name] = fmt.Sprintf("%s/%s/%d/%s", pod.UID, status.ContainerID,
						status.RestartCount, status.State.Running.StartedAt.String())
				}
			}
		}
		return identities
	}
	beforeIdentities := processes(pods)
	afterIdentities := processes(after)
	if len(beforeIdentities) != len(pods.Items) || len(after.Items) != len(pods.Items) ||
		!reflect.DeepEqual(beforeIdentities, afterIdentities) {
		return nil, fmt.Errorf("Kueue manager processes changed or became unready during effective configuration capture")
	}
	var queue quotaObject
	if err := readJSON("ib-clusterqueue.json", &queue); err != nil {
		return nil, fmt.Errorf("read selected ClusterQueue: %w", err)
	}
	declared := false
	for _, group := range queue.Spec.ResourceGroups {
		for _, resource := range group.CoveredResources {
			declared = declared || resource == rdmaResource
		}
	}
	result := make(map[string]string, len(pods.Items))
	for _, pod := range pods.Items {
		logs, err := os.ReadFile(filepath.Join(directory, pod.Name+".log"))
		if err != nil {
			return nil, fmt.Errorf("read current manager startup logs: %w", err)
		}
		policy, err := accountingPolicy(logs, declared)
		if err != nil {
			return nil, fmt.Errorf("Kueue Pod %q: %w", pod.Name, err)
		}
		if policy == "DeclaredQuota" {
			if err := verifyQuota(directory, queue); err != nil {
				return nil, fmt.Errorf("verify declared RDMA queue capacity: %w", err)
			}
		}
		result[pod.Name] = policy
	}
	return result, nil
}

func main() {
	directory := flag.String("directory", "", "directory containing current controller and queue evidence")
	flag.Parse()
	result, err := verifyDirectory(*directory)
	if err != nil {
		fmt.Fprintf(os.Stderr, "IB accounting policy verification failed: %v\n", err)
		os.Exit(1)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintf(os.Stderr, "write IB accounting policy evidence: %v\n", err)
		os.Exit(1)
	}
}
