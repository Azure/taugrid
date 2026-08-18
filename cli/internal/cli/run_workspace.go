// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"path"
	"strings"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
)

func applyWorkspaceDefaults(o runDispatchOptions, w tauworkspace.Workspace, runName string) (runDispatchOptions, error) {
	if !tauworkspace.Ready(w) {
		return o, fmt.Errorf("workspace %q is not Ready (phase=%s)", w.Metadata.Name, w.Status.Phase)
	}
	o.experiment.Workspace = w.Metadata.Name
	o.workspace = w.Metadata.Name
	o.workspaceResultScope = w.Spec.Defaults.OutputRoot
	workspaceNamespace := firstNonEmpty(w.Status.Target.ResolvedNamespace, w.Spec.Target.Namespace, w.Metadata.Name)
	if o.namespace != "" && o.namespace != workspaceNamespace {
		return o, fmt.Errorf("namespace %q conflicts with TauWorkspace %q target namespace %q", o.namespace, w.Metadata.Name, workspaceNamespace)
	}
	o.namespace = workspaceNamespace
	workspaceQueue := firstNonEmpty(w.Status.Queue.LocalQueue, w.Spec.Queue)
	queueAuto := strings.EqualFold(strings.TrimSpace(o.queue), "auto")
	if o.queue != "" && !queueAuto && o.queue != workspaceQueue {
		return o, fmt.Errorf("queue %q conflicts with TauWorkspace %q LocalQueue %q", o.queue, w.Metadata.Name, workspaceQueue)
	}
	if !queueAuto {
		o.queue = workspaceQueue
		o.workspaceQueueResolved = workspaceQueue != ""
	}
	if o.priorityTier == "" {
		o.priorityTier = workspacePriorityTier(w.Spec.Defaults.Priority)
	}
	// Retention precedence: an explicit run.ttl_seconds_after_finished (which
	// configToDispatch has already applied, and which validates as > 0) wins;
	// otherwise the workspace default applies; otherwise the renderer keeps its
	// own built-in retention. The workspace value is an override, never a
	// floor, so a workspace can neither shorten nor lengthen a run that asked
	// for a specific TTL.
	//
	// The CRD rejects values below the durability floor, but a workspace
	// created against an older schema can still carry one. Ignoring it is the
	// safe reading: a retention shorter than the lifecycle recorder's
	// observation window deletes the Job and its pods before the run can be
	// recorded, so honouring it would silently destroy the evidence the
	// default exists to preserve. Fall back to the built-in retention instead.
	if o.ttlSecondsAfterFinished == 0 && w.Spec.Defaults.TTLSecondsAfterFinished != nil {
		if ttl := *w.Spec.Defaults.TTLSecondsAfterFinished; ttl >= tauworkspace.MinTTLSecondsAfterFinished {
			o.ttlSecondsAfterFinished = ttl
		}
	}
	if w.Spec.WorkloadIdentity != nil && w.Spec.WorkloadIdentity.ServiceAccountName != "" {
		o.azureWorkloadIdentity = true
		if o.serviceAccountName == "" {
			o.serviceAccountName = w.Spec.WorkloadIdentity.ServiceAccountName
		}
	}
	if o.output != "" && w.Spec.Defaults.OutputRoot != "" {
		if err := validateRunOutputScope(o.output, w.Spec.Defaults.OutputRoot); err != nil {
			return o, fmt.Errorf("storage.output %q is outside TauWorkspace %q output root %q", o.output, w.Metadata.Name, w.Spec.Defaults.OutputRoot)
		}
	}
	if o.output == "" && w.Spec.Defaults.OutputRoot != "" && runName != "" && workspaceCanSetOutput(o) && workspaceHasDurableOutputMount(o) {
		o.output = path.Join(w.Spec.Defaults.OutputRoot, runName)
	}
	return o, nil
}

func validateRunOutputScope(output, scope string) error {
	output = path.Clean(strings.TrimSpace(output))
	scope = path.Clean(strings.TrimSpace(scope))
	if output == "." || scope == "." || (output != scope && !strings.HasPrefix(output, scope+"/")) {
		return fmt.Errorf("output path is outside the assigned result scope")
	}
	return nil
}

func workspaceCanSetOutput(o runDispatchOptions) bool {
	return o.file == ""
}

func workspaceHasDurableOutputMount(o runDispatchOptions) bool {
	return firstNonEmpty(o.dataPVC, o.resultPVC) != "" || len(o.volumeSpecs) > 0 || len(o.mountSpecs) > 0
}

func workspacePriorityTier(priority string) string {
	switch priority {
	case "normal":
		return "default"
	default:
		return priority
	}
}
