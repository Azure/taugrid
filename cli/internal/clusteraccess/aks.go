// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clusteraccess

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/containerservice/armcontainerservice/v9"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type AKSUserCredentialProvider struct {
	Credentials   CredentialFactory
	AuthMode      string
	KubeloginPath string
}

func (AKSUserCredentialProvider) Method() workspaceconnection.AccessMethod {
	return workspaceconnection.AccessMethodAKS
}

func (p AKSUserCredentialProvider) UserKubeconfig(ctx context.Context, descriptor workspaceconnection.Descriptor) ([]byte, error) {
	if descriptor.Access.AKS == nil {
		return nil, fmt.Errorf("AKS workspace access metadata is missing")
	}
	authorizationMode := descriptor.Authorization.Mode
	kubeloginPath := strings.TrimSpace(p.KubeloginPath)
	if kubeloginPath == "" && authorizationMode == workspaceconnection.AuthorizationModeWorkspaceRBAC {
		var err error
		kubeloginPath, err = exec.LookPath("kubelogin")
		if err != nil {
			return nil, fmt.Errorf("kubelogin is required for AKS user authentication; install it and retry: %w", err)
		}
	} else if kubeloginPath == "" {
		kubeloginPath, _ = exec.LookPath("kubelogin")
	}
	id, err := parseAKSResourceID(descriptor.Access.AKS.ResourceID)
	if err != nil {
		return nil, err
	}
	factory := p.Credentials
	if factory == nil {
		factory = UserCredentialFactory{Mode: p.AuthMode}
	}
	credential, err := factory.Credential(ctx, descriptor.Access.AKS.TenantID)
	if err != nil {
		return nil, err
	}
	clientFactory, err := armcontainerservice.NewClientFactory(id.SubscriptionID, credential, nil)
	if err != nil {
		return nil, fmt.Errorf("create AKS ARM client: %w", err)
	}
	result, err := clientFactory.NewManagedClustersClient().ListClusterUserCredentials(
		ctx,
		id.ResourceGroupName,
		id.Name,
		&armcontainerservice.ManagedClustersClientListClusterUserCredentialsOptions{
			Format: to.Ptr(armcontainerservice.FormatExec),
		},
	)
	if err != nil {
		return nil, fmt.Errorf("list AKS cluster-user credentials for %s: %w", id.Name, err)
	}
	if len(result.Kubeconfigs) == 0 || result.Kubeconfigs[0] == nil || len(result.Kubeconfigs[0].Value) == 0 {
		return nil, fmt.Errorf("AKS returned no cluster-user kubeconfig for %s", id.Name)
	}
	mode := strings.ToLower(strings.TrimSpace(p.AuthMode))
	if mode == "" {
		mode = AuthModeInteractive
	}
	return NormalizeKubeconfig(
		result.Kubeconfigs[0].Value,
		descriptor.Cluster.ContextName,
		authorizationMode,
		mode,
		kubeloginPath,
	)
}

func parseAKSResourceID(resourceID string) (*arm.ResourceID, error) {
	id, err := arm.ParseResourceID(strings.TrimSpace(resourceID))
	if err != nil {
		return nil, fmt.Errorf("parse AKS resource ID: %w", err)
	}
	if !strings.EqualFold(id.ResourceType.Namespace, "Microsoft.ContainerService") ||
		!strings.EqualFold(id.ResourceType.Type, "managedClusters") ||
		id.SubscriptionID == "" || id.ResourceGroupName == "" || id.Name == "" {
		return nil, fmt.Errorf("resource ID does not identify an AKS managed cluster: %q", resourceID)
	}
	return id, nil
}

func NormalizeKubeconfig(raw []byte, contextName, authorizationMode, authMode, kubeloginPath string) ([]byte, error) {
	config, err := clientcmd.Load(raw)
	if err != nil {
		return nil, fmt.Errorf("parse AKS cluster-user kubeconfig: %w", err)
	}
	current, ok := config.Contexts[config.CurrentContext]
	if !ok || current == nil {
		return nil, fmt.Errorf("AKS cluster-user kubeconfig has no current context")
	}
	cluster, ok := config.Clusters[current.Cluster]
	if !ok || cluster == nil || strings.TrimSpace(cluster.Server) == "" {
		return nil, fmt.Errorf("AKS cluster-user kubeconfig has no API server")
	}
	authInfo, ok := config.AuthInfos[current.AuthInfo]
	if !ok || authInfo == nil {
		return nil, fmt.Errorf("AKS cluster-user kubeconfig has no user authentication")
	}
	authInfo = authInfo.DeepCopy()
	switch authorizationMode {
	case workspaceconnection.AuthorizationModeWorkspaceRBAC:
		if authInfo.Exec == nil {
			return nil, fmt.Errorf("AKS cluster-user kubeconfig is not Entra exec authentication")
		}
		if hasStaticCredentials(authInfo) {
			return nil, fmt.Errorf("AKS cluster-user kubeconfig unexpectedly contains static credentials")
		}
		if !strings.EqualFold(filepath.Base(authInfo.Exec.Command), "kubelogin") {
			return nil, fmt.Errorf("AKS cluster-user kubeconfig exec command %q is not kubelogin", authInfo.Exec.Command)
		}
		switch authMode {
		case AuthModeInteractive, AuthModeDeviceCode:
		default:
			return nil, fmt.Errorf("unsupported kubelogin mode %q", authMode)
		}
		authInfo.Exec.Command = "kubelogin"
		authInfo.Exec.Args = setKubeloginMode(authInfo.Exec.Args, authMode)
	case workspaceconnection.AuthorizationModeClusterWide:
		if authInfo.Exec != nil {
			if hasStaticCredentials(authInfo) {
				return nil, fmt.Errorf("AKS cluster-user kubeconfig mixes exec and static credentials")
			}
			if !strings.EqualFold(filepath.Base(authInfo.Exec.Command), "kubelogin") {
				return nil, fmt.Errorf("AKS cluster-user kubeconfig exec command %q is not kubelogin", authInfo.Exec.Command)
			}
			if kubeloginPath == "" {
				return nil, fmt.Errorf("kubelogin is required for this AKS cluster-user kubeconfig")
			}
			authInfo.Exec.Command = "kubelogin"
			authInfo.Exec.Args = setKubeloginMode(authInfo.Exec.Args, authMode)
		} else if !hasStaticCredentials(authInfo) {
			return nil, fmt.Errorf("AKS cluster-user kubeconfig has no usable static or exec credential")
		}
	default:
		return nil, fmt.Errorf("unsupported workspace authorization mode %q", authorizationMode)
	}

	normalized := clientcmdapi.NewConfig()
	normalized.CurrentContext = contextName
	normalized.Clusters[contextName] = cluster.DeepCopy()
	normalized.AuthInfos[contextName] = authInfo
	normalized.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:  contextName,
		AuthInfo: contextName,
	}
	output, err := clientcmd.Write(*normalized)
	if err != nil {
		return nil, fmt.Errorf("write isolated AKS kubeconfig: %w", err)
	}
	return output, nil
}

func hasStaticCredentials(authInfo *clientcmdapi.AuthInfo) bool {
	return len(authInfo.ClientCertificateData) > 0 ||
		len(authInfo.ClientKeyData) > 0 ||
		authInfo.Token != "" ||
		authInfo.TokenFile != "" ||
		authInfo.Username != "" ||
		authInfo.Password != ""
}

func setKubeloginMode(args []string, mode string) []string {
	out := append([]string(nil), args...)
	for i, arg := range out {
		switch {
		case arg == "-l" || arg == "--login":
			if i+1 < len(out) {
				out[i+1] = mode
				return out
			}
		case strings.HasPrefix(arg, "--login="):
			out[i] = "--login=" + mode
			return out
		}
	}
	return append(out, "--login", mode)
}
