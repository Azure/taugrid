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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultDeleteTimeout = 20 * time.Second
const maxRESTRequestTimeout = 10 * time.Second

func main() {
	var kubeconfig string
	var contextName string
	var namespace string
	var group string
	var version string
	var resource string
	var name string
	var uid string
	var timeout time.Duration

	flag.StringVar(&kubeconfig, "kubeconfig", "", "explicit kubeconfig path")
	flag.StringVar(&contextName, "context", "", "explicit kubeconfig context")
	flag.StringVar(&namespace, "namespace", "", "resource namespace")
	flag.StringVar(&group, "group", "", "API group; empty selects the core API")
	flag.StringVar(&version, "version", "", "API version")
	flag.StringVar(&resource, "resource", "", "plural API resource")
	flag.StringVar(&name, "name", "", "fixed resource name")
	flag.StringVar(&uid, "uid", "", "owned resource UID precondition")
	flag.DurationVar(&timeout, "timeout", defaultDeleteTimeout, "overall delete deadline")
	flag.Parse()

	if kubeconfig == "" || contextName == "" ||
		version == "" || resource == "" || name == "" || uid == "" || timeout <= 0 {
		fmt.Fprintln(os.Stderr, "--kubeconfig, --context, --version, --resource, --name, --uid, and a positive --timeout are required; --namespace is optional for cluster-scoped resources")
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
	gvr := schema.GroupVersionResource{Group: group, Version: version, Resource: resource}
	if err := deleteOwnedResource(ctx, client, gvr, namespace, name, types.UID(uid), timeout); err != nil {
		fmt.Fprintf(os.Stderr, "delete owned resource: %v\n", err)
		os.Exit(1)
	}
}

func deleteOwnedResource(
	ctx context.Context,
	client dynamic.Interface,
	gvr schema.GroupVersionResource,
	namespace, name string,
	uid types.UID,
	timeout time.Duration,
) error {
	if gvr.Version == "" || gvr.Resource == "" {
		return errors.New("resource version and plural resource are required")
	}
	if name == "" {
		return errors.New("name is required")
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
	namespaceable := client.Resource(gvr)
	var resourceClient dynamic.ResourceInterface = namespaceable
	if namespace != "" {
		resourceClient = namespaceable.Namespace(namespace)
	}
	err := resourceClient.Delete(ctx, name, metav1.DeleteOptions{
		PropagationPolicy: &foreground,
		Preconditions:     &metav1.Preconditions{UID: &uid},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

func boundedRESTTimeout(timeout time.Duration) time.Duration {
	if timeout < maxRESTRequestTimeout {
		return timeout
	}
	return maxRESTRequestTimeout
}
