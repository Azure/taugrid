// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/util/validation"

	profile "github.com/Azure/taugrid/core/resourceprofile"
)

const serveProfileSourceAnnotation = "tau.azure.com/workload-profile-source"

type serveProfileOptions struct {
	Kind          string
	Name          string
	Namespace     string
	Context       string
	Snapshot      string
	DryRun        string
	ExplicitGPUs  *int
	ExplicitNodes *int
}

type serveProfileTarget struct {
	Profile   profile.Profile
	Selected  *selectedWorkloadProfile
	Namespace string
	Workspace string
	Context   string
	Runner    kubeRawRunner
	Restore   func()
}

func resolveServeProfileTarget(cmd *cobra.Command, options serveProfileOptions) (serveProfileTarget, error) {
	if options.Snapshot != "" {
		return resolveSnapshotServeProfile(cmd, options)
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return serveProfileTarget{}, fmt.Errorf("resolve current repository: %w", err)
	}
	resolver := newActiveWorkspaceResolver(newServeConnectionEnsurer, fetchServeWorkspace)
	active, err := resolver.Resolve(cmd, activeWorkspaceRequest{
		Source:                  runConnectionSource{StartDir: workingDirectory},
		KubeContext:             options.Context,
		KubeContextExplicit:     runContextExplicit(cmd),
		KubeContextFromFlag:     cmd.Flags().Changed("context"),
		Namespace:               options.Namespace,
		RequireRepositoryTarget: true,
	})
	if err != nil {
		return serveProfileTarget{}, err
	}
	success := false
	defer func() {
		if !success {
			active.Restore()
		}
	}()
	runner := newServeRunner(active.Context)
	placement := active.Placement
	target, err := resolveServeWorkspaceTarget(
		cmd.Context(), runner, placement.Namespace, placement.LocalQueue,
		placement.ClusterQueue, serveWorkloadResource(options.Kind),
	)
	if err != nil {
		return serveProfileTarget{}, err
	}
	client, err := newClusterProfileClient(active.Context)
	if err != nil {
		return serveProfileTarget{}, err
	}
	p, selected, err := selectServeWorkloadProfile(
		cmd.Context(), profile.NewClusterProvider(client), options.Name,
		target.Namespace, target.Queue, target.ClusterQueue, options.ExplicitGPUs, options.ExplicitNodes, options.Kind,
	)
	if err != nil {
		return serveProfileTarget{}, err
	}
	success = true
	return serveProfileTarget{
		Profile: p, Selected: selected, Namespace: target.Namespace,
		Workspace: placement.Workspace, Context: active.Context,
		Runner: runner, Restore: active.Restore,
	}, nil
}

func resolveSnapshotServeProfile(cmd *cobra.Command, options serveProfileOptions) (serveProfileTarget, error) {
	if options.DryRun != "client" {
		return serveProfileTarget{}, fmt.Errorf("--workload-profile-snapshot requires --dry-run=client; snapshots cannot authorize server dry-run or apply")
	}
	namespace := strings.TrimSpace(options.Namespace)
	if namespace == "" {
		return serveProfileTarget{}, fmt.Errorf("--namespace is required with --workload-profile-snapshot")
	}
	if problems := validation.IsDNS1123Label(namespace); len(problems) > 0 {
		return serveProfileTarget{}, fmt.Errorf("invalid snapshot namespace %q: %s", namespace, strings.Join(problems, "; "))
	}
	if cmd.Flags().Changed("context") {
		return serveProfileTarget{}, fmt.Errorf("--context cannot be combined with --workload-profile-snapshot; snapshot rendering is offline")
	}
	data, err := os.ReadFile(options.Snapshot)
	if err != nil {
		return serveProfileTarget{}, fmt.Errorf("read --workload-profile-snapshot: %w", err)
	}
	provider, err := profile.DecodeSnapshotProvider(data)
	if err != nil {
		return serveProfileTarget{}, fmt.Errorf("load --workload-profile-snapshot: %w", err)
	}
	p, selected, err := selectServeWorkloadProfile(
		cmd.Context(), provider, options.Name, namespace, "", "", options.ExplicitGPUs, options.ExplicitNodes, options.Kind,
	)
	if err != nil {
		return serveProfileTarget{}, err
	}
	return serveProfileTarget{
		Profile: p, Selected: selected, Namespace: namespace, Restore: func() {},
	}, nil
}

func validateServeKind(kind string) error {
	switch kind {
	case "rayservice", "deployment":
		return nil
	default:
		return fmt.Errorf("--kind must be one of: rayservice, deployment")
	}
}
