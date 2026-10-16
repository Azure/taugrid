// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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

var helmUpgradeHelp = func(ctx context.Context) ([]byte, error) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, helm, "upgrade", "--help").Output()
}

var helmSupportsForceConflicts = sync.OnceValue(func() bool {
	return probeHelmForceConflicts(context.Background())
})

var helmSupportsRollbackOnFailure = sync.OnceValue(func() bool {
	return probeHelmRollbackOnFailure(context.Background())
})

func probeHelmForceConflicts(ctx context.Context) bool {
	out, err := helmUpgradeHelp(ctx)
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "--force-conflicts")
}

func probeHelmRollbackOnFailure(ctx context.Context) bool {
	out, err := helmUpgradeHelp(ctx)
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "--rollback-on-failure")
}

// third-party webhooks away from system namespaces. Helm 4 defaults to
func appendForceConflicts(args []string) []string {
	if !helmSupportsForceConflicts() {
		return args
	}
	return append(args, "--server-side=true", "--force-conflicts")
}

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
		"cannot resolve default TauGrid OCI chart %s version %s; verify that this version is published and Helm can access the registry, or pass --chart <reference-or-local-path>: %w",
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

	registryContext := strings.Contains(message, "oci://") ||
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

func appendKubeContext(args []string, kubeContext string) []string {
	if kubeContext == "" {
		return args
	}
	return append(args, "--kube-context", kubeContext)
}

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
