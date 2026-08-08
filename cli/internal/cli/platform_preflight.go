// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/depcontract"
	"github.com/Azure/taugrid/cli/internal/platform"
	"github.com/Azure/taugrid/core/kube"
)

// platformPreflightRunnerFactory builds a read-only kubectl runner for a
// given worker kube context. Production wiring uses kube.New; tests inject
// a fake so no live cluster is required.
type platformPreflightRunnerFactory func(workerContext string) platform.Runner

func newPlatformPreflightMultiKueueCmd() *cobra.Command {
	var manifestPath string
	var workerContexts []string
	var namespace string

	cmd := &cobra.Command{
		Use:   "preflight-multikueue",
		Short: "Read-only MultiKueue worker dependency parity check (operator/canary tool)",
		Long: `preflight-multikueue classifies the pre-provisioned, name-parity
dependencies (Secret, PersistentVolumeClaim + StorageClass, ServiceAccount,
ImagePullSecret, SecretProviderClass) of a rendered Job/RayJob manifest,
then runs read-only "kubectl get" checks for each one against every
explicitly supplied MultiKueue worker kube context.

It never mutates a cluster, never reads a Secret's data, and never
discovers or distributes worker credentials: every --worker-context you
pass must already be usable by you, the operator invoking this command.

Container image references are listed for inventory only — a kubectl get
cannot prove an image will pull successfully on a worker. See issue #871
for live-pull canary evidence, and
https://github.com/Azure/taugrid/blob/main/site/content/en/docs/operations/multicluster.md for the full dependency
contract this command checks against.

This is a platform/operator tool: it is not, and must never be, invoked
automatically by "tau run" or "tau serve".`,
		RunE: func(cmd *cobra.Command, args []string) error {
			manifest, err := readPlatformPreflightManifest(manifestPath)
			if err != nil {
				return err
			}
			return runPlatformPreflightMultiKueue(
				cmd.Context(),
				manifest,
				workerContexts,
				namespace,
				func(workerContext string) platform.Runner { return kube.New(workerContext) },
				cmd.OutOrStdout(),
			)
		},
	}

	cmd.Flags().StringVarP(&manifestPath, "manifest", "f", "", `path to a rendered Job/RayJob manifest ("-" for stdin); required`)
	cmd.Flags().StringArrayVar(&workerContexts, "worker-context", nil, "kubectl context of a MultiKueue worker cluster to check (repeatable, required, must already be usable by the operator)")
	cmd.Flags().StringVar(&namespace, "namespace", "", "namespace override for every namespaced dependency check; takes precedence over the manifest's own metadata.namespace when set (default: use the manifest's own metadata.namespace)")
	_ = cmd.MarkFlagRequired("manifest")
	_ = cmd.MarkFlagRequired("worker-context")

	return cmd
}

func readPlatformPreflightManifest(path string) ([]byte, error) {
	if path == "" {
		return nil, fmt.Errorf("--manifest is required")
	}
	if path == "-" {
		return io.ReadAll(os.Stdin)
	}
	return os.ReadFile(path)
}

// runPlatformPreflightMultiKueue is the cobra-free core of the command so
// tests can exercise it directly with a fake runner factory.
func runPlatformPreflightMultiKueue(
	ctx context.Context,
	manifest []byte,
	workerContexts []string,
	namespaceOverride string,
	newRunner platformPreflightRunnerFactory,
	out io.Writer,
) error {
	if len(workerContexts) == 0 {
		return fmt.Errorf("--worker-context is required at least once")
	}

	workloads, err := depcontract.Classify(manifest)
	if err != nil {
		return fmt.Errorf("classify manifest: %w", err)
	}
	if len(workloads) == 0 {
		return fmt.Errorf("no Job or RayJob document found in manifest")
	}

	workers := make([]platform.Worker, 0, len(workerContexts))
	for _, workerContext := range workerContexts {
		workers = append(workers, platform.Worker{
			Context: workerContext,
			Runner:  newRunner(workerContext),
		})
	}

	report, err := platform.CheckMultiKueuePreflight(ctx, workers, depcontract.Flatten(workloads), platform.Options{
		// namespaceOverride is passed through as-is: empty means "use
		// each dependency's own classified namespace", non-empty means
		// "override every namespaced dependency's namespace", never a
		// fallback derived from the manifest itself.
		NamespaceOverride: namespaceOverride,
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(out, report.Summary())
	return report.Err()
}
