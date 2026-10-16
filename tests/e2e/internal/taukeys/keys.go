// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package taukeys

const (
	APIGroup = "tau.azure.com"

	LabelManagedBy     = "tau.azure.com/managed-by"
	LabelWorkspace     = "tau.azure.com/workspace"
	LabelMigrationRole = "tau.azure.com/migration-role"
	LabelRunID         = "tau.azure.com/run-id"
	LabelWorkloadKind  = "tau.azure.com/workload-kind"

	AnnotationWorkspaceID = "tau.azure.com/workspace-id"

	LabelGPUCount = "tau.azure.com/gpu-count"

	LabelSchedulerTest = "tau.azure.com/scheduler-test"

	AnnotationPayloadDigest = "tau.azure.com/payload-digest"
	AnnotationPayloadSHA256 = "tau.azure.com/payload-sha256"
	AnnotationClusterName   = "tau.azure.com/cluster-name"

	LabelActivationRun              = "tau.azure.com/activation-run"
	LabelActivationScope            = "tau.azure.com/activation-scope"
	AnnotationWorkerAPIServer       = "tau.azure.com/worker-api-server"
	AnnotationCredentialTransaction = "tau.azure.com/credential-transaction"
)
