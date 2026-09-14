// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

const mpiJobName = "e2e-nccl-rdma-2x8xh200"
const defaultDeleteTimeout = 20 * time.Second
const maxRESTRequestTimeout = 10 * time.Second

var mpiJobGVR = schema.GroupVersionResource{
	Group:    "kubeflow.org",
	Version:  "v2beta1",
	Resource: "mpijobs",
}

func main() {
	var kubeconfig string
	var contextName string
	var namespace string
	var uid string
	var timeout time.Duration

	flag.StringVar(&kubeconfig, "kubeconfig", "", "explicit kubeconfig path")
	flag.StringVar(&contextName, "context", "", "explicit kubeconfig context")
	flag.StringVar(&namespace, "namespace", "", "MPIJob namespace")
	flag.StringVar(&uid, "uid", "", "owned MPIJob UID precondition")
	flag.DurationVar(&timeout, "timeout", defaultDeleteTimeout, "overall delete deadline")
	flag.Parse()

	if kubeconfig == "" || contextName == "" || namespace == "" || uid == "" || timeout <= 0 {
		fmt.Fprintln(os.Stderr, "--kubeconfig, --context, --namespace, --uid, and a positive --timeout are required")
		os.Exit(2)
	}

	loadingRules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}
	overrides := &clientcmd.ConfigOverrides{CurrentContext: contextName}
	config, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides).ClientConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "load explicit Kubernetes client configuration: %v\n", err)
		os.Exit(1)
	}
	config.Timeout = boundedRESTTimeout(timeout)
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		fmt.Fprintf(os.Stderr, "build Kubernetes dynamic client: %v\n", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := deleteOwnedMPIJob(ctx, client, namespace, types.UID(uid), timeout); err != nil {
		fmt.Fprintf(os.Stderr, "delete owned MPIJob: %v\n", err)
		os.Exit(1)
	}
}

func deleteOwnedMPIJob(ctx context.Context, client dynamic.Interface, namespace string, uid types.UID, timeout time.Duration) error {
	if namespace == "" {
		return errors.New("namespace is required")
	}
	if uid == "" {
		return errors.New("UID precondition is required")
	}
	if timeout <= 0 {
		return errors.New("positive timeout is required")
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	foreground := metav1.DeletePropagationForeground
	return client.Resource(mpiJobGVR).Namespace(namespace).Delete(ctx, mpiJobName, metav1.DeleteOptions{
		PropagationPolicy: &foreground,
		Preconditions:     &metav1.Preconditions{UID: &uid},
	})
}

func boundedRESTTimeout(timeout time.Duration) time.Duration {
	if timeout < maxRESTRequestTimeout {
		return timeout
	}
	return maxRESTRequestTimeout
}
