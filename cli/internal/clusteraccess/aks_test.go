package clusteraccess

import (
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

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
	if exec == nil || exec.Command != "kubelogin" || strings.Join(exec.Args, " ") != "get-token --login interactive --server-id server" {
		t.Fatalf("normalized exec = %#v", exec)
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

func TestSetKubeloginModeAddsMissingMode(t *testing.T) {
	got := setKubeloginMode([]string{"get-token", "--server-id", "server"}, AuthModeDeviceCode)
	if strings.Join(got, " ") != "get-token --server-id server --login devicecode" {
		t.Fatalf("args = %v", got)
	}
}
