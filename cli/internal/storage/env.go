// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storage

import "github.com/Azure/taugrid/core/resourceprofile"

// ContractEnv returns the runtime env vars that expose Tau's storage contract
// inside workload and validation pods.
func ContractEnv(contract profile.StorageContract, hasContract bool) map[string]string {
	env := map[string]string{
		"TAU_DATA_DIR":                DurableRoot,
		"TAU_HOT_DIR":                 HotRoot,
		"TAU_DATASETS_DIR":            HotDatasetsDir,
		"TAU_CHECKPOINTS_DIR":         HotCheckpointsDir,
		"TAU_DURABLE_DATASETS_DIR":    DurableDatasetsDir,
		"TAU_DURABLE_CHECKPOINTS_DIR": DurableCheckpointsDir,
	}
	if hasContract {
		if durable, ok := contract.Role(profile.StorageRoleDurableData); ok {
			addStorageRoleEnv(env, "DURABLE", durable)
		}
		if hot, ok := contract.Role(profile.StorageRoleHotScratch); ok {
			addStorageRoleEnv(env, "HOT", hot)
		}
		if modelCache, ok := contract.Role(profile.StorageRoleModelCache); ok {
			addStorageRoleEnv(env, "MODEL_CACHE", modelCache)
			if modelCache.MountPath != "" {
				env["TAU_MODEL_CACHE_DIR"] = modelCache.MountPath
			}
		}
		if contract.Checkpointing.Format != "" {
			env["TAU_CHECKPOINT_FORMAT"] = contract.Checkpointing.Format
		}
	}
	return env
}

func addStorageRoleEnv(env map[string]string, prefix string, role profile.StorageRole) {
	if role.Type != "" {
		env["TAU_STORAGE_"+prefix+"_TYPE"] = role.Type
	}
	if role.MountPath != "" {
		env["TAU_STORAGE_"+prefix+"_MOUNT"] = role.MountPath
	}
	if role.Cache != "" {
		env["TAU_STORAGE_"+prefix+"_CACHE"] = role.Cache
	}
	if role.Fallback != "" {
		env["TAU_STORAGE_"+prefix+"_FALLBACK"] = role.Fallback
	}
	if role.Scope != "" {
		env["TAU_STORAGE_"+prefix+"_SCOPE"] = role.Scope
	}
}
