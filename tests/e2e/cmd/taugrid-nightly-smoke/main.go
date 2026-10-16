// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

func main() {
	opts := options{}
	flag.StringVar(&opts.tauBinary, "tau", filepath.FromSlash("../../cli/bin/tau"), "path to the Tau CLI")
	flag.StringVar(&opts.contextName, "context", "unbounded-stable", "kubeconfig context")
	flag.StringVar(&opts.systemNamespace, "system-namespace", "tau-system", "TauGrid system namespace")
	flag.StringVar(&opts.workspace, "workspace", "taugrid-default", "Tau workspace name")
	flag.StringVar(&opts.profile, "profile", "unbounded.cpu.topology-smoke", "Tau workload profile name")
	flag.StringVar(&opts.cpuFlavor, "cpu-flavor", "taugrid-default-cpu", "expected CPU ResourceFlavor")
	flag.StringVar(&opts.queue, "queue", "jobqueue", "expected admitting ClusterQueue")
	flag.StringVar(&opts.principalName, "principal-name", "nightly-test-researcher", "workspace principal name")
	flag.StringVar(&opts.sourceDir, "source-dir", "tau-native-nightly", "source workload directory")
	flag.StringVar(&opts.artifactDir, "artifact-dir", "", "artifact output directory")
	flag.StringVar(&opts.runName, "run-name", "", "Tau run name")
	flag.DurationVar(&opts.profileTimeout, "profile-timeout", 2*time.Minute, "profile readiness timeout")
	flag.DurationVar(&opts.workspaceTimeout, "workspace-timeout", 2*time.Minute, "workspace readiness timeout")
	flag.DurationVar(&opts.topologyTimeout, "topology-timeout", 5*time.Minute, "topology assignment timeout")
	flag.DurationVar(&opts.lifecycleTimeout, "lifecycle-timeout", 15*time.Minute, "Tau lifecycle timeout")
	flag.DurationVar(&opts.logsTimeout, "logs-timeout", 20*time.Minute, "Tau log capture timeout")
	flag.DurationVar(&opts.cleanupTimeout, "cleanup-timeout", 5*time.Minute, "RayJob cleanup timeout")
	flag.DurationVar(&opts.pollInterval, "poll-interval", 2*time.Second, "Kubernetes and Tau polling interval")
	flag.Parse()

	if err := opts.validate(); err != nil {
		fmt.Fprintf(os.Stderr, "invalid arguments: %v\n", err)
		os.Exit(2)
	}

	dynamicClient, err := buildDynamicClient(opts.contextName)
	if err != nil {
		fmt.Fprintf(os.Stderr, "creating Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner := newRunner(opts, osCommandExecutor{}, newKubernetesClient(dynamicClient, opts.pollInterval))
	if err := runner.execute(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "TauGrid unbounded-stable Tau-native smoke failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("TauGrid unbounded-stable Tau-native smoke passed")
}

func buildDynamicClient(contextName string) (dynamic.Interface, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("loading kubeconfig: %w", err)
	}
	client, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}
	return client, nil
}
