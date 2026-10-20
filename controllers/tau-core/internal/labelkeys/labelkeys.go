// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package labelkeys

const Domain = "tau.azure.com/"

const (
	LabelWorkspace          = "tau.azure.com/workspace"
	LabelTeam               = "tau.azure.com/team"
	LabelLocalQueue         = "tau.azure.com/local-queue"
	LabelGPUClass           = "tau.azure.com/gpu-class"
	LabelSite               = "tau.azure.com/site"
	LabelRegion             = "tau.azure.com/region"
	LabelNetworkDomain      = "tau.azure.com/network-domain"
	LabelAcceleratorDomain  = "tau.azure.com/accelerator-domain"
	LabelInfiniband         = "tau.azure.com/infiniband"
	LabelDiscoverGPUFlavors = "tau.azure.com/discover-gpu-flavors"

	AnnotationApproved             = "tau.azure.com/approved"
	AnnotationRejected             = "tau.azure.com/rejected"
	AnnotationReviewedBy           = "tau.azure.com/reviewed-by"
	AnnotationResultScope          = "tau.azure.com/result-scope"
	AnnotationOwnerUID             = "tau.azure.com/owner-uid"
	AnnotationV0Primary            = "tau.azure.com/v0-primary-workspace"
	AnnotationAdoptionNamespaceUID = "tau.azure.com/adoption-namespace-uid"
	AnnotationManagedQuotaKeys     = "tau.azure.com/managed-quota-keys"

	FinalizerWorkspaceCleanup = "tau.azure.com/workspace-cleanup"
	FinalizerTeamCleanup      = "tau.azure.com/team-cleanup"
)
