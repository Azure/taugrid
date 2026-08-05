package kvspec

import (
	"github.com/Azure/taugrid/core/workloadmeta"
	"strings"
	"testing"
)

func TestRenderSPCBasic(t *testing.T) {
	spec := &Spec{
		Entries: []Entry{
			{EnvVar: "HF_TOKEN", SecretName: "hf-token"},
			{EnvVar: "WANDB_KEY", SecretName: "wandb-api-key"},
		},
		Vault:    "my-vault",
		TenantID: "tenant-123",
		ClientID: "client-456",
	}
	out, err := RenderSPC("tau-job-kv", "ray", spec)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"kind: SecretProviderClass",
		"name: tau-job-kv",
		"namespace: ray",
		"provider: azure",
		"secretName: tau-job-kv-sync",
		"keyvaultName: my-vault",
		"tenantId: tenant-123",
		"clientID: client-456",
		"objectName: hf-token",
		"objectName: wandb-api-key",
		"key: HF_TOKEN",
		"key: WANDB_KEY",
		workloadmeta.LabelManagedBy + ": tau",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in SPC output:\n%s", want, s)
		}
	}
}

func TestRenderSPCNil(t *testing.T) {
	out, err := RenderSPC("x", "ns", nil)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Error("expected nil for nil spec")
	}
}

func TestRenderSPCEmpty(t *testing.T) {
	spec := &Spec{Entries: nil}
	out, err := RenderSPC("x", "ns", spec)
	if err != nil {
		t.Fatal(err)
	}
	if out != nil {
		t.Error("expected nil for empty entries")
	}
}

func TestVolumeYAML(t *testing.T) {
	out := VolumeYAML("tau-job-kv", 8)
	if !strings.Contains(out, "secretProviderClass: tau-job-kv") {
		t.Errorf("missing secretProviderClass reference:\n%s", out)
	}
	if !strings.Contains(out, "secrets-store.csi.k8s.io") {
		t.Errorf("missing CSI driver:\n%s", out)
	}
	if !strings.Contains(out, "readOnly: true") {
		t.Errorf("missing readOnly:\n%s", out)
	}
	if !strings.HasPrefix(out, "        - name:") {
		t.Errorf("expected 8-space indent, got:\n%s", out)
	}
}

func TestVolumeMountYAML(t *testing.T) {
	out := VolumeMountYAML(12)
	if !strings.Contains(out, "/mnt/secrets-store") {
		t.Errorf("missing mountPath:\n%s", out)
	}
	if !strings.Contains(out, "readOnly: true") {
		t.Errorf("missing readOnly:\n%s", out)
	}
	if !strings.HasPrefix(out, "            - name:") {
		t.Errorf("expected 12-space indent, got:\n%s", out)
	}
}
