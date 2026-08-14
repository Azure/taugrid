// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storage

// RuntimeEnv returns the stable runtime paths for typed storage mounts.
func RuntimeEnv() map[string]string {
	return map[string]string{
		"TAU_DATA_DIR":                DurableRoot,
		"TAU_HOT_DIR":                 HotRoot,
		"TAU_DATASETS_DIR":            HotDatasetsDir,
		"TAU_CHECKPOINTS_DIR":         HotCheckpointsDir,
		"TAU_DURABLE_DATASETS_DIR":    DurableDatasetsDir,
		"TAU_DURABLE_CHECKPOINTS_DIR": DurableCheckpointsDir,
	}
}
