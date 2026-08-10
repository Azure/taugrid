// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/installationcheck"
)

const (
	defaultTauGridRelease      = "taugrid"
	defaultTauGridNamespace    = "tau-system"
	defaultTauGridChartVersion = "0.2.0"
	defaultTauGridChart        = "oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid"
)

type clusterInstallSpec struct {
	KubeContext  string
	Release      string
	Namespace    string
	Chart        string
	Version      string
	Timeout      string
	ValuesFiles  []string
	SetValues    []string
	SetString    []string
	CreateNS     bool
	Wait         bool
	Atomic       bool
	DryRun       bool
	DependencyUp bool
}

func newClusterInstallCmd() *cobra.Command {
	spec := clusterInstallSpec{
		Release:      defaultTauGridRelease,
		Namespace:    defaultTauGridNamespace,
		Chart:        defaultTauGridChart,
		Version:      defaultTauGridChartVersion,
		Timeout:      "15m",
		CreateNS:     true,
		Wait:         false,
		Atomic:       false,
		DependencyUp: true,
	}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install or upgrade the TauGrid Helm distribution",
		Args:  cobra.NoArgs,
		Long: `Install the versioned TauGrid distribution on an existing Kubernetes cluster.

The chart owns cluster-scoped installation state: Kueue, KubeRay, Tau APIs,
tau-core-controller, and the portable baseline queue.
Tau does not apply lane manifests, create PVCs, label nodes, or create workload
identity resources directly. Create a TauWorkspace after installation; the
controller reconciles its namespace, access, LocalQueue, and optional
ServiceAccount.

Storage-backed workloads must name an existing PVC in their target namespace.
The platform pre-provisions and owns that claim, its StorageClass, CSI setup,
cloud storage, access policy, and lifecycle outside TauGrid.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := validateClusterInstallSpec(spec); err != nil {
				return err
			}
			validationTimeout, err := time.ParseDuration(spec.Timeout)
			if err != nil {
				return fmt.Errorf("invalid --timeout: %w", err)
			}
			if validationTimeout <= 0 {
				return fmt.Errorf("invalid --timeout: must be greater than zero")
			}
			printClusterInstallPlan(cmd, spec)
			if spec.DryRun {
				var rendered bytes.Buffer
				if err := runClusterInstallHelm(cmd, spec, &rendered, clusterInstallRenderArgs(spec)); err != nil {
					return err
				}
				if err := printRenderedKindSummary(cmd.OutOrStdout(), rendered.Bytes()); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "\nRendered the TauGrid manifests offline. The cluster was not read or changed.")
				return nil
			}
			releaseExists, err := tauGridReleaseExists(cmd, spec.KubeContext, spec.Release, spec.Namespace)
			if err != nil {
				return err
			}
			if !releaseExists {
				if err := runClusterInstallHelm(cmd, spec, cmd.OutOrStdout(), clusterInstallBootstrapArgs(spec)); err != nil {
					return fmt.Errorf("install TauGrid control plane before queue policy: %w", err)
				}
			}
			if err := runClusterInstallHelm(cmd, spec, cmd.OutOrStdout(), clusterInstallArgs(spec)); err != nil {
				return err
			}
			disabled := disabledTauGridComponents(cmd, spec.KubeContext, spec.Release, spec.Namespace)
			if err := runTauGridInstallationValidation(
				cmd.Context(),
				newInstallationCheckRunner(spec.KubeContext),
				installationcheck.Options{
					Release:               spec.Release,
					ControlPlaneNamespace: spec.Namespace,
					Timeout:               validationTimeout,
					PollInterval:          defaultInstallationValidationPollInterval,
					DisabledComponents:    disabled,
				},
				cmd.OutOrStdout(),
			); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), `
TauGrid is installed and ready as Helm release %s in namespace %s.

Next:
  1. Create a workspace: tau workspace create --principal-name <external-group-or-team> --apply
  2. Wait for: kubectl get workspaces.tau.azure.com -n tau-platform
  3. Before storage-backed runs, pre-provision a Bound PVC in the workspace namespace.
  4. Generate its repository with: tau workspace init-repo
  5. Give the repository to the researcher; their first command is: tau run smoke
`, spec.Release, spec.Namespace)
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&spec.KubeContext, "context", defaultKubeContext(), kubeContextHelp())
	flags.StringVar(&spec.Release, "release", spec.Release, "Helm release name")
	flags.StringVar(&spec.Namespace, "namespace", spec.Namespace, "namespace for Kueue and KubeRay control-plane workloads")
	flags.StringVar(&spec.Chart, "chart", spec.Chart, "TauGrid chart reference or local chart path")
	flags.StringVar(&spec.Version, "version", spec.Version, "TauGrid chart version")
	flags.StringVar(&spec.Timeout, "timeout", spec.Timeout, "timeout for each Helm operation and the readiness wait")
	flags.StringArrayVarP(&spec.ValuesFiles, "values", "f", nil, "additional Helm values file (repeatable)")
	flags.StringArrayVar(&spec.SetValues, "set", nil, "set a Helm value (repeatable)")
	flags.StringArrayVar(&spec.SetString, "set-string", nil, "set a Helm string value (repeatable)")
	flags.BoolVar(&spec.CreateNS, "create-namespace", spec.CreateNS, "create the release namespace")
	flags.BoolVar(&spec.Wait, "wait", spec.Wait, "also run Helm's generic resource watcher before Tau's component-aware readiness validation")
	flags.BoolVar(&spec.Atomic, "atomic", spec.Atomic, "roll back on Helm failure (also enables Helm's generic watcher wait)")
	flags.BoolVar(&spec.DryRun, "dry-run", false, "summarize the chart manifests offline without contacting the cluster")
	flags.BoolVar(&spec.DependencyUp, "dependency-update", spec.DependencyUp, "update missing chart dependencies")
	return cmd
}

func validateClusterInstallSpec(spec clusterInstallSpec) error {
	for name, value := range map[string]string{
		"--release":   spec.Release,
		"--namespace": spec.Namespace,
		"--chart":     spec.Chart,
		"--version":   spec.Version,
		"--timeout":   spec.Timeout,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	return nil
}

func printClusterInstallPlan(cmd *cobra.Command, spec clusterInstallSpec) {
	helmWait := "bootstrap only (Tau readiness validation still runs after queue policy)"
	if spec.Wait {
		helmWait = "enabled for both passes"
	}
	rollback := "disabled"
	if spec.Atomic {
		rollback = "enabled (implies Helm watcher wait)"
	}
	fmt.Fprintf(cmd.OutOrStdout(), `TauGrid installation plan
  Release:    %s
  Namespace:  %s
  Chart:      %s
  Version:    %s
  Helm wait:  %s
  Rollback:   %s
  Components: Kueue, KubeRay, tau-core-controller, TauCluster, baseline queue, quota admission guard
  Validation: Kubernetes >=1.30 and all required control-plane resources ready

`, spec.Release, spec.Namespace, spec.Chart, spec.Version, helmWait, rollback)
}

func runClusterInstallHelm(cmd *cobra.Command, spec clusterInstallSpec, out io.Writer, args []string) error {
	return runTauGridHelmCommand(cmd, spec.Chart, spec.Version, out, args)
}

func tauGridReleaseExists(cmd *cobra.Command, kubeContext, release, namespace string) (bool, error) {
	args := []string{
		"list",
		"--namespace", namespace,
	}
	// Helm 4 removed the `--all` aggregate flag from `helm list`; the individual
	// state flags it expanded to are still accepted by both Helm 3 and Helm 4,
	// so list them explicitly to stay portable across both major versions.
	args = append(args, helmListAllStateFlags...)
	args = append(args, "--output", "json")
	if kubeContext != "" {
		args = append(args, "--kube-context", kubeContext)
	}
	var output bytes.Buffer
	if err := runHelmCommand(cmd.Context(), cmd.InOrStdin(), &output, cmd.ErrOrStderr(), args); err != nil {
		return false, fmt.Errorf("inspect existing TauGrid Helm release: %w", err)
	}
	var releases []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output.Bytes(), &releases); err != nil {
		return false, fmt.Errorf("decode Helm release list: %w", err)
	}
	for _, r := range releases {
		if r.Name == release {
			return true, nil
		}
	}
	return false, nil
}

func clusterInstallBootstrapArgs(spec clusterInstallSpec) []string {
	// The queue-policy pass creates Kueue resources that are admitted through
	// the webhooks installed by the bootstrap pass. Wait for those Deployments
	// before asking the API server to call their webhooks.
	spec.Wait = true
	args := clusterInstallArgs(spec)
	return append(args, "--set", "baselineQueue.enabled=false")
}

// Helm resolves every rendered object's GVK against the live cluster even for
// --dry-run=client, so a chart that ships CRDs and their custom resources can
// never dry-run against a cluster that lacks them. Rendering with `helm
// template` keeps the preview offline.
func clusterInstallRenderArgs(spec clusterInstallSpec) []string {
	args := []string{
		"template", spec.Release, spec.Chart,
		"--namespace", spec.Namespace,
		"--version", spec.Version,
		"--include-crds",
	}
	if spec.KubeContext != "" {
		args = append(args, "--kube-context", spec.KubeContext)
	}
	if spec.DependencyUp {
		args = append(args, "--dependency-update")
	}
	return appendHelmValueArgs(args, spec)
}

func printRenderedKindSummary(out io.Writer, manifest []byte) error {
	counts := map[string]int{}
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	for {
		var obj struct {
			Kind string `yaml:"kind"`
		}
		err := dec.Decode(&obj)
		if errors.Is(err, io.EOF) {
			break
		}
		// --dependency-update prints its progress banner on stdout, ahead of the
		// first document. Skip anything that is not an object and keep decoding.
		var typeErr *yaml.TypeError
		if errors.As(err, &typeErr) {
			continue
		}
		if err != nil {
			return fmt.Errorf("parse rendered manifest: %w", err)
		}
		if kind := strings.TrimSpace(obj.Kind); kind != "" {
			counts[kind]++
		}
	}

	kinds := make([]string, 0, len(counts))
	width := len("Total")
	total := 0
	for kind, count := range counts {
		kinds = append(kinds, kind)
		width = max(width, len(kind))
		total += count
	}
	sort.Slice(kinds, func(i, j int) bool {
		if counts[kinds[i]] != counts[kinds[j]] {
			return counts[kinds[i]] > counts[kinds[j]]
		}
		return kinds[i] < kinds[j]
	})

	fmt.Fprintln(out, "TauGrid render summary (nothing was applied)")
	for _, kind := range kinds {
		fmt.Fprintf(out, "  %-*s  %4d\n", width, kind, counts[kind])
	}
	fmt.Fprintf(out, "  %-*s  %4d objects\n", width, "Total", total)
	return nil
}

func clusterInstallArgs(spec clusterInstallSpec) []string {
	// Helm >=3.18 replays the previous release's user-supplied values on upgrade.
	// Without --reset-values the bootstrap pass's baselineQueue.enabled=false
	// survives into the pass that must create the baseline queue.
	args := []string{
		"upgrade", "--install", spec.Release, spec.Chart,
		"--namespace", spec.Namespace,
		"--version", spec.Version,
		"--timeout", spec.Timeout,
		"--reset-values",
	}
	if spec.KubeContext != "" {
		args = append(args, "--kube-context", spec.KubeContext)
	}
	if spec.CreateNS {
		args = append(args, "--create-namespace")
	}
	if spec.DependencyUp {
		args = append(args, "--dependency-update")
	}
	if spec.Wait {
		args = append(args, "--wait")
	}
	if spec.Atomic {
		args = appendRollbackOnFailure(args)
	}
	return appendHelmValueArgs(appendForceConflicts(args), spec)
}

// Both Helm verbs must resolve chart values identically, or a dry-run would
// preview something other than what installs.
func appendHelmValueArgs(args []string, spec clusterInstallSpec) []string {
	for _, file := range spec.ValuesFiles {
		args = append(args, "--values", file)
	}
	for _, value := range spec.SetValues {
		args = append(args, "--set", value)
	}
	for _, value := range spec.SetString {
		args = append(args, "--set-string", value)
	}
	return args
}
