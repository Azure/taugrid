// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import "github.com/Azure/taugrid/controllers/tau-core/internal/labelkeys"

const (
	labelManagedBy              = "app.kubernetes.io/managed-by"
	labelManagedByValue         = "tau-core-controller"
	labelWorkspace              = labelkeys.LabelWorkspace
	labelWorkspaceLocalQueue    = labelkeys.LabelLocalQueue
	labelKueueDefaultLocalQueue = "kueue.x-k8s.io/default-local-queue"
	labelPSAEnforce             = "pod-security.kubernetes.io/enforce"
	labelPSAAudit               = "pod-security.kubernetes.io/audit"
	labelPSAWarn                = "pod-security.kubernetes.io/warn"
	labelAzureWIUse             = "azure.workload.identity/use"

	annotationApproved        = labelkeys.AnnotationApproved
	annotationRejected        = labelkeys.AnnotationRejected
	annotationReviewedBy      = labelkeys.AnnotationReviewedBy
	annotationAzureWIClientID = "azure.workload.identity/client-id"
	annotationResultScope     = labelkeys.AnnotationResultScope
	annotationV0Primary       = labelkeys.AnnotationV0Primary

	workspaceFinalizer = labelkeys.FinalizerWorkspaceCleanup

	defaultRoleName            = "tau-researcher-v1"
	clusterQueueReaderRoleName = "tau-clusterqueue-reader-v1"
)
