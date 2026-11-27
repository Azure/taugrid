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
	defaultTauGridChartVersion = "0.4.8"
	defaultTauGridChart        = "oci://mcr.microsoft.com/aks/ai-runtime/helm/taugrid"
	operatorModeManaged        = "managed"
	operatorModeHelm           = "helm"
	operatorModeLegacyPartial  = "legacy-partial"
)

type clusterInstallSpec struct {
	KubeContext  string
	Release      string
	Namespace    string
	Chart        string
	Version      string
	Mode         string
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
		Mode:         operatorModeManaged,
		Timeout:      "15m",
		CreateNS:     true,
		Wait:         false,
		Atomic:       false,
		DependencyUp: true,
	}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install or upgrade TauGrid with managed or Helm-owned operators",
		Args:  cobra.NoArgs,
		Long: `Install the versioned TauGrid distribution on an existing Kubernetes cluster.

Managed mode is the default AKS path. The microsoft.kueue and
microsoft.kuberay extensions must already be installed and ready; TauGrid
validates their ownership and compatibility, then installs no operator CRDs,
controllers, or webhooks.

Helm mode is the explicit self-managed path. The TauGrid release owns the
Kueue and KubeRay lifecycle and refuses existing foreign operator or CRD
ownership.
On upgrade, Tau updates its own CRDs from the selected chart before running
Helm. Existing custom resources are retained; Helm rollback does not revert
these CRD updates. Third-party CRD migrations remain platform-owned.
Tau does not apply lane manifests, create PVCs, label nodes, or create workload
identity resources directly. Create a TauWorkspace after installation; the
controller reconciles its namespace, access, LocalQueue, and optional
ServiceAccount.

Storage-backed workloads must name an existing PVC in their target namespace.
The platform pre-provisions and owns that claim, its StorageClass, CSI setup,
cloud storage, access policy, and lifecycle outside TauGrid.`,
		Example: `  tau cluster install
  tau cluster install --mode helm
  tau cluster install --mode managed --set baselineQueue.enabled=false
  tau cluster install --dry-run
  tau cluster install --namespace tau-system --set portal.enabled=false --wait`,
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
			installationRunner := newInstallationCheckRunner(spec.KubeContext)
			if releaseExists {
				existingMode, err := installedOperatorMode(cmd, spec.KubeContext, spec.Release, spec.Namespace)
				if err != nil {
					return err
				}
				if existingMode != spec.Mode {
					return fmt.Errorf(
						"cannot change operator ownership mode from %s to %s in place; no safe same-cluster transfer can verify the replacement before removing the active singleton controllers: keep the current mode, or provision and verify a separate cluster in %s mode, migrate reviewed workload state, then uninstall the old %s-owned installation",
						existingMode,
						spec.Mode,
						spec.Mode,
						existingMode,
					)
				}
			}
			if err := runOperatorOwnershipPreflight(cmd.Context(), installationRunner, spec, releaseExists); err != nil {
				return err
			}
			if releaseExists {
				if err := ensureSystemNamespaceMigrationSafe(cmd.Context(), installationRunner, spec.Namespace); err != nil {
					return err
				}
				if err := upgradeTauGridCRDs(cmd, installationRunner, spec); err != nil {
					return err
				}
			}
			if !releaseExists && spec.Mode == operatorModeHelm {
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
				installationRunner,
				installationcheck.Options{
					Release:                 spec.Release,
					SystemNamespace:         spec.Namespace,
					Timeout:                 validationTimeout,
					PollInterval:            defaultInstallationValidationPollInterval,
					DisabledComponents:      disabled,
					ExternalComponentOwners: externalOperatorComponents(spec.Mode),
				},
				cmd.OutOrStdout(),
			); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), `
TauGrid is installed and ready as Helm release %s in namespace %s.

Next:
  1. Create a workspace: tau workspace create --system-namespace %s --principal-name <external-group-or-team> --apply
  2. Wait for: kubectl get workspaces.tau.azure.com -n %s
  3. Before storage-backed runs, pre-provision a Bound PVC in the workspace namespace.
  4. Generate its repository with: tau workspace init-repo
  5. Give the repository to the researcher; they can run its checked-in target with: tau run train
`, spec.Release, spec.Namespace, spec.Namespace, spec.Namespace)
			return nil
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&spec.KubeContext, "context", defaultKubeContext(), kubeContextHelp())
	flags.StringVar(&spec.Release, "release", spec.Release, "Helm release name")
	flags.StringVar(&spec.Namespace, "namespace", spec.Namespace, "namespace for all TauGrid system workloads and TauWorkspace objects")
	flags.StringVar(&spec.Chart, "chart", spec.Chart, "TauGrid chart reference or local chart path")
	flags.StringVar(&spec.Version, "version", spec.Version, "TauGrid chart version")
	flags.StringVar(&spec.Mode, "mode", spec.Mode, "operator ownership mode: managed or helm")
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
		"--mode":      spec.Mode,
		"--timeout":   spec.Timeout,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("%s must not be empty", name)
		}
	}
	if spec.Mode != operatorModeManaged && spec.Mode != operatorModeHelm {
		return fmt.Errorf("--mode must be %q or %q", operatorModeManaged, operatorModeHelm)
	}
	return nil
}

func printClusterInstallPlan(cmd *cobra.Command, spec clusterInstallSpec) {
	helmWait := "disabled (Tau component-aware readiness validation still runs)"
	if spec.Mode == operatorModeHelm {
		helmWait = "bootstrap only (Tau readiness validation still runs after queue policy)"
	}
	if spec.Wait {
		helmWait = "enabled"
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
  Mode:       %s
  Operators:  %s
  Helm wait:  %s
  Rollback:   %s
  Tau CRDs:   update from the selected chart before an existing-release upgrade; not rolled back by Helm
  Defaults:   tau-core-controller, TauCluster, baseline queue, quota admission guard, GPU monitoring, Portal
  Opt-in:     Stellar, lifecycle recorder, image prewarm
  Validation: Kubernetes >=1.30 and all required control-plane and Portal resources ready

`, spec.Release, spec.Namespace, spec.Chart, spec.Version, spec.Mode, operatorOwnershipSummary(spec.Mode), helmWait, rollback)
}

func runClusterInstallHelm(cmd *cobra.Command, spec clusterInstallSpec, out io.Writer, args []string) error {
	return runTauGridHelmCommand(cmd, spec.Chart, spec.Version, out, args)
}

func tauGridReleaseExists(cmd *cobra.Command, kubeContext, release, namespace string) (bool, error) {
	args := []string{
		"list",
		"--all-namespaces",
	}
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
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
		Status    string `json:"status"`
	}
	if err := json.Unmarshal(output.Bytes(), &releases); err != nil {
		return false, fmt.Errorf("decode Helm release list: %w", err)
	}
	var otherNamespaces []string
	foundInTarget := false
	for _, r := range releases {
		if r.Name != release {
			continue
		}
		if r.Status == "uninstalled" {
			continue
		}
		if r.Namespace == "" || r.Namespace == namespace {
			foundInTarget = true
			continue
		}
		otherNamespaces = append(otherNamespaces, r.Namespace)
	}
	if len(otherNamespaces) > 0 {
		sort.Strings(otherNamespaces)
		return false, fmt.Errorf(
			"Helm release %q already exists in namespace %s; a Helm release namespace cannot be changed in place: rerun with --namespace %s, or migrate and uninstall the existing release before installing it in %s",
			release,
			strings.Join(otherNamespaces, ", "),
			otherNamespaces[0],
			namespace,
		)
	}
	return foundInTarget, nil
}

func clusterInstallBootstrapArgs(spec clusterInstallSpec) []string {
	spec.Wait = true
	args := clusterInstallArgs(spec)
	return append(args, "--set", "baselineQueue.enabled=false")
}

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
	return appendHelmValueArgs(args, spec)
}

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
	managed := spec.Mode == operatorModeManaged
	args = append(args,
		"--set", "installation.mode="+spec.Mode,
		"--set", fmt.Sprintf("components.kueue.enabled=%t", !managed),
		"--set", fmt.Sprintf("components.kuberayOperator.enabled=%t", !managed),
	)
	return args
}

func operatorOwnershipSummary(mode string) string {
	if mode == operatorModeManaged {
		return "AKS microsoft.kueue and microsoft.kuberay extensions; TauGrid owns no operator CRDs, controllers, or webhooks"
	}
	return "TauGrid Helm release owns Kueue and KubeRay CRDs, controllers, webhooks, upgrades, and removal"
}

func externalOperatorComponents(mode string) map[installationcheck.Component]installationcheck.ExternalComponentOwner {
	if mode != operatorModeManaged {
		return nil
	}
	return map[installationcheck.Component]installationcheck.ExternalComponentOwner{
		installationcheck.ComponentKueue: {
			Release:               managedKueueRelease,
			Namespace:             managedKueueNamespace,
			CompatibleChartPrefix: supportedManagedKueueChart,
		},
		installationcheck.ComponentKubeRay: {
			Release:               managedKubeRayRelease,
			Namespace:             managedKubeRayNamespace,
			CompatibleChartPrefix: supportedManagedRayChart,
		},
	}
}
