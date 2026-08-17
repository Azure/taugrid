// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

// Keep existing behavioral tests focused on input construction while the
// production name makes the pre-resolution lifetime explicit.
type runDispatchOptions = unresolvedRunOptions

func resolvedJobRequestForTest(target resolvedRunTarget) *runJobRequest {
	request, _ := target.request.(*runJobRequest)
	return request
}

func resolvedRayRequestForTest(target resolvedRunTarget) *runRayRequest {
	request, _ := target.request.(*runRayRequest)
	return request
}

func resolvedManagedWorkflowRequestForTest(target resolvedRunTarget) *runManagedWorkflowRequest {
	request, _ := target.request.(*runManagedWorkflowRequest)
	return request
}

func resolvedRuntimeForTest(target resolvedRunTarget) (resolvedRunRuntime, bool) {
	switch request := target.request.(type) {
	case *runJobRequest:
		return request.Options.resolvedRunRuntime, true
	case *runRayRequest:
		return request.Options.resolvedRunRuntime, true
	case *runManagedWorkflowRequest:
		return request.Options.resolvedRunRuntime, true
	default:
		return resolvedRunRuntime{}, false
	}
}
