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

	AnnotationApproved    = "tau.azure.com/approved"
	AnnotationRejected    = "tau.azure.com/rejected"
	AnnotationReviewedBy  = "tau.azure.com/reviewed-by"
	AnnotationResultScope = "tau.azure.com/result-scope"
	AnnotationV0Primary   = "tau.azure.com/v0-primary-workspace"

	FinalizerWorkspaceCleanup = "tau.azure.com/workspace-cleanup"
)
