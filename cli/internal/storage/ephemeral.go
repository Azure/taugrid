// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storage

func EphemeralVolume(name, storageClassName, size string) map[string]any {
	return map[string]any{
		"name": name,
		"ephemeral": map[string]any{
			"volumeClaimTemplate": map[string]any{
				"spec": map[string]any{
					"accessModes":      []any{"ReadWriteOnce"},
					"storageClassName": storageClassName,
					"resources": map[string]any{
						"requests": map[string]any{"storage": size},
					},
				},
			},
		},
	}
}
