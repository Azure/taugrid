package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/workloadmeta"
	"k8s.io/apimachinery/pkg/types"
)

type resolvedRunWorkload struct {
	Resource                string
	Kind                    string
	Name                    string
	LogicalName             string
	RunID                   string
	Namespace               string
	UID                     types.UID
	Labels                  map[string]string
	Annotations             map[string]string
	Terminal                bool
	TTLSecondsAfterFinished *int32
}

func (r resolvedRunWorkload) active() bool { return !r.Terminal }

type runWorkloadNotFoundError struct {
	selector  string
	namespace string
}

func (e *runWorkloadNotFoundError) Error() string {
	return fmt.Sprintf("no Tau-managed Job or RayJob matches %q in namespace %q", e.selector, e.namespace)
}

func isRunWorkloadNotFound(err error) bool {
	var target *runWorkloadNotFoundError
	return errors.As(err, &target)
}

func resolveRunWorkload(ctx context.Context, runner kubeRawRunner, namespace, selector string) (resolvedRunWorkload, error) {
	if runner == nil {
		return resolvedRunWorkload{}, fmt.Errorf("resolve run workload: runner is required")
	}
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return resolvedRunWorkload{}, fmt.Errorf("resolve run workload: selector is required")
	}
	var workloads []resolvedRunWorkload
	for _, source := range []struct {
		resource string
		kind     string
	}{
		{resource: "jobs.batch", kind: "Job"},
		{resource: "rayjobs.ray.io", kind: "RayJob"},
	} {
		out, err := runner.Raw(ctx, []string{
			"-n", namespace, "get", source.resource,
			"-l", workloadmeta.LabelManagedBy + "=" + workloadmeta.ManagedByValue,
			"-o", "json",
		}, nil)
		if err != nil {
			if source.kind == "RayJob" && isUnknownResourceError(err) {
				continue
			}
			return resolvedRunWorkload{}, fmt.Errorf("list Tau %s workloads in %s: %w", source.kind, namespace, err)
		}
		parsed, err := parseResolvedRunWorkloads([]byte(out), source.resource, source.kind)
		if err != nil {
			return resolvedRunWorkload{}, err
		}
		workloads = append(workloads, parsed...)
	}

	for _, match := range []func(resolvedRunWorkload) bool{
		func(run resolvedRunWorkload) bool { return run.Name == selector },
		func(run resolvedRunWorkload) bool { return run.RunID == selector },
		func(run resolvedRunWorkload) bool { return run.LogicalName == selector },
	} {
		var matches []resolvedRunWorkload
		for _, run := range workloads {
			if match(run) {
				matches = append(matches, run)
			}
		}
		switch len(matches) {
		case 0:
			continue
		case 1:
			return matches[0], nil
		default:
			sort.Slice(matches, func(i, j int) bool { return matches[i].Name < matches[j].Name })
			candidates := make([]string, 0, len(matches))
			for _, run := range matches {
				candidates = append(candidates, fmt.Sprintf("%s (run-id=%s)", run.Name, run.RunID))
			}
			return resolvedRunWorkload{}, fmt.Errorf(
				"run selector %q is ambiguous in namespace %q; choose an exact physical name or run ID: %s",
				selector, namespace, strings.Join(candidates, ", "),
			)
		}
	}
	return resolvedRunWorkload{}, &runWorkloadNotFoundError{selector: selector, namespace: namespace}
}

func parseResolvedRunWorkloads(data []byte, resource, kind string) ([]resolvedRunWorkload, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name        string            `json:"name"`
				Namespace   string            `json:"namespace"`
				UID         types.UID         `json:"uid"`
				Labels      map[string]string `json:"labels"`
				Annotations map[string]string `json:"annotations"`
			} `json:"metadata"`
			Status struct {
				Active     int `json:"active"`
				Conditions []struct {
					Type   string `json:"type"`
					Status string `json:"status"`
				} `json:"conditions"`
				JobDeploymentStatus string `json:"jobDeploymentStatus"`
				JobStatus           string `json:"jobStatus"`
				EndTime             string `json:"endTime"`
			} `json:"status"`
			Spec struct {
				TTLSecondsAfterFinished *int32 `json:"ttlSecondsAfterFinished"`
			} `json:"spec"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("decode Tau %s workload list: %w", kind, err)
	}
	out := make([]resolvedRunWorkload, 0, len(list.Items))
	for _, item := range list.Items {
		if item.Metadata.Labels[workloadmeta.LabelManagedBy] != workloadmeta.ManagedByValue {
			continue
		}
		runID := strings.TrimSpace(item.Metadata.Labels[workloadmeta.LabelRunID])
		if runID == "" {
			runID = item.Metadata.Name
		}
		logicalName := strings.TrimSpace(item.Metadata.Labels[workloadmeta.LabelRun])
		if logicalName == "" {
			logicalName = item.Metadata.Name
		}
		terminal := false
		if kind == "Job" {
			for _, condition := range item.Status.Conditions {
				if condition.Status == "True" && (condition.Type == "Complete" || condition.Type == "Failed") {
					terminal = true
					break
				}
			}
		} else if isTerminalRayState(item.Status.JobDeploymentStatus) ||
			isTerminalRayState(item.Status.JobStatus) ||
			strings.TrimSpace(item.Status.EndTime) != "" {
			terminal = true
		}
		out = append(out, resolvedRunWorkload{
			Resource:                resource,
			Kind:                    kind,
			Name:                    item.Metadata.Name,
			LogicalName:             logicalName,
			RunID:                   runID,
			Namespace:               item.Metadata.Namespace,
			UID:                     item.Metadata.UID,
			Labels:                  item.Metadata.Labels,
			Annotations:             item.Metadata.Annotations,
			Terminal:                terminal,
			TTLSecondsAfterFinished: item.Spec.TTLSecondsAfterFinished,
		})
	}
	return out, nil
}

func isTerminalRayState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "complete", "completed", "succeeded", "failed", "stopped":
		return true
	default:
		return false
	}
}

func deleteOwnedRunWorkload(
	ctx context.Context,
	runner runSubmissionCleanupRunner,
	run resolvedRunWorkload,
) error {
	if run.Labels[workloadmeta.LabelManagedBy] != workloadmeta.ManagedByValue {
		return fmt.Errorf("refusing to delete %s %s/%s: object is not managed by Tau", run.Kind, run.Namespace, run.Name)
	}

	if run.RunID == "" || run.Labels[workloadmeta.LabelRunID] != run.RunID {
		return fmt.Errorf("refusing to delete %s %s/%s: immutable run ID ownership is missing", run.Kind, run.Namespace, run.Name)
	}
	if run.UID == "" {
		return fmt.Errorf("refusing to delete %s %s/%s: object UID is missing", run.Kind, run.Namespace, run.Name)
	}
	if run.Kind == "Job" {
		service := runSubmission{Resource: "service", Name: run.Name + "-headless", Namespace: run.Namespace}
		metadata, err := existingRunMetadata(ctx, runner, service)
		if err == nil {
			if metadata.Labels[workloadmeta.LabelManagedBy] != workloadmeta.ManagedByValue ||
				metadata.Labels[workloadmeta.LabelRunID] != run.RunID {
				return fmt.Errorf("refusing to delete Service %s/%s: ownership labels do not match run %s", run.Namespace, service.Name, run.RunID)
			}
			if metadata.UID == "" {
				return fmt.Errorf("refusing to delete Service %s/%s: object UID is missing", run.Namespace, service.Name)
			}
			if err := runner.DeleteWithUID(ctx, service, metadata.UID); err != nil {
				return fmt.Errorf("delete owned Service %s/%s: %w", run.Namespace, service.Name, err)
			}
		} else if !isExactObjectNotFound(err, service.Name, "service", "services") {
			return fmt.Errorf("verify Service %s/%s ownership: %w", run.Namespace, service.Name, err)
		}
	}
	return runner.DeleteWithUID(ctx, runSubmission{
		Resource:  run.Resource,
		Name:      run.Name,
		Namespace: run.Namespace,
	}, run.UID)
}

func deleteOwnedRunRayClusters(
	ctx context.Context,
	runner runSubmissionCleanupRunner,
	namespace, physicalName string,
) ([]string, error) {
	out, err := runner.Raw(ctx, []string{
		"-n", namespace, "get", "rayclusters.ray.io",
		"-l", rayOriginLabel + "=" + physicalName,
		"-o", "json",
	}, nil)
	if err != nil {
		if isMissingRayCRD(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list RayClusters owned by %s/%s: %w", namespace, physicalName, err)
	}
	var list struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				UID    types.UID         `json:"uid"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("decode RayClusters owned by %s/%s: %w", namespace, physicalName, err)
	}
	deleted := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		labels := item.Metadata.Labels
		runID := labels[workloadmeta.LabelRunID]
		if labels[rayOriginLabel] != physicalName ||
			labels[workloadmeta.LabelManagedBy] != workloadmeta.ManagedByValue ||
			runID == "" ||
			!strings.HasSuffix(physicalName, "-"+runID) {
			return deleted, fmt.Errorf(
				"refusing to delete RayCluster %s/%s: immutable Tau ownership does not match run %s",
				namespace, item.Metadata.Name, physicalName,
			)
		}
		if item.Metadata.UID == "" {
			return deleted, fmt.Errorf("refusing to delete RayCluster %s/%s: object UID is missing", namespace, item.Metadata.Name)
		}
		if err := runner.DeleteWithUID(ctx, runSubmission{
			Resource:  "raycluster.ray.io",
			Name:      item.Metadata.Name,
			Namespace: namespace,
		}, item.Metadata.UID); err != nil {
			return deleted, fmt.Errorf("delete owned RayCluster %s/%s: %w", namespace, item.Metadata.Name, err)
		}
		deleted = append(deleted, item.Metadata.Name)
	}
	return deleted, nil
}
