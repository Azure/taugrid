// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspaceconnection

import (
	"context"
	"fmt"
	"strings"

	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/core/kube"
)

type rawRunner interface {
	Raw(context.Context, []string, []byte) (string, error)
}

type KubectlVerifier struct {
	KubectlPath string
	NewRunner   func(contextName, kubeconfigPath string) rawRunner
}

func (v KubectlVerifier) Verify(ctx context.Context, descriptor Descriptor, kubeconfigPath string) (Verification, error) {
	runner := v.runner(descriptor.Cluster.ContextName, kubeconfigPath)
	raw, err := runner.Raw(ctx, []string{
		"-n", descriptor.ResolvedSystemNamespace(),
		"get", "workspace.tau.azure.com", descriptor.Workspace,
		"-o", "json",
	}, nil)
	if err != nil {
		return Verification{}, fmt.Errorf("read TauWorkspace %s: %w", descriptor.Workspace, err)
	}
	workspace, err := tauworkspace.Parse([]byte(raw))
	if err != nil {
		return Verification{}, err
	}
	namespace := tauworkspace.ResolvedNamespace(workspace)
	if namespace == "" {
		return Verification{}, fmt.Errorf("TauWorkspace %q has no resolved target namespace", descriptor.Workspace)
	}
	queue := tauworkspace.ResolvedLocalQueue(workspace)
	if queue == "" {
		return Verification{}, fmt.Errorf("TauWorkspace %q has no resolved LocalQueue", descriptor.Workspace)
	}
	if !tauworkspace.Ready(workspace) {
		return Verification{}, fmt.Errorf(
			"TauWorkspace %q is not Ready (phase=%s, observedGeneration=%d, generation=%d)",
			descriptor.Workspace,
			workspace.Status.Phase,
			workspace.Status.ObservedGeneration,
			workspace.Metadata.Generation,
		)
	}
	workspaceAuthorizationMode := tauworkspace.AuthorizationModeWorkspaceRBAC
	if workspace.Spec.Authorization != nil && strings.TrimSpace(workspace.Spec.Authorization.Mode) != "" {
		workspaceAuthorizationMode = strings.TrimSpace(workspace.Spec.Authorization.Mode)
	}
	if workspaceAuthorizationMode != descriptor.Authorization.Mode {
		return Verification{}, fmt.Errorf(
			"workspace connection authorization mode %q does not match TauWorkspace %q mode %q",
			descriptor.Authorization.Mode,
			descriptor.Workspace,
			workspaceAuthorizationMode,
		)
	}
	if workspaceAuthorizationMode == AuthorizationModeWorkspaceRBAC &&
		strings.TrimSpace(workspace.Spec.Role) != descriptor.Authorization.RequiredRole {
		return Verification{}, fmt.Errorf(
			"workspace connection required role %q does not match TauWorkspace %q role %q",
			descriptor.Authorization.RequiredRole,
			descriptor.Workspace,
			strings.TrimSpace(workspace.Spec.Role),
		)
	}
	serviceAccount := tauworkspace.EffectiveServiceAccount(workspace)
	if _, err := runner.Raw(ctx, []string{"-n", namespace, "get", "localqueue.kueue.x-k8s.io", queue, "-o", "name"}, nil); err != nil {
		return Verification{}, fmt.Errorf("read workspace LocalQueue %s/%s: %w", namespace, queue, err)
	}
	if descriptor.Authorization.Mode == AuthorizationModeClusterWide {
		output, err := runner.Raw(ctx, []string{"auth", "can-i", "*", "*", "--all-namespaces"}, nil)
		if err != nil {
			return Verification{}, fmt.Errorf("verify cluster-wide access: %w", err)
		}
		if strings.TrimSpace(output) != "yes" {
			return Verification{}, fmt.Errorf(
				"workspace %q requests cluster-wide access, but the Kubernetes credential is not authorized across the cluster",
				descriptor.Workspace,
			)
		}
		return Verification{
			ContextName:    descriptor.Cluster.ContextName,
			Namespace:      namespace,
			Queue:          queue,
			ServiceAccount: serviceAccount,
			WorkspaceUID:   workspace.Metadata.UID,
			WorkspacePhase: workspace.Status.Phase,
		}, nil
	}
	checks := []struct {
		verb      string
		resource  string
		namespace string
	}{
		{verb: "create", resource: "jobs.batch", namespace: namespace},
		{verb: "get", resource: "jobs.batch", namespace: namespace},
		{verb: "list", resource: "jobs.batch", namespace: namespace},
		{verb: "patch", resource: "jobs.batch", namespace: namespace},
		{verb: "delete", resource: "jobs.batch", namespace: namespace},
		{verb: "create", resource: "rayjobs.ray.io", namespace: namespace},
		{verb: "get", resource: "rayjobs.ray.io", namespace: namespace},
		{verb: "list", resource: "rayjobs.ray.io", namespace: namespace},
		{verb: "patch", resource: "rayjobs.ray.io", namespace: namespace},
		{verb: "delete", resource: "rayjobs.ray.io", namespace: namespace},
		{verb: "get", resource: "pods", namespace: namespace},
		{verb: "list", resource: "pods", namespace: namespace},
		{verb: "get", resource: "pods/log", namespace: namespace},
		{verb: "get", resource: "localqueues.kueue.x-k8s.io", namespace: namespace},
		{verb: "get", resource: "workloads.kueue.x-k8s.io", namespace: namespace},
		{verb: "list", resource: "workloads.kueue.x-k8s.io", namespace: namespace},
	}
	for _, check := range checks {
		output, err := runner.Raw(ctx, []string{
			"-n", check.namespace,
			"auth", "can-i", check.verb, check.resource,
		}, nil)
		if err != nil {
			return Verification{}, fmt.Errorf("verify permission %s %s in %s: %w", check.verb, check.resource, check.namespace, err)
		}
		if strings.TrimSpace(output) != "yes" {
			return Verification{}, fmt.Errorf(
				"workspace %q is missing required permission: %s %s in namespace %s (expected role %s)",
				descriptor.Workspace,
				check.verb,
				check.resource,
				check.namespace,
				descriptor.Authorization.RequiredRole,
			)
		}
	}
	return Verification{
		ContextName:    descriptor.Cluster.ContextName,
		Namespace:      namespace,
		Queue:          queue,
		ServiceAccount: serviceAccount,
		WorkspaceUID:   workspace.Metadata.UID,
		WorkspacePhase: workspace.Status.Phase,
	}, nil
}

func (v KubectlVerifier) runner(contextName, kubeconfigPath string) rawRunner {
	if v.NewRunner != nil {
		return v.NewRunner(contextName, kubeconfigPath)
	}
	runner := kube.NewWithKubeconfig(contextName, kubeconfigPath)
	runner.Path = v.KubectlPath
	return runner
}
