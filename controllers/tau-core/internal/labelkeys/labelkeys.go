// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package labelkeys owns every tau.azure.com label, annotation, and finalizer
// key that the Tau workspace controller module reads or writes.
//
// Keep this package dependency-free: both reconcilers and manifest contract
// tests import it. The Tau CLI is a separate Go module, so contract_test.go
// source-parses its canonical workloadmeta declarations to catch cross-module
// drift without coupling either published artifact to the other.
package labelkeys

const Domain = "tau.azure.com/"

// Controller-owned namespace, helper-object, and quota keys.
const (
	LabelWorkspace  = "tau.azure.com/workspace"
	LabelLocalQueue = "tau.azure.com/local-queue"
	LabelGPUClass   = "tau.azure.com/gpu-class"
	LabelManagedBy  = "tau.azure.com/managed-by"

	AnnotationApproved         = "tau.azure.com/approved"
	AnnotationRejected         = "tau.azure.com/rejected"
	AnnotationReviewedBy       = "tau.azure.com/reviewed-by"
	AnnotationResultScope      = "tau.azure.com/result-scope"
	AnnotationV0Primary        = "tau.azure.com/v0-primary-workspace"
	AnnotationResultPVC        = "tau.azure.com/result-pvc"
	AnnotationArtifactBundleID = "tau.azure.com/artifact-bundle-id"
	AnnotationArtifactStore    = "tau.azure.com/artifact-store"

	FinalizerWorkspaceCleanup = "tau.azure.com/workspace-cleanup"
)
