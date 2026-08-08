// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package profile

import (
	"fmt"
	"github.com/Azure/taugrid/core/workloadmeta"
	"sort"
	"strings"
)

const (
	StorageRoleDurableData = "durable"
	StorageRoleHotScratch  = "hot"
	StorageRoleModelCache  = "model-cache"

	StorageTypeEmptyDir              = "empty-dir"
	storageTypeAzureContainerStorage = "azure-container-storage"
	storageTypeLocalNVMe             = "local-nvme"
)

// StorageContract captures the profile-level storage roles Tau can reason
// about without exposing provider-specific mount flags to researchers.
type StorageContract struct {
	Roles         []StorageRole
	Checkpointing StorageCheckpointing
}

type StorageRole struct {
	Role      string
	Type      string
	MountPath string
	Cache     string
	Fallback  string
	Scope     string
}

type StorageCheckpointing struct {
	Format string
}

// StorageContractFromProfile reads spec.resources.storage. Missing blocks are
// allowed for backwards compatibility and return ok=false.
func StorageContractFromProfile(p Profile) (StorageContract, bool, error) {
	res, ok := p.Spec["resources"].(map[string]any)
	if !ok {
		return StorageContract{}, false, nil
	}
	raw, ok := res["storage"]
	if !ok {
		return StorageContract{}, false, nil
	}
	block, ok := raw.(map[string]any)
	if !ok {
		return StorageContract{}, true, fmt.Errorf("spec.resources.storage must be a map")
	}

	contract := StorageContract{}
	seen := map[string]bool{}
	for key, value := range block {
		normalizedKey := normalizeStorageKey(key)
		if normalizedKey == "checkpointing" {
			cp, err := parseStorageCheckpointing(value)
			if err != nil {
				return StorageContract{}, true, fmt.Errorf("spec.resources.storage.%s: %w", key, err)
			}
			contract.Checkpointing = cp
			continue
		}
		role := normalizeStorageRole(normalizedKey)
		if role == "" {
			return StorageContract{}, true, fmt.Errorf("spec.resources.storage.%s: unknown role (allowed: durable, hot, modelCache, checkpointing)", key)
		}
		if seen[role] {
			return StorageContract{}, true, fmt.Errorf("spec.resources.storage.%s duplicates role %q", key, role)
		}
		seen[role] = true
		parsed, err := parseStorageRole(role, value)
		if err != nil {
			return StorageContract{}, true, fmt.Errorf("spec.resources.storage.%s: %w", key, err)
		}
		contract.Roles = append(contract.Roles, parsed)
	}
	sort.Slice(contract.Roles, func(i, j int) bool {
		return storageRoleOrder(contract.Roles[i].Role) < storageRoleOrder(contract.Roles[j].Role)
	})
	return contract, true, nil
}

func (c StorageContract) Role(role string) (StorageRole, bool) {
	role = normalizeStorageRole(role)
	for _, r := range c.Roles {
		if r.Role == role {
			return r, true
		}
	}
	return StorageRole{}, false
}

func (c StorageContract) Labels() map[string]string {
	out := map[string]string{}
	if durable, ok := c.Role(StorageRoleDurableData); ok && durable.Type != "" {
		out[workloadmeta.LabelStorageDurableType] = durable.Type
	}
	if hot, ok := c.Role(StorageRoleHotScratch); ok && hot.Type != "" {
		out[workloadmeta.LabelStorageHotType] = hot.Type
	}
	if modelCache, ok := c.Role(StorageRoleModelCache); ok && modelCache.Type != "" {
		out[workloadmeta.LabelStorageModelCacheType] = modelCache.Type
	}
	if c.Checkpointing.Format != "" {
		out[workloadmeta.LabelStorageCheckpointFormat] = c.Checkpointing.Format
	}
	return out
}

func (c StorageContract) Annotations() map[string]string {
	if summary := c.Summary(); summary != "" {
		return map[string]string{workloadmeta.AnnotationStorageContract: summary}
	}
	return nil
}

func (c StorageContract) Summary() string {
	var parts []string
	for _, role := range c.Roles {
		if s := role.Summary(); s != "" {
			parts = append(parts, s)
		}
	}
	if c.Checkpointing.Format != "" {
		parts = append(parts, "checkpointing(format="+c.Checkpointing.Format+")")
	}
	return strings.Join(parts, ";")
}

func (r StorageRole) Summary() string {
	var attrs []string
	if r.Type != "" {
		attrs = append(attrs, "type="+r.Type)
	}
	if r.MountPath != "" {
		attrs = append(attrs, "mount="+r.MountPath)
	}
	if r.Cache != "" {
		attrs = append(attrs, "cache="+r.Cache)
	}
	if r.Fallback != "" {
		attrs = append(attrs, "fallback="+r.Fallback)
	}
	if r.Scope != "" {
		attrs = append(attrs, "scope="+r.Scope)
	}
	if len(attrs) == 0 {
		return r.Role
	}
	return r.Role + "(" + strings.Join(attrs, ",") + ")"
}

func parseStorageRole(role string, raw any) (StorageRole, error) {
	block, ok := raw.(map[string]any)
	if !ok {
		return StorageRole{}, fmt.Errorf("must be a map")
	}
	out := StorageRole{Role: role}
	for key, value := range block {
		s, ok := value.(string)
		if !ok {
			return StorageRole{}, fmt.Errorf("%s must be a string", key)
		}
		s = strings.TrimSpace(s)
		switch normalizeStorageKey(key) {
		case "type", "kind", "backing":
			out.Type = normalizeStorageValue(s)
			if out.Type != "" {
				if err := validateStorageSlug(out.Type); err != nil {
					return StorageRole{}, fmt.Errorf("%s=%q: %w", key, s, err)
				}
			}
		case "mountpath":
			out.MountPath = s
			if out.MountPath != "" && !strings.HasPrefix(out.MountPath, "/") {
				return StorageRole{}, fmt.Errorf("mountPath must be absolute")
			}
		case "cache":
			out.Cache = normalizeStorageValue(s)
			if out.Cache != "" {
				if err := validateStorageSlug(out.Cache); err != nil {
					return StorageRole{}, fmt.Errorf("cache=%q: %w", s, err)
				}
			}
		case "fallback":
			out.Fallback = normalizeStorageRole(s)
			if out.Fallback == "" {
				return StorageRole{}, fmt.Errorf("fallback=%q must name a known storage role", s)
			}
		case "scope":
			out.Scope = normalizeStorageValue(s)
			if out.Scope != "" {
				if err := validateStorageSlug(out.Scope); err != nil {
					return StorageRole{}, fmt.Errorf("scope=%q: %w", s, err)
				}
			}
		default:
			return StorageRole{}, fmt.Errorf("unknown field %q", key)
		}
	}
	return out, nil
}

func validateStorageSlug(s string) error {
	if len(s) > 63 {
		return fmt.Errorf("storage slug must be <= 63 characters")
	}
	if !profileNameRE.MatchString(s) {
		return fmt.Errorf("must be a lowercase storage slug")
	}
	return nil
}

func parseStorageCheckpointing(raw any) (StorageCheckpointing, error) {
	block, ok := raw.(map[string]any)
	if !ok {
		return StorageCheckpointing{}, fmt.Errorf("must be a map")
	}
	out := StorageCheckpointing{}
	for key, value := range block {
		s, ok := value.(string)
		if !ok {
			return StorageCheckpointing{}, fmt.Errorf("%s must be a string", key)
		}
		switch normalizeStorageKey(key) {
		case "format":
			out.Format = normalizeStorageValue(s)
		default:
			return StorageCheckpointing{}, fmt.Errorf("unknown field %q", key)
		}
	}
	return out, nil
}

func normalizeStorageKey(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(s), "-", ""), "_", ""))
}

func normalizeStorageRole(s string) string {
	switch normalizeStorageKey(s) {
	case "durable", "durabledata", "data":
		return StorageRoleDurableData
	case "hot", "hotscratch", "scratch":
		return StorageRoleHotScratch
	case "modelcache", "model", "models":
		return StorageRoleModelCache
	case "checkpointing":
		return "checkpointing"
	default:
		return ""
	}
}

func normalizeStorageValue(s string) string {
	v := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "_", "-"))
	switch v {
	case "emptydir":
		return StorageTypeEmptyDir
	case "localnvme":
		return storageTypeLocalNVMe
	case "azurecontainerstorage":
		return storageTypeAzureContainerStorage
	default:
		return v
	}
}

func storageRoleOrder(role string) int {
	switch role {
	case StorageRoleDurableData:
		return 0
	case StorageRoleHotScratch:
		return 1
	case StorageRoleModelCache:
		return 2
	default:
		return 99
	}
}
