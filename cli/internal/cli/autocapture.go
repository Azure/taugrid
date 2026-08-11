// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"strings"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/cli/internal/manifest"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/runconfig"
)

func buildJobCaptureMetadata(ctx context.Context, captureCommand, name, namespace, image, scriptPath, pvcMount string, source *runconfig.Source, p profile.Profile, volumes []jobrender.Volume, volumeMounts []jobrender.VolumeMount) (experiment.Metadata, error) {
	var configHash string
	if source != nil {
		configHash = experiment.HashBytes([]byte(source.Image + "\n" + source.Path + "\n" + scriptPath))
	} else {
		var err error
		configHash, err = experiment.HashFile(scriptPath)
		if err != nil {
			return experiment.Metadata{}, err
		}
	}
	metadata := experiment.Metadata{
		RunID:            name,
		Namespace:        namespace,
		WorkloadKind:     experiment.WorkloadKindJob,
		TauCommand:       experiment.RedactCommandArgs(strings.Fields(captureCommand)),
		Image:            firstNonEmpty(image, profileRuntimeImage(p)),
		CodeSHA:          experiment.GitHeadSHA("."),
		ConfigHash:       configHash,
		GPUCount:         gpuCountFromProfile(p),
		DRAClaimTemplate: profile.DRAClaimTemplate(p),
		StorageMounts:    jobStorageMounts(p, pvcMount, volumes, volumeMounts),
	}
	metadata.Stellar = runExperimentMetadataFromContext(ctx).StellarMetadata()
	return metadata, nil
}

func buildManagedWorkflowCaptureMetadata(ctx context.Context, captureCommand string, m *manifest.Manifest, raw []byte, namespace, workloadKind string) experiment.Metadata {
	kind := workloadKind
	if kind == "" {
		kind = manifest.WorkloadKindJob
	}
	metadata := experiment.Metadata{
		RunID:            m.ResourceName(),
		Namespace:        namespace,
		WorkloadKind:     kind,
		TauCommand:       experiment.RedactCommandArgs(strings.Fields(captureCommand)),
		Image:            m.RuntimeImage(),
		CodeSHA:          experiment.GitHeadSHA("."),
		ConfigHash:       experiment.HashBytes(raw),
		GPUCount:         m.Compute.GPUs,
		DRAClaimTemplate: manifest.Claim(m.Compute.GPUs),
		StorageMounts:    managedWorkflowStorageMounts(m.DataPVC()),
	}
	metadata.Stellar = runExperimentMetadataFromContext(ctx).StellarMetadata()
	return metadata
}

func addRunWorkspaceMetadata(metadata experiment.Metadata, workspaceID, resultScope string) experiment.Metadata {
	metadata.WorkspaceID = workspaceID
	metadata.ResultScope = resultScope
	return metadata
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func gpuCountFromProfile(p profile.Profile) int {
	if contract, ok, err := profile.GPUContractFromProfile(p); err == nil && ok && contract.Count > 0 {
		return contract.Count
	}
	if count, ok := profile.GPUCountFromClaimTemplate(profile.DRAClaimTemplate(p)); ok {
		return count
	}
	return 0
}

func jobStorageMounts(p profile.Profile, pvcMount string, volumes []jobrender.Volume, volumeMounts []jobrender.VolumeMount) []experiment.StorageMount {
	var mounts []experiment.StorageMount
	for _, mount := range profilePersistenceMounts(p) {
		mounts = append(mounts, experiment.StorageMount{
			Source:    "pvc",
			SourceRef: mount.PVC,
			Path:      mount.MountPath,
			ReadOnly:  mount.ReadOnly,
		})
	}
	if pvcMount != "" {
		mounts = append(mounts, experiment.StorageMount{Source: "pvc", SourceRef: pvcMount, Path: "/data"})
	}
	volumeByName := map[string]jobrender.Volume{}
	for _, volume := range volumes {
		volumeByName[volume.Name] = volume
	}
	for _, mount := range volumeMounts {
		volume := volumeByName[mount.Name]
		if volume.PVC == "" {
			continue
		}
		mounts = append(mounts, experiment.StorageMount{
			Source:    "pvc",
			SourceRef: volume.PVC,
			Path:      mount.MountPath,
			ReadOnly:  mount.ReadOnly,
		})
	}
	return dedupeStorageMounts(mounts)
}

func managedWorkflowStorageMounts(dataPVC string) []experiment.StorageMount {
	dataPVC = strings.TrimSpace(dataPVC)
	source := "emptyDir"
	sourceRef := ""
	if dataPVC != "" {
		source = "pvc"
		sourceRef = dataPVC
	}
	return []experiment.StorageMount{
		{Source: source, SourceRef: sourceRef, Path: "/data"},
		{Source: "emptyDir", Path: "/mnt"},
	}
}

func dedupeStorageMounts(mounts []experiment.StorageMount) []experiment.StorageMount {
	seen := map[string]bool{}
	var out []experiment.StorageMount
	for _, mount := range mounts {
		key := mount.Source + "\x00" + mount.SourceRef + "\x00" + mount.Path
		if mount.Path == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, mount)
	}
	return out
}
