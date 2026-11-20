// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
)

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
