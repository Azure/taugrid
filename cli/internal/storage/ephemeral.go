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

const EphemeralFSGroup int64 = 65532

func ApplyEphemeralFSGroup(pod map[string]any) {
	context, _ := pod["securityContext"].(map[string]any)
	if context == nil {
		context = map[string]any{}
	}
	if _, exists := context["fsGroup"]; !exists {
		context["fsGroup"] = EphemeralFSGroup
		context["fsGroupChangePolicy"] = "OnRootMismatch"
	}
	pod["securityContext"] = context
}
