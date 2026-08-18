// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

// Keep existing behavioral tests focused on input construction while the
// production name makes the pre-resolution lifetime explicit.
type runDispatchOptions = unresolvedRunOptions

func resolvedJobRequestForTest(target resolvedRunTarget) *runJobRequest {
	request, _ := target.(*runJobRequest)
	return request
}

func resolvedRayJobRequestForTest(target resolvedRunTarget) *runRayJobRequest {
	request, _ := target.(*runRayJobRequest)
	return request
}

func resolvedManagedWorkflowRequestForTest(target resolvedRunTarget) *runManagedWorkflowRequest {
	request, _ := target.(*runManagedWorkflowRequest)
	return request
}
