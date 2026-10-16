// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kvspec

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/envspec"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type Entry struct {
	EnvVar     string
	VaultName  string
	SecretName string
}

type Spec struct {
	Entries  []Entry
	Vault    string
	TenantID string
	ClientID string
}

func ParseEntries(envKV map[string]string, defaultVault string) ([]Entry, error) {
	if len(envKV) == 0 {
		return nil, nil
	}
	var entries []Entry
	vaults := map[string]bool{}

	for envVar, ref := range envKV {
		if !envNameRE.MatchString(envVar) {
			return nil, fmt.Errorf("env_kv key %q: invalid env var name (use C_IDENTIFIER format)", envVar)
		}
		vault, secret, err := parseRef(ref, defaultVault)
		if err != nil {
			return nil, fmt.Errorf("env_kv %s=%q: %w", envVar, ref, err)
		}
		entries = append(entries, Entry{
			EnvVar:     envVar,
			VaultName:  vault,
			SecretName: secret,
		})
		vaults[vault] = true
	}

	if len(vaults) > 1 {
		var names []string
		for v := range vaults {
			names = append(names, v)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("env_kv entries reference multiple vaults %v; all entries must use the same vault (one SecretProviderClass per workload)", names)
	}

	sort.Slice(entries, func(i, j int) bool {
		return entries[i].EnvVar < entries[j].EnvVar
	})
	return entries, nil
}

func parseRef(ref, defaultVault string) (string, string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", "", fmt.Errorf("empty secret reference")
	}

	parts := strings.SplitN(ref, "/", 2)
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0], parts[1], nil
	}

	if defaultVault == "" {
		return "", "", fmt.Errorf("bare secret name %q requires --key-vault or profile spec.secrets.keyVault", ref)
	}
	return defaultVault, ref, nil
}

func NewSpec(entries []Entry, tenantID, clientID string) (*Spec, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	if tenantID == "" {
		return nil, fmt.Errorf("env_kv requires --tenant-id or profile spec.secrets.tenantId")
	}
	if clientID == "" {
		return nil, fmt.Errorf("env_kv requires --workload-identity-client-id or profile spec.secrets.serviceAccount with WI annotation")
	}
	return &Spec{
		Entries:  entries,
		Vault:    entries[0].VaultName,
		TenantID: tenantID,
		ClientID: clientID,
	}, nil
}

func (s *Spec) EnvVars(syncedSecretName string) []envspec.Var {
	vars := make([]envspec.Var, len(s.Entries))
	for i, e := range s.Entries {
		vars[i] = envspec.Secret(e.EnvVar, syncedSecretName, e.EnvVar)
	}
	return vars
}

func SyncedSecretName(resourceName string) string {
	return resourceName + "-kv-sync"
}

func SPCName(resourceName string) string {
	return resourceName + "-kv"
}
