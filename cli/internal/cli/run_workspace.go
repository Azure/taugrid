// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"path"
	"strings"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type workspacePlacement struct {
	Workspace    string
	Namespace    string
	LocalQueue   string
	ClusterQueue string
}

func resolveWorkspacePlacement(w tauworkspace.Workspace, connection workspaceconnection.ActiveConnection) (workspacePlacement, error) {
	if !tauworkspace.Ready(w) {
		return workspacePlacement{}, fmt.Errorf("workspace %q is not Ready (phase=%s)", w.Metadata.Name, w.Status.Phase)
	}
	if expected := strings.TrimSpace(connection.Workspace); expected != "" && expected != strings.TrimSpace(w.Metadata.Name) {
		return workspacePlacement{}, fmt.Errorf("active connection workspace %q conflicts with TauWorkspace %q", expected, w.Metadata.Name)
	}
	if expected, actual := strings.TrimSpace(connection.WorkspaceUID), strings.TrimSpace(w.Metadata.UID); expected != "" && actual != expected {
		return workspacePlacement{}, fmt.Errorf(
			"active connection workspace UID %q conflicts with TauWorkspace %q UID %q; reconnect the repository workspace",
			expected,
			w.Metadata.Name,
			actual,
		)
	}

	placement := workspacePlacement{
		Workspace:    strings.TrimSpace(w.Metadata.Name),
		Namespace:    firstNonEmpty(w.Status.Target.ResolvedNamespace, w.Spec.Target.Namespace, w.Metadata.Name),
		LocalQueue:   firstNonEmpty(w.Status.Queue.LocalQueue, w.Spec.Queue),
		ClusterQueue: strings.TrimSpace(w.Status.Queue.ClusterQueue),
	}
	if workspaceConnectionQueueCompatible(w, connection, placement) {
		placement.LocalQueue = strings.TrimSpace(connection.Queue)
	}
	if placement.Namespace == "" {
		return workspacePlacement{}, fmt.Errorf("workspace %q has no resolved target namespace", w.Metadata.Name)
	}
	if placement.LocalQueue == "" {
		return workspacePlacement{}, fmt.Errorf("workspace %q has no resolved LocalQueue", w.Metadata.Name)
	}
	for _, cached := range []struct {
		field  string
		value  string
		actual string
	}{
		{field: "namespace", value: connection.Namespace, actual: placement.Namespace},
		{field: "LocalQueue", value: connection.Queue, actual: placement.LocalQueue},
	} {
		if value := strings.TrimSpace(cached.value); value != "" && value != cached.actual {
			return workspacePlacement{}, fmt.Errorf(
				"active connection %s %q conflicts with TauWorkspace %q %s %q; reconnect the repository workspace",
				cached.field,
				value,
				w.Metadata.Name,
				cached.field,
				cached.actual,
			)
		}
	}
	return placement, nil
}

func workspaceConnectionQueueCompatible(
	w tauworkspace.Workspace,
	connection workspaceconnection.ActiveConnection,
	placement workspacePlacement,
) bool {
	return w.Spec.TeamRef != nil &&
		connection.QueueVerified &&
		strings.TrimSpace(connection.Namespace) == placement.Namespace &&
		strings.TrimSpace(connection.Queue) != ""
}

func applyWorkspaceDefaults(o unresolvedRunOptions, w tauworkspace.Workspace, runName string) (unresolvedRunOptions, error) {
	return applyWorkspaceDefaultsWithConnection(o, w, runName, workspaceconnection.ActiveConnection{})
}

func applyWorkspaceDefaultsWithConnection(
	o unresolvedRunOptions,
	w tauworkspace.Workspace,
	runName string,
	connection workspaceconnection.ActiveConnection,
) (unresolvedRunOptions, error) {
	placement, err := resolveWorkspacePlacement(w, connection)
	if err != nil {
		return o, err
	}
	o.experiment.Workspace = placement.Workspace
	o.workspace = placement.Workspace
	o.workspaceResultScope = w.Spec.Defaults.OutputRoot
	workspaceNamespace := placement.Namespace
	if o.namespace != "" && o.namespace != workspaceNamespace {
		return o, fmt.Errorf("namespace %q conflicts with TauWorkspace %q target namespace %q", o.namespace, w.Metadata.Name, workspaceNamespace)
	}
	o.namespace = workspaceNamespace
	workspaceQueue := placement.LocalQueue
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

func workspaceCanSetOutput(o unresolvedRunOptions) bool {
	return o.file == ""
}

func workspaceHasDurableOutputMount(o unresolvedRunOptions) bool {
	return firstNonEmpty(o.dataPVC, o.resultPVC) != "" || len(o.volumeSpecs) > 0 || len(o.mountSpecs) > 0
}

func workspacePriorityTier(priority string) string {
	switch priority {
	case "normal":
		return ""
	default:
		return priority
	}
}
