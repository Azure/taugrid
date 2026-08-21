// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package kube centralizes Tau's Kubernetes client identity and execution.
//
// Tau intentionally shells out to kubectl for its normal manifest and query
// paths because:
//
//   - kubectl is the contract operators already trust; mirroring its
//     behaviour (--context, --dry-run=server, --output) keeps surprises low.
//   - server-side apply and discovery behavior stay aligned with the operator's
//     installed kubectl.
//
// RESTConfig exists for narrow API semantics kubectl does not expose, such as
// UID-preconditioned deletion. Both paths resolve the same context,
// kubeconfig, and impersonation identity.
package kube

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Runner runs kubectl with a fixed context (empty = current-context).
type Runner struct {
	Context    string
	Kubeconfig string
	Path       string // kubectl binary; empty = "kubectl" on PATH
}

// New returns a Runner using "kubectl" from PATH.
func New(kubeContext string) *Runner {
	return &Runner{Context: kubeContext}
}

func NewWithKubeconfig(kubeContext, kubeconfig string) *Runner {
	return &Runner{Context: kubeContext, Kubeconfig: kubeconfig}
}

// RESTConfig resolves the same kubeconfig and context used by kubectl calls.
// Callers that need API behavior kubectl does not expose, such as UID
// preconditions on deletion, can therefore preserve the exact same identity.
func (r *Runner) RESTConfig() (*rest.Config, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if r.Kubeconfig != "" {
		rules.ExplicitPath = r.Kubeconfig
	}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: r.Context}
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
}

func (r *Runner) bin() string {
	if r.Path != "" {
		return r.Path
	}
	return "kubectl"
}

func (r *Runner) baseArgs() []string {
	var args []string
	if r.Kubeconfig != "" {
		args = append(args, "--kubeconfig", r.Kubeconfig)
	}
	if r.Context != "" {
		args = append(args, "--context", r.Context)
	}
	return args
}

// Apply runs `kubectl apply -f <path>` for each path. dryRun is one of
// "", "client", "server".
// ApplyOptions controls how Apply invokes kubectl.
//
// DryRun: "", "client", or "server".
// ServerSide: use --server-side (avoids the client-side 3-way merge that
// fails with `resourceVersion: 0: must be specified for an update` when
// objects were created by a different field manager, e.g. restored from a
// snapshot).
// ForceConflicts: only meaningful with ServerSide=true; passes
// --force-conflicts so server-side apply takes ownership of fields
// currently owned by another manager.
type ApplyOptions struct {
	DryRun         string
	ServerSide     bool
	ForceConflicts bool
	FieldManager   string
}

func (r *Runner) Apply(ctx context.Context, paths []string, opts ApplyOptions) (string, error) {
	if len(paths) == 0 {
		return "", fmt.Errorf("no paths to apply")
	}
	args := r.baseArgs()
	args = append(args, "apply")
	for _, p := range paths {
		args = append(args, "-f", p)
	}
	if opts.ServerSide {
		args = append(args, "--server-side")
		if opts.ForceConflicts {
			args = append(args, "--force-conflicts")
		}
		if opts.FieldManager != "" {
			args = append(args, "--field-manager="+opts.FieldManager)
		}
	}
	if opts.DryRun != "" {
		args = append(args, "--dry-run="+opts.DryRun)
	}
	return r.run(ctx, args, nil)
}

// Get runs `kubectl get <kind> -o <output>`. allNamespaces=true adds -A.
func (r *Runner) Get(ctx context.Context, kind, output string, allNamespaces bool) (string, error) {
	args := r.baseArgs()
	args = append(args, "get", kind)
	if allNamespaces {
		args = append(args, "-A")
	}
	if output != "" {
		args = append(args, "-o", output)
	}
	return r.run(ctx, args, nil)
}

// Raw runs kubectl with arbitrary args (context is prepended automatically).
func (r *Runner) Raw(ctx context.Context, extraArgs []string, stdin []byte) (string, error) {
	args := append(r.baseArgs(), extraArgs...)
	return r.run(ctx, args, stdin)
}

// ExecInteractive runs `kubectl exec` with the process's stdin/stdout/stderr
// connected directly to the user's TTY. This is the only way `kubectl exec -it`
// works correctly — capturing into buffers breaks the shell. Blocks until the
// user exits the remote shell.
func (r *Runner) ExecInteractive(ctx context.Context, extraArgs []string) error {
	args := append(r.baseArgs(), extraArgs...)
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (r *Runner) run(ctx context.Context, args []string, stdin []byte) (string, error) {
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("kubectl %v: %w: %s", args, err, stderr.String())
	}
	return stdout.String(), nil
}
