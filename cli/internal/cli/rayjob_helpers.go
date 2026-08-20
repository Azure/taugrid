// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/Azure/taugrid/cli/internal/rayjobrender"
	"github.com/Azure/taugrid/core/experiment"
)

func rayJobRequestedGPUCount(workers, gpusPerWorker int) int {
	if workers < 1 || gpusPerWorker <= 0 {
		return 0
	}
	return workers * gpusPerWorker
}

func buildRayJobCaptureMetadata(ctx context.Context, captureCommand, name, namespace, image, scriptPath string, workers, gpusPerWorker int, dataPVC string) (experiment.Metadata, error) {
	configHash, err := experiment.HashFile(scriptPath)
	if err != nil {
		return experiment.Metadata{}, err
	}
	if image == "" {
		if gpusPerWorker > 0 {
			image = rayjobrender.DefaultGPUImage
		} else {
			image = rayjobrender.DefaultCPUImage
		}
	}
	metadata := experiment.Metadata{
		RunID:        name,
		Namespace:    namespace,
		WorkloadKind: experiment.WorkloadKindRayJob,
		TauCommand:   experiment.RedactCommandArgs(strings.Fields(captureCommand)),
		Image:        image,
		CodeSHA:      experiment.GitHeadSHA("."),
		ConfigHash:   configHash,
		GPUCount:     workers * gpusPerWorker,
		StorageMounts: []experiment.StorageMount{
			{Source: storageMountType(dataPVC), SourceRef: dataPVC, Path: "/data"},
		},
	}
	metadata.Stellar = runExperimentMetadataFromContext(ctx).StellarMetadata()
	return metadata, nil
}

func storageMountType(pvc string) string {
	if pvc == "" {
		return "emptyDir"
	}
	return "pvc"
}

func formatRayJobSubmitHandoff(name, namespace, kubeContext string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nsubmitted %s (kind=rayjob, ns=%s)\n", name, namespace)
	fmt.Fprintf(&b, "rayjob:  kubectl get rayjob %s -n %s%s\n", name, namespace, contextFlag(kubeContext))
	fmt.Fprintf(&b, "head:    cluster=$(kubectl get rayjob %s -n %s -o jsonpath='{.status.rayClusterName}'%s) && kubectl get pod -n %s -l ray.io/cluster=$cluster,ray.io/node-type=head%s\n", name, namespace, contextFlag(kubeContext), namespace, contextFlag(kubeContext))
	fmt.Fprintf(&b, "logs:    tau run logs %s -n %s -f%s\n", name, namespace, contextFlag(kubeContext))
	fmt.Fprintf(&b, "kueue:   uid=$(kubectl get rayjob %s -n %s -o jsonpath='{.metadata.uid}'%s) && kubectl get workload -n %s -l kueue.x-k8s.io/job-uid=$uid%s\n", name, namespace, contextFlag(kubeContext), namespace, contextFlag(kubeContext))
	fmt.Fprintf(&b, "delete:  kubectl delete rayjob %s -n %s%s\n", name, namespace, contextFlag(kubeContext))
	return b.String()
}
