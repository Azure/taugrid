// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kube

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Runner struct {
	Context    string
	Kubeconfig string
	Path       string
}

func New(kubeContext string) *Runner {
	return &Runner{Context: kubeContext}
}

func NewWithKubeconfig(kubeContext, kubeconfig string) *Runner {
	return &Runner{Context: kubeContext, Kubeconfig: kubeconfig}
}

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

func (r *Runner) Raw(ctx context.Context, extraArgs []string, stdin []byte) (string, error) {
	args := append(r.baseArgs(), extraArgs...)
	return r.run(ctx, args, stdin)
}

func (r *Runner) RawStream(ctx context.Context, extraArgs []string, stdin []byte, out io.Writer) error {
	args := append(r.baseArgs(), extraArgs...)
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	if out == nil {
		out = io.Discard
	}
	var stderr bytes.Buffer
	cmd.Stdout = out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %v: %w: %s", args, err, stderr.String())
	}
	return nil
}

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
