// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clusteraccess

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type recordingCredentialFactory struct {
	ctx      context.Context
	tenantID string
	err      error
}

type callerContextKey struct{}

func (f *recordingCredentialFactory) Credential(ctx context.Context, tenantID string) (CredentialResult, error) {
	f.ctx = ctx
	f.tenantID = tenantID
	return CredentialResult{}, f.err
}

func TestAKSUserCredentialProviderUsesCallerContextAndTenant(t *testing.T) {
	wantErr := errors.New("credential unavailable")
	factory := &recordingCredentialFactory{err: wantErr}
	ctx := context.WithValue(context.Background(), callerContextKey{}, "caller")
	_, err := (AKSUserCredentialProvider{Credentials: factory}).UserKubeconfig(ctx, workspaceconnection.Descriptor{
		Cluster: workspaceconnection.ClusterDescriptor{ContextName: "aks"},
		Access: workspaceconnection.AccessDescriptor{
			Method: workspaceconnection.AccessMethodAKS,
			AKS: &workspaceconnection.AKSAccessDescriptor{
				ResourceID: "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.ContainerService/managedClusters/aks",
				TenantID:   "11111111-1111-1111-1111-111111111111",
			},
		},
		Authorization: workspaceconnection.AuthorizationDescriptor{
			Mode: workspaceconnection.AuthorizationModeClusterWide,
		},
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("UserKubeconfig() error = %v, want %v", err, wantErr)
	}
	if factory.ctx != ctx || factory.tenantID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("credential request used context %v and tenant %q", factory.ctx, factory.tenantID)
	}
}

func TestResolveKubeloginPathRequiresCompatibleBinary(t *testing.T) {
	provider := AKSUserCredentialProvider{
		FindExecutable: func(string) (string, error) {
			return "", errors.New("not found")
		},
	}
	_, err := provider.resolveKubeloginPath(true)
	if err == nil ||
		!strings.Contains(err.Error(), "kubelogin 0.1.7 or newer is required") ||
		!strings.Contains(err.Error(), "install or upgrade") {
		t.Fatalf("resolveKubeloginPath() error = %v", err)
	}
}

func TestRequireCompatibleKubelogin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		path    string
		output  string
		wantErr string
	}{
		{
			name:    "missing",
			wantErr: "kubelogin 0.1.7 or newer is required",
		},
		{
			name:    "old",
			path:    "/usr/local/bin/kubelogin",
			output:  "kubelogin version\ngit hash: v0.1.6/deadbeef\nGo version: go1.25.9\n",
			wantErr: "kubelogin 0.1.6 is unsupported",
		},
		{
			name:   "minimum",
			path:   "/usr/local/bin/kubelogin",
			output: "kubelogin version v0.1.7\n",
		},
		{
			name:   "newer",
			path:   "/usr/local/bin/kubelogin",
			output: "kubelogin version\ngit hash: v0.2.17/dff9ca0\nGo version: go1.25.9\n",
		},
		{
			name:    "unparseable",
			path:    "/usr/local/bin/kubelogin",
			output:  "kubelogin version\nGo version: go1.25.9\n",
			wantErr: "could not determine kubelogin version",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider := AKSUserCredentialProvider{
				KubeloginVersion: func(context.Context, string) (string, error) {
					return tc.output, nil
				},
			}
			err := provider.requireCompatibleKubelogin(context.Background(), tc.path)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("requireCompatibleKubelogin: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("requireCompatibleKubelogin() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestParseAKSResourceID(t *testing.T) {
	id, err := parseAKSResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg-ai/providers/Microsoft.ContainerService/managedClusters/aks-flex")
	if err != nil {
		t.Fatal(err)
	}
	if id.SubscriptionID == "" || id.ResourceGroupName != "rg-ai" || id.Name != "aks-flex" {
		t.Fatalf("resource ID = %#v", id)
	}
	if _, err := parseAKSResourceID("/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/nope"); err == nil {
		t.Fatalf("non-AKS resource ID was accepted")
	}
}

func TestNormalizeKubeconfigKeepsOnlyUserExecContext(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "clusterUser_rg_aks"
	config.Clusters["aks"] = &clientcmdapi.Cluster{Server: "https://aks.example.test", CertificateAuthorityData: []byte("ca")}
	config.AuthInfos["user"] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{
		Command: "kubelogin",
		Args:    []string{"get-token", "--login", "azurecli", "--server-id", "server"},
	}}
	config.Contexts[config.CurrentContext] = &clientcmdapi.Context{Cluster: "aks", AuthInfo: "user"}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}

	normalized, err := NormalizeKubeconfig(
		raw,
		"taugrid-flex",
		workspaceconnection.AuthorizationModeWorkspaceRBAC,
		AuthModeInteractive,
		"/usr/local/bin/kubelogin",
	)
	if err != nil {
		t.Fatalf("NormalizeKubeconfig: %v", err)
	}
	got, err := clientcmd.Load(normalized)
	if err != nil {
		t.Fatal(err)
	}
	if got.CurrentContext != "taugrid-flex" || len(got.Contexts) != 1 || len(got.Clusters) != 1 || len(got.AuthInfos) != 1 {
		t.Fatalf("normalized kubeconfig = %#v", got)
	}
	exec := got.AuthInfos["taugrid-flex"].Exec
	if exec == nil ||
		exec.Command != "/usr/local/bin/kubelogin" ||
		strings.Join(exec.Args, " ") != "get-token --login interactive --server-id server --disable-environment-override" {
		t.Fatalf("normalized exec = %#v", exec)
	}
}

func TestNormalizeKubeconfigReusesAzureCLILogin(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "clusterUser_rg_aks"
	config.Clusters["aks"] = &clientcmdapi.Cluster{Server: "https://aks.example.test"}
	config.AuthInfos["user"] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{
		Command: "kubelogin",
		Args:    []string{"get-token", "--login", "interactive", "--server-id", "server"},
	}}
	config.Contexts[config.CurrentContext] = &clientcmdapi.Context{Cluster: "aks", AuthInfo: "user"}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}

	normalized, err := NormalizeKubeconfig(
		raw,
		"taugrid-flex",
		workspaceconnection.AuthorizationModeWorkspaceRBAC,
		AuthModeAzureCLI,
		"/usr/local/bin/kubelogin",
	)
	if err != nil {
		t.Fatalf("NormalizeKubeconfig: %v", err)
	}
	got, err := clientcmd.Load(normalized)
	if err != nil {
		t.Fatal(err)
	}
	exec := got.AuthInfos["taugrid-flex"].Exec
	if exec == nil ||
		strings.Join(exec.Args, " ") != "get-token --login azurecli --server-id server --disable-environment-override" {
		t.Fatalf("normalized exec = %#v", exec)
	}
}

func TestNormalizeKubeconfigDisablesEnvironmentOverridesForEveryAuthMode(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "clusterUser_rg_aks"
	config.Clusters["aks"] = &clientcmdapi.Cluster{Server: "https://aks.example.test"}
	config.AuthInfos["user"] = &clientcmdapi.AuthInfo{Exec: &clientcmdapi.ExecConfig{
		Command: "kubelogin",
		Args: []string{
			"get-token",
			"--login",
			"stale",
			"--disable-environment-override=false",
		},
	}}
	config.Contexts[config.CurrentContext] = &clientcmdapi.Context{Cluster: "aks", AuthInfo: "user"}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}

	for _, authorizationMode := range []string{
		workspaceconnection.AuthorizationModeWorkspaceRBAC,
		workspaceconnection.AuthorizationModeClusterWide,
	} {
		for _, authMode := range []string{
			AuthModeAzureCLI,
			AuthModeInteractive,
			AuthModeDeviceCode,
		} {
			t.Run(authorizationMode+"/"+authMode, func(t *testing.T) {
				normalized, err := NormalizeKubeconfig(
					raw,
					"taugrid-flex",
					authorizationMode,
					authMode,
					"/usr/local/bin/kubelogin",
				)
				if err != nil {
					t.Fatalf("NormalizeKubeconfig: %v", err)
				}
				got, err := clientcmd.Load(normalized)
				if err != nil {
					t.Fatal(err)
				}
				exec := got.AuthInfos["taugrid-flex"].Exec
				want := "get-token --login " + authMode + " --disable-environment-override"
				if exec == nil || strings.Join(exec.Args, " ") != want {
					t.Fatalf("normalized exec = %#v, want args %q", exec, want)
				}
			})
		}
	}
}

func TestNormalizeKubeconfigRejectsStaticCredential(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "admin"
	config.Clusters["aks"] = &clientcmdapi.Cluster{Server: "https://aks.example.test"}
	config.AuthInfos["admin"] = &clientcmdapi.AuthInfo{ClientCertificateData: []byte("certificate"), ClientKeyData: []byte("key")}
	config.Contexts["admin"] = &clientcmdapi.Context{Cluster: "aks", AuthInfo: "admin"}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = NormalizeKubeconfig(
		raw,
		"aks",
		workspaceconnection.AuthorizationModeWorkspaceRBAC,
		AuthModeInteractive,
		"kubelogin",
	)
	if err == nil || !strings.Contains(err.Error(), "not Entra exec") {
		t.Fatalf("expected static credential rejection, got %v", err)
	}

}

func TestNormalizeKubeconfigAcceptsStaticCredentialOnlyForClusterWideMode(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "clusterUser"
	config.Clusters["aks"] = &clientcmdapi.Cluster{Server: "https://aks.example.test"}
	config.AuthInfos["user"] = &clientcmdapi.AuthInfo{
		ClientCertificateData: []byte("certificate"),
		ClientKeyData:         []byte("key"),
		Token:                 "legacy-cluster-user-token",
	}
	config.Contexts[config.CurrentContext] = &clientcmdapi.Context{Cluster: "aks", AuthInfo: "user"}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := NormalizeKubeconfig(
		raw,
		"aks-cluster-wide",
		workspaceconnection.AuthorizationModeClusterWide,
		AuthModeInteractive,
		"",
	)
	if err != nil {
		t.Fatalf("cluster-wide static credential rejected: %v", err)
	}
	got, err := clientcmd.Load(normalized)
	if err != nil {
		t.Fatal(err)
	}
	auth := got.AuthInfos["aks-cluster-wide"]
	if auth == nil || len(auth.ClientCertificateData) == 0 || auth.Token == "" || auth.Exec != nil {
		t.Fatalf("normalized static auth = %#v", auth)
	}
	_, err = NormalizeKubeconfig(
		raw,
		"aks-workspace",
		workspaceconnection.AuthorizationModeWorkspaceRBAC,
		AuthModeInteractive,
		"kubelogin",
	)
	if err == nil || !strings.Contains(err.Error(), "not Entra exec") {
		t.Fatalf("workspace-rbac accepted static credentials: %v", err)
	}
}

func TestNormalizeKubeloginArgsAddsMissingSecurityFlags(t *testing.T) {
	got := normalizeKubeloginArgs([]string{"get-token", "--server-id", "server"}, AuthModeDeviceCode)
	if strings.Join(got, " ") != "get-token --server-id server --login devicecode --disable-environment-override" {
		t.Fatalf("args = %v", got)
	}
}
