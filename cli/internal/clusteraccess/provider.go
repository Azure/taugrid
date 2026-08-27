// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clusteraccess

import (
	"context"
	"fmt"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type Provider struct {
	AKS        AKSUserCredentialProvider
	Kubeconfig KubeconfigProvider
}

func (p Provider) UserKubeconfig(ctx context.Context, descriptor workspaceconnection.Descriptor) ([]byte, error) {
	switch descriptor.Access.Method {
	case workspaceconnection.AccessMethodAKS:
		return p.AKS.UserKubeconfig(ctx, descriptor)
	case workspaceconnection.AccessMethodKubeconfig:
		return p.Kubeconfig.UserKubeconfig(descriptor)
	default:
		return nil, fmt.Errorf("unsupported workspace access method %q", descriptor.Access.Method)
	}
}

type KubeconfigProvider struct {
	LoadingRules *clientcmd.ClientConfigLoadingRules
}

func (p KubeconfigProvider) UserKubeconfig(descriptor workspaceconnection.Descriptor) ([]byte, error) {
	rules := p.LoadingRules
	if rules == nil {
		rules = clientcmd.NewDefaultClientConfigLoadingRules()
	}
	config, err := rules.Load()
	if err != nil {
		return nil, fmt.Errorf("load Kubernetes configuration: %w", err)
	}
	contextName := descriptor.Cluster.ContextName
	kubeContext, ok := config.Contexts[contextName]
	if !ok {
		return nil, fmt.Errorf("Kubernetes context %q was not found in KUBECONFIG or the default kubeconfig", contextName)
	}
	cluster, ok := config.Clusters[kubeContext.Cluster]
	if !ok {
		return nil, fmt.Errorf("Kubernetes context %q references missing cluster %q", contextName, kubeContext.Cluster)
	}
	isolated := clientcmdapi.NewConfig()
	isolated.CurrentContext = contextName
	isolated.Contexts[contextName] = kubeContext.DeepCopy()
	isolated.Clusters[kubeContext.Cluster] = cluster.DeepCopy()
	if kubeContext.AuthInfo != "" {
		authInfo, ok := config.AuthInfos[kubeContext.AuthInfo]
		if !ok {
			return nil, fmt.Errorf("Kubernetes context %q references missing user %q", contextName, kubeContext.AuthInfo)
		}
		isolated.AuthInfos[kubeContext.AuthInfo] = authInfo.DeepCopy()
	}
	if err := clientcmd.ResolveLocalPaths(isolated); err != nil {
		return nil, fmt.Errorf("resolve isolated Kubernetes configuration paths: %w", err)
	}
	raw, err := clientcmd.Write(*isolated)
	if err != nil {
		return nil, fmt.Errorf("write isolated Kubernetes configuration: %w", err)
	}
	return raw, nil
}
