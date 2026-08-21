// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workloadmeta

import "strings"

func StampWorkspace(labels map[string]string, workspace string) map[string]string {
	if workspace == "" {
		return labels
	}
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelWorkspace] = workspace
	return labels
}

// PodCorrelationAnnotations returns annotations used to correlate pod logs
// with a workspace and experiment.
func PodCorrelationAnnotations(annotations map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range annotations {
		if value == "" {
			continue
		}
		if key == AnnotationWorkspaceID ||
			key == AnnotationResultScope ||
			key == AnnotationExperimentSource ||
			key == AnnotationMetricsSession ||
			strings.HasPrefix(key, StellarAnnotationPrefix) {
			out[key] = value
		}
	}
	return out
}
