// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
	"github.com/Azure/taugrid/core/kube"
)

type clusterUninstallSpec struct {
	KubeContext  string
	Release      string
	Namespace    string
	Chart        string
	Timeout      string
	Wait         bool
	DryRun       bool
	KeepHistory  bool
	NoHooks      bool
	AssumeYes    bool
	DrainQueue   bool
	KueueRelease string
}

func newClusterUninstallCmd() *cobra.Command {
	spec := clusterUninstallSpec{
		Release:      defaultTauGridRelease,
		Namespace:    defaultTauGridNamespace,
		Chart:        defaultTauGridChart,
		Timeout:      "15m",
		Wait:         true,
		DrainQueue:   true,
		KueueRelease: defaultTauGridKueueRelease,
	}
	cmd := &cobra.Command{
		Use:          "uninstall",
		Short:        "Uninstall the TauGrid Helm release",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Uninstall the TauGrid Helm release without ad hoc Kubernetes deletion.

Uninstall removes the release-owned queue
policy while Kueue is still running, so Kueue can release the finalizers it
holds on those objects. It then uninstalls TauGrid and, when TauGrid owns the
self-managed Kueue release, removes that separate release last.

Helm removes resources owned by the release. Kubernetes CRDs, user-created
custom resources, retained namespaces, cloud infrastructure, node labels,
storage, and workspace data are intentionally not deleted. Release-owned queue
policy and the TauCluster singleton are removed. Remove or migrate TauWorkspace
objects before uninstalling the controller.`,
		Example: `  tau cluster uninstall --dry-run
  tau cluster uninstall --yes
  tau cluster uninstall --yes --release taugrid --namespace tau-system`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateClusterUninstallSpec(spec); err != nil {
				return err
			}
			if !spec.DryRun && !spec.AssumeYes {
				return fmt.Errorf("uninstall removes the TauGrid control plane; rerun with --yes after migrating active TauWorkspace objects")
			}
			queue := sync.OnceValue(func() baselineQueuePolicy {
				return tauGridBaselineQueue(cmd, spec)
			})
			disabled := disabledTauGridComponents(cmd, spec.KubeContext, spec.Release, spec.Namespace)
			kueueReleaseExists, err := tauGridReleaseExists(
				cmd,
				spec.KubeContext,
				spec.KueueRelease,
				defaultTauGridKueueNS,
			)
			if err != nil {
				return err
			}
			removeKueue := kueueReleaseExists &&
				!slices.Contains(disabled, installationcheck.ComponentKueue)
			drained := !spec.DryRun && spec.DrainQueue && drainTauGridQueuePolicy(cmd, spec, queue())
			if err := runHelmCommand(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), clusterUninstallArgs(spec)); err != nil {
				if !drained {
					printUninstallRecovery(cmd.ErrOrStderr(), queue())
				}
				return err
			}
			if removeKueue {
				if err := runHelmCommand(
					cmd.Context(),
					cmd.InOrStdin(),
					cmd.OutOrStdout(),
					cmd.ErrOrStderr(),
					clusterUninstallKueueArgs(spec),
				); err != nil {
					return fmt.Errorf("TauGrid release was removed, but self-managed Kueue removal failed: %w", err)
				}
			}
			if spec.DryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "\nTauGrid Helm uninstall dry-run completed; no cluster resources were changed.")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), `
TauGrid Helm release removed. CRDs, user-created custom resources, retained
namespaces, cloud infrastructure, storage, and workspace data remain in the
cluster.`)
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&spec.KubeContext, "context", defaultKubeContext(), kubeContextHelp())
	flags.StringVar(&spec.Release, "release", spec.Release, "Helm release name")
	flags.StringVar(&spec.Namespace, "namespace", spec.Namespace, "Helm release namespace")
	flags.StringVar(&spec.Chart, "chart", spec.Chart, "TauGrid chart reference used to drain the queue policy")
	flags.StringVar(&spec.KueueRelease, "kueue-release", spec.KueueRelease, "self-managed Kueue Helm release name")
	flags.StringVar(&spec.Timeout, "timeout", spec.Timeout, "Helm operation timeout")
	flags.BoolVar(&spec.Wait, "wait", spec.Wait, "wait for release resources to be removed")
	flags.BoolVar(&spec.DryRun, "dry-run", false, "show Helm's uninstall plan without changing the cluster")
	flags.BoolVar(&spec.KeepHistory, "keep-history", false, "retain the Helm release history")
	flags.BoolVar(&spec.NoHooks, "no-hooks", false, "prevent uninstall hooks from running")
	flags.BoolVar(&spec.AssumeYes, "yes", false, "confirm removal of the TauGrid control plane")
	flags.BoolVar(&spec.DrainQueue, "drain-queue", spec.DrainQueue, "remove the queue policy while Kueue still runs, so its finalizers are released")
	return cmd
}

func validateClusterUninstallSpec(spec clusterUninstallSpec) error {
	required := map[string]string{
		"--release":       spec.Release,
		"--namespace":     spec.Namespace,
		"--timeout":       spec.Timeout,
		"--kueue-release": spec.KueueRelease,
	}
	if spec.DrainQueue {
		required["--chart"] = spec.Chart
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	return nil
}

type baselineQueuePolicy struct {
	Known        bool
	Enabled      bool
	ClusterQueue string
	Flavor       string
	Topology     string
}

func (q baselineQueuePolicy) objects() []string {
	objects := []string{"clusterqueue/" + q.ClusterQueue, "resourceflavor/" + q.Flavor}
	if q.Topology != "" {
		objects = append(objects, "topology/"+q.Topology)
	}
	return objects
}

func tauGridBaselineQueue(cmd *cobra.Command, spec clusterUninstallSpec) baselineQueuePolicy {
	defaults := baselineQueuePolicy{
		ClusterQueue: "jobqueue",
		Flavor:       "taugrid-default-cpu",
		Topology:     "taugrid-gpu-topology",
	}
	raw, err := tauGridReleaseValues(cmd, spec.KubeContext, spec.Release, spec.Namespace)
	if err != nil {
		return defaults
	}
	var values struct {
		BaselineQueue struct {
			Enabled *bool  `json:"enabled"`
			Name    string `json:"name"`
			Flavor  struct {
				Name string `json:"name"`
			} `json:"flavor"`
			Topology struct {
				Enabled *bool  `json:"enabled"`
				Name    string `json:"name"`
			} `json:"topology"`
		} `json:"baselineQueue"`
	}
	if err := json.Unmarshal(raw, &values); err != nil {
		return defaults
	}
	queue := values.BaselineQueue
	policy := baselineQueuePolicy{Known: true, Enabled: queue.Enabled == nil || *queue.Enabled}
	policy.ClusterQueue = cmp.Or(queue.Name, defaults.ClusterQueue)
	policy.Flavor = cmp.Or(queue.Flavor.Name, defaults.Flavor)
	if queue.Topology.Enabled == nil || *queue.Topology.Enabled {
		policy.Topology = cmp.Or(queue.Topology.Name, defaults.Topology)
	}
	return policy
}

func deployedChart(cmd *cobra.Command, spec clusterUninstallSpec) (name, version string, ok bool) {
	args := appendKubeContext(
		[]string{"get", "metadata", spec.Release, "--namespace", spec.Namespace, "--output", "json"},
		spec.KubeContext,
	)
	var out bytes.Buffer
	if err := runHelmCommand(cmd.Context(), cmd.InOrStdin(), &out, io.Discard, args); err != nil {
		return "", "", false
	}
	var metadata struct {
		Chart   string `json:"chart"`
		Version string `json:"version"`
	}
	if err := json.Unmarshal(out.Bytes(), &metadata); err != nil || metadata.Version == "" {
		return "", "", false
	}
	return metadata.Chart, metadata.Version, true
}

func chartReferenceName(reference string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(reference), "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

func drainTauGridQueuePolicy(cmd *cobra.Command, spec clusterUninstallSpec, queue baselineQueuePolicy) bool {
	if queue.Known && !queue.Enabled {
		return false
	}
	exists, err := tauGridReleaseExists(cmd, spec.KubeContext, spec.Release, spec.Namespace)
	if err != nil || !exists {
		return false
	}
	if !queue.Known {
		fmt.Fprintf(cmd.ErrOrStderr(), "Cannot read the values of release %s; skipping the queue drain, so %s may be left with a Kueue finalizer that no controller can clear.\n",
			spec.Release, strings.Join(queue.objects(), ", "))
		return false
	}
	chart, version, ok := deployedChart(cmd, spec)
	if !ok {
		fmt.Fprintf(cmd.ErrOrStderr(), "Cannot read the deployed chart version for release %s; skipping the queue drain.\n", spec.Release)
		return false
	}
	if want := chartReferenceName(spec.Chart); chart != "" && chart != want {
		fmt.Fprintf(cmd.ErrOrStderr(), "Release %s was installed from chart %q but --chart resolves to %q; skipping the queue drain, so %s may be left with a Kueue finalizer that no controller can clear.\n",
			spec.Release, chart, want, strings.Join(queue.objects(), ", "))
		return false
	}
	if err := runTauGridHelmCommand(
		cmd,
		spec.Chart,
		version,
		cmd.OutOrStdout(),
		clusterUninstallDrainArgs(spec, version),
	); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Cannot drain the queue policy (%v); %s may be left with a Kueue finalizer that no controller can clear.\n",
			err, strings.Join(queue.objects(), ", "))
		return false
	}
	present, err := queuePolicyPresent(cmd.Context(), spec, queue)
	if err != nil || present {
		reason := "could not be confirmed removed"
		if present {
			reason = "is still present"
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "Drained the queue policy, but %s %s; Kueue may still hold resource-in-use for an active run.\n",
			strings.Join(queue.objects(), ", "), reason)
		return false
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Removed the queue policy (%s) while Kueue was still running.\n", strings.Join(queue.objects(), ", "))
	return true
}

var queuePolicyPresent = func(ctx context.Context, spec clusterUninstallSpec, queue baselineQueuePolicy) (bool, error) {
	out, err := kube.New(spec.KubeContext).Raw(ctx,
		append([]string{"get"}, append(queue.objects(), "--ignore-not-found", "-o", "name")...), nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func clusterUninstallDrainArgs(spec clusterUninstallSpec, version string) []string {
	args := []string{
		"upgrade", spec.Release, spec.Chart,
		"--namespace", spec.Namespace,
		"--version", version,
		"--timeout", spec.Timeout,
		"--reuse-values",
		"--set", "baselineQueue.enabled=false",
	}
	args = appendKubeContext(args, spec.KubeContext)
	if spec.NoHooks {
		args = append(args, "--no-hooks")
	}
	return appendForceConflicts(args)
}

func printUninstallRecovery(out io.Writer, queue baselineQueuePolicy) {
	fmt.Fprintf(out, `
Uninstall did not finish. These release-owned objects may remain, holding a
Kueue finalizer that no controller is left to clear:

  %s

To finish the teardown, first remove anything still referencing the queue:

  kubectl delete job,workload --all -n <workspace-namespace>

Then, only once nothing references them, clear the stranded finalizers:

  for r in %s; do
    kubectl patch $r --type=merge -p '{"metadata":{"finalizers":[]}}'
  done
`, strings.Join(queue.objects(), "\n  "), strings.Join(queue.objects(), " "))
}

func clusterUninstallArgs(spec clusterUninstallSpec) []string {
	args := []string{
		"uninstall", spec.Release,
		"--namespace", spec.Namespace,
		"--timeout", spec.Timeout,
		"--ignore-not-found",
	}

	args = appendKubeContext(args, spec.KubeContext)
	if spec.Wait {
		args = append(args, "--wait")
	}
	if spec.DryRun {
		args = append(args, "--dry-run")
	}
	if spec.KeepHistory {
		args = append(args, "--keep-history")
	}
	if spec.NoHooks {
		args = append(args, "--no-hooks")
	}
	return args
}

func clusterUninstallKueueArgs(spec clusterUninstallSpec) []string {
	kueueSpec := spec
	kueueSpec.Release = spec.KueueRelease
	kueueSpec.Namespace = defaultTauGridKueueNS
	return clusterUninstallArgs(kueueSpec)
}
