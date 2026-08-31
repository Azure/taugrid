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
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/core/kube"
)

type clusterUninstallSpec struct {
	KubeContext string
	Release     string
	Namespace   string
	Chart       string
	Timeout     string
	Wait        bool
	DryRun      bool
	KeepHistory bool
	NoHooks     bool
	AssumeYes   bool
	DrainQueue  bool
}

func newClusterUninstallCmd() *cobra.Command {
	spec := clusterUninstallSpec{
		Release:    defaultTauGridRelease,
		Namespace:  defaultTauGridNamespace,
		Chart:      defaultTauGridChart,
		Timeout:    "15m",
		Wait:       true,
		DrainQueue: true,
	}
	cmd := &cobra.Command{
		Use:          "uninstall",
		Short:        "Uninstall the TauGrid Helm release",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		Long: `Uninstall the TauGrid Helm release without ad hoc Kubernetes deletion.

Uninstall runs in two Helm phases. The first removes the release-owned queue
policy while Kueue is still running, so Kueue can release the finalizers it
holds on those objects. The second uninstalls the release. Helm's own uninstall
order deletes the Kueue controller before its custom resources, which would
otherwise leave them undeletable.

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
			// Reading the values costs a Helm round trip, and a dry run that
			// succeeds must not make one. Resolve on first use instead.
			queue := sync.OnceValue(func() baselineQueuePolicy {
				return tauGridBaselineQueue(cmd, spec)
			})
			drained := !spec.DryRun && spec.DrainQueue && drainTauGridQueuePolicy(cmd, spec, queue())
			if err := runHelmCommand(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr(), clusterUninstallArgs(spec)); err != nil {
				if !drained {
					printUninstallRecovery(cmd.ErrOrStderr(), queue())
				}
				return err
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
		"--release":   spec.Release,
		"--namespace": spec.Namespace,
		"--timeout":   spec.Timeout,
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

// baselineQueuePolicy names the cluster-scoped Kueue objects the chart owns.
// They are the ones that strand on a finalizer when Kueue is removed first, so
// both the drain report and the recovery guidance need their real names.
type baselineQueuePolicy struct {
	// Known is false when the release values could not be read, so the names
	// below are chart defaults rather than this cluster's actual ones.
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

// tauGridBaselineQueue reads the release's coalesced values so the drain and
// the recovery guidance name the objects this cluster actually has. An
// unreadable release falls back to the chart defaults, which are correct
// wherever the operator did not rename them, and reports Known false so the
// caller can say the drain was skipped rather than skip it in silence.
func tauGridBaselineQueue(cmd *cobra.Command, spec clusterUninstallSpec) baselineQueuePolicy {
	defaults := baselineQueuePolicy{
		ClusterQueue: "jobqueue",
		Flavor:       "taugrid-default",
		Topology:     "default-node-topology",
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
	// --all reports coalesced values, so the chart's own defaults are present
	// whenever the release rendered a queue at all. A missing switch therefore
	// means the release predates it, and the chart default is enabled.
	policy := baselineQueuePolicy{Known: true, Enabled: queue.Enabled == nil || *queue.Enabled}
	policy.ClusterQueue = cmp.Or(queue.Name, defaults.ClusterQueue)
	policy.Flavor = cmp.Or(queue.Flavor.Name, defaults.Flavor)
	// The chart renders a Topology only when it is enabled, so a disabled one
	// must stay unnamed or the guidance points at an object that never existed.
	if queue.Topology.Enabled == nil || *queue.Topology.Enabled {
		policy.Topology = cmp.Or(queue.Topology.Name, defaults.Topology)
	}
	return policy
}

// deployedChart reports the chart name and version the release was installed
// from. The drain re-renders the whole release, so rendering it from a
// different chart would diff far more than the queue policy.
//
// Helm applies --version only when it has to resolve the chart, so this pins
// the repository case and is inert for a local --chart path, where the files on
// disk are the version by definition. Refusing to drain when the version cannot
// be read keeps the repository case from falling back to a guess.
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

// chartReferenceName is the chart name a --chart value resolves to. Helm
// reports the deployed chart by bare name, while --chart is a reference, so the
// last path segment is what the two have in common across OCI URLs and local
// directories alike.
func chartReferenceName(reference string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(reference), "/")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		return trimmed[i+1:]
	}
	return trimmed
}

// drainTauGridQueuePolicy removes the release-owned queue policy while Kueue is
// still running. It reports whether the policy is gone, so the failure path can
// avoid naming objects the drain already removed. Failing to drain is never
// fatal: the uninstall that follows behaves exactly as it did before this phase
// existed.
func drainTauGridQueuePolicy(cmd *cobra.Command, spec clusterUninstallSpec, queue baselineQueuePolicy) bool {
	// Values are already in hand, so a release known to carry no queue policy
	// needs no further Helm round trips.
	if queue.Known && !queue.Enabled {
		return false
	}
	exists, err := tauGridReleaseExists(cmd, spec.KubeContext, spec.Release, spec.Namespace)
	if err != nil || !exists {
		return false
	}
	// Skipping in silence would read as "nothing to drain" when the truth is
	// "could not tell", and the two leave the cluster in different states.
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
	// The drain is a real mutating upgrade that re-renders the whole release, so
	// a --chart pointing somewhere else would apply a different chart's
	// manifests on the way out. Releases installed before Helm reported a chart
	// name have nothing to compare, and must not be blocked.
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
	// Helm does not wait on resources it drops from a release, so a successful
	// upgrade only means the deletes were accepted. If Kueue is still holding
	// resource-in-use for an active run, the objects stay Terminating and phase
	// two removes the controller underneath them — reporting a drain here would
	// suppress the very guidance that case needs.
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

// queuePolicyPresent reports whether any of the drained objects still exist.
// Indirected so tests can drive both outcomes without a cluster.
var queuePolicyPresent = func(ctx context.Context, spec clusterUninstallSpec, queue baselineQueuePolicy) (bool, error) {
	out, err := kube.New(spec.KubeContext).Raw(ctx,
		append([]string{"get"}, append(queue.objects(), "--ignore-not-found", "-o", "name")...), nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

// clusterUninstallDrainArgs renders the queue policy out of the release without
// touching anything else.
//
// --reuse-values keeps every surviving resource rendering exactly as deployed,
// so the diff Helm computes is only the queue policy; --set still wins over the
// stored value. Deliberately absent: --install would install the chart during
// an uninstall, --atomic would roll back and recreate the queue this phase just
// removed, and --wait waits on the resources that remain rather than on the
// deletions, so it only adds delay on the unhealthy clusters people uninstall.
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

// printUninstallRecovery explains how to finish a teardown Helm could not.
// Deleting the workloads first matters: it is what makes the finalizer
// unreferenced rather than merely unattended, so stripping it afterwards
// cannot orphan a running job.
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
