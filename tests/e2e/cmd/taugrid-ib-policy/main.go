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
	"sigs.k8s.io/yaml"
)

const rdmaResource = "rdma/rdma_shared_device_a"

type configuration struct {
	Kind       string `json:"kind"`
	APIVersion string `json:"apiVersion"`
	Resources  struct {
		ExcludeResourcePrefixes []string `json:"excludeResourcePrefixes"`
		QuotaCheckStrategy      string   `json:"quotaCheckStrategy"`
	} `json:"resources"`
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
		if strategyEnabled == nil {
			return "", fmt.Errorf("effective QuotaCheckStrategy feature gate is unavailable in current controller logs")
		}
		if *strategyEnabled && !declared {
			return "IgnoreUndeclared", nil
		}
	case "", "BlockUndeclared":
	default:
		return "", fmt.Errorf("unsupported Kueue quotaCheckStrategy %q", config.Resources.QuotaCheckStrategy)
	}
	return "", fmt.Errorf("RDMA remains Kueue-accounted; verify the intended managed resource-exclusion policy, not an RDMA quota workaround")
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
	var queue struct {
		Spec struct {
			ResourceGroups []struct {
				CoveredResources []string `json:"coveredResources"`
			} `json:"resourceGroups"`
		} `json:"spec"`
	}
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
