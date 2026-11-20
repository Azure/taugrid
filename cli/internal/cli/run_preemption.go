// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"
)

func validateRenderedPodPreemptionPolicy(ctx context.Context, runner kubeRawRunner, manifest []byte, requested string) error {
	if requested == "" {
		return nil
	}
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(manifest), 4096)
	found := false
	for {
		var workload unstructured.Unstructured
		if err := decoder.Decode(&workload); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("decode workload pod priority: %w", err)
		}
		path := []string{"spec", "template", "spec", "priorityClassName"}
		switch workload.GetKind() {
		case "Job":
		case "RayJob":
			path = []string{"spec", "rayClusterSpec", "headGroupSpec", "template", "spec", "priorityClassName"}
		default:
			continue
		}
		found = true
		class, _, err := unstructured.NestedString(workload.Object, path...)
		if err != nil {
			return fmt.Errorf("read rendered pod priority class: %w", err)
		}
		if err := validatePodPreemptionPolicy(ctx, runner, class, requested); err != nil {
			return err
		}
	}
	if !found {
		return fmt.Errorf("no Job or RayJob found for pod preemption validation")
	}
	return nil
}

func validatePodPreemptionPolicy(ctx context.Context, runner kubeRawRunner, class, requested string) error {
	if requested == "" {
		return nil
	}
	if class == "" {
		return fmt.Errorf("policy.pod_preemption_policy requires a named pod PriorityClass for connected submission")
	}
	output, err := runner.Raw(ctx, []string{"get", "priorityclass", class, "-o", "json"}, nil)
	if err != nil {
		return fmt.Errorf("read pod PriorityClass %q: %w", class, err)
	}
	var priority schedulingv1.PriorityClass
	if err := json.Unmarshal([]byte(output), &priority); err != nil {
		return fmt.Errorf("decode pod PriorityClass %q: %w", class, err)
	}
	if priority.Name != class {
		return fmt.Errorf("pod PriorityClass response names %q, expected %q", priority.Name, class)
	}
	effective := corev1.PreemptLowerPriority
	if priority.PreemptionPolicy != nil {
		effective = *priority.PreemptionPolicy
	}
	if requested != string(effective) {
		return fmt.Errorf("policy.pod_preemption_policy=%q conflicts with PriorityClass %q preemptionPolicy=%q; Kubernetes requires these policies to match", requested, class, effective)
	}
	return nil
}
