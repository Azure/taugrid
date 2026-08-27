// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package clusteraccess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

func TestKubeconfigProviderIsolatesNamedContext(t *testing.T) {
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "other"
	config.Clusters["wanted-cluster"] = &clientcmdapi.Cluster{Server: "https://wanted.example"}
	config.AuthInfos["wanted-user"] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion: "client.authentication.k8s.io/v1",
			Command:    "credential-plugin",
		},
	}
	config.Contexts["wanted"] = &clientcmdapi.Context{Cluster: "wanted-cluster", AuthInfo: "wanted-user", Namespace: "research"}
	config.Clusters["other-cluster"] = &clientcmdapi.Cluster{Server: "https://other.example"}
	config.AuthInfos["other-user"] = &clientcmdapi.AuthInfo{Token: "other-token"}
	config.Contexts["other"] = &clientcmdapi.Context{Cluster: "other-cluster", AuthInfo: "other-user"}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	provider := KubeconfigProvider{
		LoadingRules: &clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
	}
	gotRaw, err := provider.UserKubeconfig(workspaceconnection.Descriptor{
		Cluster: workspaceconnection.ClusterDescriptor{ContextName: "wanted"},
	})
	if err != nil {
		t.Fatalf("UserKubeconfig: %v", err)
	}
	got, err := clientcmd.Load(gotRaw)
	if err != nil {
		t.Fatalf("load isolated kubeconfig: %v", err)
	}
	if got.CurrentContext != "wanted" ||
		len(got.Contexts) != 1 ||
		len(got.Clusters) != 1 ||
		len(got.AuthInfos) != 1 ||
		got.Contexts["wanted"].Namespace != "research" {
		t.Fatalf("isolated kubeconfig = %#v", got)
	}
	if _, ok := got.Contexts["other"]; ok {
		t.Fatal("isolated kubeconfig retained unrelated context")
	}
}

func TestKubeconfigProviderPreservesCredentialFileReferences(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"ca.crt", "client.crt", "client.key", "token"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("sensitive-"+name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	config := clientcmdapi.NewConfig()
	config.CurrentContext = "wanted"
	config.Clusters["wanted-cluster"] = &clientcmdapi.Cluster{
		Server:               "https://wanted.example",
		CertificateAuthority: "ca.crt",
	}
	config.AuthInfos["wanted-user"] = &clientcmdapi.AuthInfo{
		ClientCertificate: "client.crt",
		ClientKey:         "client.key",
		TokenFile:         "token",
	}
	config.Contexts["wanted"] = &clientcmdapi.Context{
		Cluster:  "wanted-cluster",
		AuthInfo: "wanted-user",
	}
	raw, err := clientcmd.Write(*config)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	gotRaw, err := (KubeconfigProvider{
		LoadingRules: &clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
	}).UserKubeconfig(workspaceconnection.Descriptor{
		Cluster: workspaceconnection.ClusterDescriptor{ContextName: "wanted"},
	})
	if err != nil {
		t.Fatalf("UserKubeconfig: %v", err)
	}
	got, err := clientcmd.Load(gotRaw)
	if err != nil {
		t.Fatalf("load isolated kubeconfig: %v", err)
	}
	cluster := got.Clusters["wanted-cluster"]
	authInfo := got.AuthInfos["wanted-user"]
	if cluster.CertificateAuthority != filepath.Join(root, "ca.crt") ||
		authInfo.ClientCertificate != filepath.Join(root, "client.crt") ||
		authInfo.ClientKey != filepath.Join(root, "client.key") ||
		authInfo.TokenFile != filepath.Join(root, "token") {
		t.Fatalf("isolated paths were not resolved: cluster=%#v auth=%#v", cluster, authInfo)
	}
	if len(cluster.CertificateAuthorityData) != 0 ||
		len(authInfo.ClientCertificateData) != 0 ||
		len(authInfo.ClientKeyData) != 0 ||
		authInfo.Token != "" {
		t.Fatalf("isolated kubeconfig inlined credential data: cluster=%#v auth=%#v", cluster, authInfo)
	}
	for _, secret := range []string{"sensitive-client.key", "sensitive-token"} {
		if strings.Contains(string(gotRaw), secret) {
			t.Fatalf("isolated kubeconfig copied %q", secret)
		}
	}
}

func TestKubeconfigProviderRequiresNamedContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	provider := KubeconfigProvider{
		LoadingRules: &clientcmd.ClientConfigLoadingRules{ExplicitPath: path},
	}
	_, err := provider.UserKubeconfig(workspaceconnection.Descriptor{
		Cluster: workspaceconnection.ClusterDescriptor{ContextName: "missing"},
	})
	if err == nil || !strings.Contains(err.Error(), `context "missing" was not found`) {
		t.Fatalf("UserKubeconfig() error = %v", err)
	}
}
