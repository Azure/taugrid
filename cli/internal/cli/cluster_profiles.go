// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Azure/taugrid/core/kube"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/dynamic"
)

var newClusterProfileClient = func(kubeContext string) (dynamic.Interface, error) {
	config, err := kube.New(kubeContext).RESTConfig()
	if err != nil {
		return nil, fmt.Errorf("resolve Kubernetes client: %w", err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("create Kubernetes client: %w", err)
	}
	return client, nil
}

func newClusterProfilesCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "Inspect ready TauCluster workload profiles",
	}
	cmd.AddCommand(newClusterProfilesExportCmd())
	return cmd
}

func newClusterProfilesExportCmd() *cobra.Command {
	var kubeContext string
	var outputPath string
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export the ready TauCluster workload profiles",
		Long: `Fetch the singleton TauCluster and export its current ready workload profile
status as a deterministic TauWorkloadProfileSnapshot. The command fails closed
when status is missing, stale, unready, or has a mismatched profile-set hash.
It reads only the ready TauCluster status published by the controller.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, err := newClusterProfileClient(kubeContext)
			if err != nil {
				return err
			}
			return exportClusterProfiles(cmd.Context(), client, cmd.OutOrStdout(), outputPath)
		},
	}
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), kubeContextHelp())
	cmd.Flags().StringVarP(&outputPath, "output", "o", "", "write the snapshot to this path instead of stdout")
	return cmd
}

func exportClusterProfiles(
	ctx context.Context,
	client dynamic.Interface,
	out io.Writer,
	outputPath string,
) error {
	set, err := profile.NewClusterProvider(client).ProfileSet(ctx)
	if err != nil {
		return err
	}
	snapshot, err := profile.NewProfileSetSnapshot(set.Generation, set.Profiles)
	if err != nil {
		return fmt.Errorf("create workload profile snapshot: %w", err)
	}
	if snapshot.ProfileSetHash != set.ProfileSetHash {
		return fmt.Errorf(
			"refuse workload profile export: snapshot hash %q does not match TauCluster status hash %q",
			snapshot.ProfileSetHash,
			set.ProfileSetHash,
		)
	}
	data, err := yaml.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("encode workload profile snapshot: %w", err)
	}
	if outputPath == "" {
		if _, err := out.Write(data); err != nil {
			return fmt.Errorf("write workload profile snapshot: %w", err)
		}
		return nil
	}
	return writeProfileSnapshotFile(outputPath, data)
}

func writeProfileSnapshotFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".snapshot-*")
	if err != nil {
		return fmt.Errorf("create workload profile snapshot next to %q: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set workload profile snapshot permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write workload profile snapshot %q: %w", path, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync workload profile snapshot %q: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close workload profile snapshot %q: %w", path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace workload profile snapshot %q: %w", path, err)
	}
	return nil
}
