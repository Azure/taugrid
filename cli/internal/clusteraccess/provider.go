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
	adapters map[workspaceconnection.AccessMethod]Adapter
	err      error
}

type Adapter interface {
	Method() workspaceconnection.AccessMethod
	UserKubeconfig(context.Context, workspaceconnection.Descriptor) ([]byte, error)
}

func NewProvider(adapters ...Adapter) Provider {
	provider := Provider{
		adapters: make(map[workspaceconnection.AccessMethod]Adapter, len(adapters)),
	}
	for _, adapter := range adapters {
		if adapter == nil {
			provider.err = fmt.Errorf("cluster credential adapter is nil")
			return provider
		}
		method := adapter.Method()
		if method == "" {
			provider.err = fmt.Errorf("cluster credential adapter has an empty access method")
			return provider
		}
		if _, exists := provider.adapters[method]; exists {
			provider.err = fmt.Errorf("multiple cluster credential adapters are configured for access method %q", method)
			return provider
		}
		provider.adapters[method] = adapter
	}
	return provider
}

func NewDefaultProvider(aks AKSUserCredentialProvider) Provider {
	return NewProvider(aks, KubeconfigProvider{})
}

func (p Provider) UserKubeconfig(ctx context.Context, descriptor workspaceconnection.Descriptor) ([]byte, error) {
	if p.err != nil {
		return nil, p.err
	}
	adapter, ok := p.adapters[descriptor.Access.Method]
	if !ok {
		return nil, fmt.Errorf("unsupported workspace access method %q", descriptor.Access.Method)
	}
	return adapter.UserKubeconfig(ctx, descriptor)
}

type KubeconfigProvider struct {
	LoadingRules *clientcmd.ClientConfigLoadingRules
}

func (KubeconfigProvider) Method() workspaceconnection.AccessMethod {
	return workspaceconnection.AccessMethodKubeconfig
}

func (p KubeconfigProvider) UserKubeconfig(_ context.Context, descriptor workspaceconnection.Descriptor) ([]byte, error) {
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
