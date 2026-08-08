// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvspec

import (
	"strings"
	"testing"
)

func TestParseEntriesBareSecret(t *testing.T) {
	entries, err := ParseEntries(map[string]string{
		"HF_TOKEN": "hf-token",
	}, "my-vault")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(entries))
	}
	if entries[0].VaultName != "my-vault" {
		t.Errorf("vault = %q, want %q", entries[0].VaultName, "my-vault")
	}
	if entries[0].SecretName != "hf-token" {
		t.Errorf("secret = %q, want %q", entries[0].SecretName, "hf-token")
	}
}

func TestParseEntriesExplicitVault(t *testing.T) {
	entries, err := ParseEntries(map[string]string{
		"CUSTOM_KEY": "other-vault/custom-secret",
	}, "default-vault")
	if err != nil {
		t.Fatal(err)
	}
	if entries[0].VaultName != "other-vault" {
		t.Errorf("vault = %q, want %q", entries[0].VaultName, "other-vault")
	}
	if entries[0].SecretName != "custom-secret" {
		t.Errorf("secret = %q, want %q", entries[0].SecretName, "custom-secret")
	}
}

func TestParseEntriesBareSecretNoDefaultVaultFails(t *testing.T) {
	_, err := ParseEntries(map[string]string{
		"HF_TOKEN": "hf-token",
	}, "")
	if err == nil {
		t.Fatal("expected error for bare secret without default vault")
	}
	if !strings.Contains(err.Error(), "--key-vault") {
		t.Errorf("error should mention --key-vault; got: %v", err)
	}
}

func TestParseEntriesMultipleVaultsFails(t *testing.T) {
	_, err := ParseEntries(map[string]string{
		"A": "vault-a/secret-a",
		"B": "vault-b/secret-b",
	}, "")
	if err == nil {
		t.Fatal("expected error for multiple vaults")
	}
	if !strings.Contains(err.Error(), "multiple vaults") {
		t.Errorf("error should mention multiple vaults; got: %v", err)
	}
}

func TestParseEntriesInvalidEnvName(t *testing.T) {
	_, err := ParseEntries(map[string]string{
		"bad-name": "hf-token",
	}, "my-vault")
	if err == nil {
		t.Fatal("expected error for invalid env name")
	}
	if !strings.Contains(err.Error(), "invalid env var name") {
		t.Errorf("error should mention invalid name; got: %v", err)
	}
}

func TestParseEntriesEmptyRef(t *testing.T) {
	_, err := ParseEntries(map[string]string{
		"HF_TOKEN": "",
	}, "my-vault")
	if err == nil {
		t.Fatal("expected error for empty ref")
	}
}

func TestParseEntriesNilReturnsNil(t *testing.T) {
	entries, err := ParseEntries(nil, "vault")
	if err != nil {
		t.Fatal(err)
	}
	if entries != nil {
		t.Errorf("expected nil for nil input")
	}
}

func TestParseEntriesSorted(t *testing.T) {
	entries, err := ParseEntries(map[string]string{
		"Z_VAR": "z-secret",
		"A_VAR": "a-secret",
		"M_VAR": "m-secret",
	}, "vault")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(entries))
	}
	if entries[0].EnvVar != "A_VAR" || entries[1].EnvVar != "M_VAR" || entries[2].EnvVar != "Z_VAR" {
		t.Errorf("entries not sorted: %v, %v, %v", entries[0].EnvVar, entries[1].EnvVar, entries[2].EnvVar)
	}
}

func TestNewSpecMissingTenantID(t *testing.T) {
	entries := []Entry{{EnvVar: "X", VaultName: "v", SecretName: "s"}}
	_, err := NewSpec(entries, "", "client-id")
	if err == nil || !strings.Contains(err.Error(), "--tenant-id") {
		t.Fatalf("expected tenant-id error; got: %v", err)
	}
}

func TestNewSpecMissingClientID(t *testing.T) {
	entries := []Entry{{EnvVar: "X", VaultName: "v", SecretName: "s"}}
	_, err := NewSpec(entries, "tenant", "")
	if err == nil || !strings.Contains(err.Error(), "--workload-identity-client-id") {
		t.Fatalf("expected client-id error; got: %v", err)
	}
}

func TestNewSpecEmpty(t *testing.T) {
	spec, err := NewSpec(nil, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if spec != nil {
		t.Error("expected nil for no entries")
	}
}

func TestEnvVars(t *testing.T) {
	spec := &Spec{
		Entries: []Entry{
			{EnvVar: "HF_TOKEN", SecretName: "hf-token"},
			{EnvVar: "WANDB_KEY", SecretName: "wandb-key"},
		},
	}
	vars := spec.EnvVars("my-job-kv-sync")
	if len(vars) != 2 {
		t.Fatalf("got %d vars, want 2", len(vars))
	}
	if vars[0].Name != "HF_TOKEN" {
		t.Errorf("vars[0].Name = %q, want HF_TOKEN", vars[0].Name)
	}
	if vars[0].ValueFrom == nil || vars[0].ValueFrom.SecretKeyRef.Name != "my-job-kv-sync" {
		t.Errorf("vars[0] should reference my-job-kv-sync")
	}
	if vars[0].ValueFrom.SecretKeyRef.Key != "HF_TOKEN" {
		t.Errorf("vars[0] key should be HF_TOKEN, got %q", vars[0].ValueFrom.SecretKeyRef.Key)
	}
}

func TestSPCAndSyncNames(t *testing.T) {
	if got := SPCName("tau-my-job"); got != "tau-my-job-kv" {
		t.Errorf("SPCName = %q", got)
	}
	if got := SyncedSecretName("tau-my-job"); got != "tau-my-job-kv-sync" {
		t.Errorf("SyncedSecretName = %q", got)
	}
}
