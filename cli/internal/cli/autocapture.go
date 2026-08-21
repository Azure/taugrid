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
	"github.com/Azure/taugrid/core/workloadmeta"
)

func buildJobCaptureMetadata(ctx context.Context, captureCommand, name, namespace, image, pvcMount string, p profile.Profile, volumes []jobrender.Volume, volumeMounts []jobrender.VolumeMount, configHash string) experiment.Metadata {
	metadata := experiment.Metadata{
		RunID:         name,
		Namespace:     namespace,
		WorkloadKind:  experiment.WorkloadKindJob,
		TauCommand:    experiment.RedactCommandArgs(strings.Fields(captureCommand)),
		Image:         firstNonEmpty(image, p.Runtime.Image),
		CodeSHA:       experiment.GitHeadSHA("."),
		ConfigHash:    configHash,
		GPUCount:      gpuCountFromProfile(p),
		StorageMounts: jobStorageMounts(pvcMount, volumes, volumeMounts),
	}
	metadata.Stellar = runExperimentMetadataFromContext(ctx).StellarMetadata()
	return metadata
}

func buildManagedWorkflowCaptureMetadata(ctx context.Context, captureCommand string, m *manifest.Manifest, raw []byte, namespace, workloadKind, configHash string) experiment.Metadata {
	kind := workloadKind
	if kind == "" {
		kind = manifest.WorkloadKindJob
	}
	if configHash == "" {
		configHash = experiment.HashBytes(raw)
	}
	metadata := experiment.Metadata{
		RunID:            m.ResourceName(),
		Namespace:        namespace,
		WorkloadKind:     kind,
		TauCommand:       experiment.RedactCommandArgs(strings.Fields(captureCommand)),
		Image:            m.RuntimeImage(),
		CodeSHA:          experiment.GitHeadSHA("."),
		ConfigHash:       configHash,
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

func directJobPayloadAnnotation(scriptPath string, source *runconfig.Source) (string, string, error) {
	if source != nil {
		identity := strings.Join([]string{source.Image, source.Path, scriptPath}, "\n")
		return workloadmeta.AnnotationPayloadDigest, experiment.HashBytes([]byte(identity)), nil
	}
	digest, err := experiment.HashFile(scriptPath)
	return workloadmeta.AnnotationScriptPayloadDigest, digest, err
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
	if p.Resources.GPU.Count > 0 {
		return p.Resources.GPU.Count
	}
	return 0
}

func jobStorageMounts(pvcMount string, volumes []jobrender.Volume, volumeMounts []jobrender.VolumeMount) []experiment.StorageMount {
	var mounts []experiment.StorageMount
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
