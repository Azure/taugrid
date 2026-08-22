// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package profile contains the shared workload-profile contract and the
// render-time resource contract used by Tau manifest builders. It intentionally
// does not load legacy profile catalogs.
package profile

// Profile is the in-memory resource contract consumed by renderers.
type Profile struct {
	Name                  string
	Lane                  string
	Queue                 string
	ExecutionTarget       ExecutionTarget
	Topology              Topology
	Resources             Resources
	Runtime               Runtime
	ActiveDeadlineSeconds int64
}

type Topology struct {
	Team                      string
	Mode                      string
	Placement                 string
	Shape                     string
	GPUClass                  string
	PriorityTier              string
	PodPriorityClassName      string
	WorkloadPriorityClassName string
	DisableDefaultPriorities  bool
}

type Resources struct {
	Requests map[string]any
	Limits   map[string]any
	GPU      GPUContract
}

type Runtime struct {
	Image           string
	ImagePullPolicy string
	SecurityContext map[string]any
}
