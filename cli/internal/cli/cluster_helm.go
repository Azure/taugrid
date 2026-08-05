package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"

	"github.com/spf13/cobra"
)

// helmListAllStateFlags enumerates the release states that Helm 3's `helm list --all`
// aggregated. Helm 4 removed `--all` from `helm list` but kept every individual state
// flag, so passing them explicitly works on both major versions without branching.
var helmListAllStateFlags = []string{
	"--deployed",
	"--failed",
	"--pending",
	"--superseded",
	"--uninstalled",
	"--uninstalling",
}

type helmCommandRunner func(context.Context, io.Reader, io.Writer, io.Writer, []string) error

var runHelmCommand helmCommandRunner = executeHelmCommand

// helmSupportsForceConflicts reports whether the installed Helm accepts
// --force-conflicts. Indirected so tests can answer it without a Helm on PATH.
var helmSupportsForceConflicts = sync.OnceValue(func() bool {
	return probeHelmForceConflicts(context.Background())
})

// helmSupportsRollbackOnFailure reports whether Helm uses the v4
// --rollback-on-failure spelling instead of Helm 3's --atomic spelling.
var helmSupportsRollbackOnFailure = sync.OnceValue(func() bool {
	return probeHelmRollbackOnFailure(context.Background())
})

// probeHelmForceConflicts asks Helm which flags it accepts rather than parsing
// `helm version`. Version strings carry build suffixes and prereleases, while
// the flag's presence is the property that actually matters; `upgrade --help`
// is offline and touches no cluster.
//
// It runs Helm directly rather than through runHelmCommand: this is a local
// capability query, not an action on a release, and threading it through the
// command runner would put a `helm upgrade --help` in the middle of every
// caller's observable Helm sequence.
//
// Fails closed: Helm 3 rejects an unknown flag outright, so a probe that cannot
// run must not add one.
func probeHelmForceConflicts(ctx context.Context) bool {
	helm, err := exec.LookPath("helm")
	if err != nil {
		return false
	}
	out, err := exec.CommandContext(ctx, helm, "upgrade", "--help").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "--force-conflicts")
}

func probeHelmRollbackOnFailure(ctx context.Context) bool {
	helm, err := exec.LookPath("helm")
	if err != nil {
		return false
	}
	out, err := exec.CommandContext(ctx, helm, "upgrade", "--help").Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "--rollback-on-failure")
}

// appendForceConflicts lets Helm's server-side apply proceed when another field
// manager already owns part of an object this release declares.
//
// On AKS the admissionsenforcer addon takes ownership of
// .webhooks[*].namespaceSelector on Kueue's webhook configurations to keep
// third-party webhooks away from system namespaces. Helm 4 defaults to
// server-side apply, so every upgrade of the release then fails on those
// fields. Forcing does not overwrite what AKS put there — the chart declares no
// value for those selectors, so the conflict is over the signature, not the
// content. Helm 3 never sees this because client-side apply ignores
// managedFields entirely.
//
// --server-side=true is sent with it because the two are not independent: Helm
// rejects "forceConflicts enabled when serverSideApply disabled", and the
// default --server-side=auto inherits whichever method the previous release
// used. A release that was ever applied client-side would otherwise turn this
// into a hard error instead of a fix.
func appendForceConflicts(args []string) []string {
	if !helmSupportsForceConflicts() {
		return args
	}
	return append(args, "--server-side=true", "--force-conflicts")
}

// appendRollbackOnFailure preserves Tau's --atomic compatibility flag while
// using the native spelling of the installed Helm major version.
func appendRollbackOnFailure(args []string) []string {
	if helmSupportsRollbackOnFailure() {
		return append(args, "--rollback-on-failure")
	}
	return append(args, "--atomic")
}

func runTauGridHelmCommand(cmd *cobra.Command, chart, version string, out io.Writer, args []string) error {
	var diagnostics bytes.Buffer
	err := runHelmCommand(
		cmd.Context(),
		cmd.InOrStdin(),
		out,
		io.MultiWriter(cmd.ErrOrStderr(), &diagnostics),
		args,
	)
	return wrapTauGridChartError(chart, version, diagnostics.String(), err)
}

func wrapTauGridChartError(chart, version, diagnostics string, err error) error {
	if err == nil || chart != defaultTauGridChart || !isHelmChartResolutionError(diagnostics, err) {
		return err
	}
	return fmt.Errorf(
		"cannot resolve default TauGrid OCI chart %s version %s; verify that this version is published and Helm can authenticate to aksairuntime.azurecr.io, or pass --chart <reference-or-local-path>: %w",
		chart,
		version,
		err,
	)
}

func isHelmChartResolutionError(diagnostics string, err error) bool {
	message := strings.ToLower(diagnostics + "\n" + err.Error())
	for _, signal := range []string{
		"fetchreference",
		"pull access denied",
		"manifest unknown",
		"manifest_unknown",
		"repository not found",
		"repository does not exist",
	} {
		if strings.Contains(message, signal) {
			return true
		}
	}

	registryContext := strings.Contains(message, "aksairuntime.azurecr.io") ||
		strings.Contains(message, "oci://") ||
		strings.Contains(message, "registry")
	if !registryContext {
		return false
	}
	for _, signal := range []string{
		"failed to authorize",
		"authorization failed",
		"authentication required",
		"login required",
		"unauthorized",
	} {
		if strings.Contains(message, signal) {
			return true
		}
	}
	return false
}

// appendKubeContext adds --kube-context only when one was resolved, so commands
// keep following kubectl's current context when the flag is left empty.
func appendKubeContext(args []string, kubeContext string) []string {
	if kubeContext == "" {
		return args
	}
	return append(args, "--kube-context", kubeContext)
}

// tauGridReleaseValues returns the release's coalesced Helm values as JSON.
// Callers decode only the keys they need; every one of them degrades to a
// default rather than aborting, because a release that cannot be read must
// never be more fatal than a release that is simply configured differently.
func tauGridReleaseValues(cmd *cobra.Command, kubeContext, release, namespace string) ([]byte, error) {
	args := appendKubeContext(
		[]string{"get", "values", release, "--namespace", namespace, "--all", "--output", "json"},
		kubeContext,
	)
	var values bytes.Buffer
	if err := runHelmCommand(cmd.Context(), cmd.InOrStdin(), &values, io.Discard, args); err != nil {
		return nil, err
	}
	return values.Bytes(), nil
}

// detectedHelmVersion returns `helm version --short` output, or "" if it cannot be
// determined. Used only to enrich error messages, never to gate behaviour.
func detectedHelmVersion(ctx context.Context, helm string) string {
	out, err := exec.CommandContext(ctx, helm, "version", "--short").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func executeHelmCommand(ctx context.Context, in io.Reader, out, errOut io.Writer, args []string) error {
	helm, err := exec.LookPath("helm")
	if err != nil {
		return fmt.Errorf("Helm is required: install Helm 3 or 4 and ensure `helm` is on PATH")
	}
	command := exec.CommandContext(ctx, helm, args...)
	command.Stdin = in
	command.Stdout = out
	command.Stderr = errOut
	if err := command.Run(); err != nil {
		if version := detectedHelmVersion(ctx, helm); version != "" {
			return fmt.Errorf("helm %s failed (using %s at %s): %w", args[0], version, helm, err)
		}
		return fmt.Errorf("helm %s failed: %w", args[0], err)
	}
	return nil
}
