// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspaceconnection

import (
	"context"
	"fmt"
	"strconv"
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
		"-n", tauworkspace.PlatformNamespace,
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
	namespace := firstNonEmpty(workspace.Status.Target.ResolvedNamespace, workspace.Spec.Target.Namespace)
	if namespace == "" {
		return Verification{}, fmt.Errorf("TauWorkspace %q has no resolved target namespace", descriptor.Workspace)
	}
	queue := firstNonEmpty(workspace.Status.Queue.LocalQueue, workspace.Spec.Queue)
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
	serviceAccount := ""
	if workspace.Spec.WorkloadIdentity != nil {
		serviceAccount = workspace.Spec.WorkloadIdentity.ServiceAccountName
	}
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
				"workspace %q requests cluster-wide access, but the AKS cluster-user credential is not authorized across the cluster",
				descriptor.Workspace,
			)
		}
		return Verification{
			ContextName:       descriptor.Cluster.ContextName,
			Namespace:         namespace,
			Queue:             queue,
			ServiceAccount:    serviceAccount,
			WorkspaceUID:      workspace.Metadata.UID,
			WorkspacePhase:    workspace.Status.Phase,
			WorkspaceRevision: strconv.FormatInt(workspace.Status.ObservedGeneration, 10),
		}, nil
	}
	checks := []struct {
		verb      string
		resource  string
		namespace string
	}{
		{verb: "create", resource: "jobs.batch", namespace: namespace},
		{verb: "get", resource: "jobs.batch", namespace: namespace},
		{verb: "get", resource: "pods", namespace: namespace},
		{verb: "get", resource: "pods/log", namespace: namespace},
		{verb: "get", resource: "localqueues.kueue.x-k8s.io", namespace: namespace},
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
		ContextName:       descriptor.Cluster.ContextName,
		Namespace:         namespace,
		Queue:             queue,
		ServiceAccount:    serviceAccount,
		WorkspaceUID:      workspace.Metadata.UID,
		WorkspacePhase:    workspace.Status.Phase,
		WorkspaceRevision: strconv.FormatInt(workspace.Status.ObservedGeneration, 10),
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
