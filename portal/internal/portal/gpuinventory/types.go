// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package gpuinventory

import (
	"context"
	"time"
)

const (
	fullGPUResource = "nvidia.com/gpu"
	queueLabel      = "kueue.x-k8s.io/queue-name"
)

type Reader interface {
	ListPods(ctx context.Context, namespace string) ([]byte, error)
}

type Options struct {
	Namespace    string
	LocalQueue   string
	ManagedOnly  bool
	MaxWorkloads int
	Now          time.Time
}

type Snapshot struct {
	GeneratedAt time.Time          `json:"generatedAt"`
	Summary     Summary            `json:"summary"`
	Workloads   []Workload         `json:"workloads"`
	Partial     bool               `json:"partial"`
	Truncated   bool               `json:"truncated"`
	Sources     []SourceDiagnostic `json:"sources"`
}

type Summary struct {
	Workloads         int   `json:"workloads"`
	Pods              int   `json:"pods"`
	ReadyPods         int   `json:"readyPods"`
	PendingPods       int   `json:"pendingPods"`
	BoundPods         int   `json:"boundPods"`
	RequestedFullGPUs int64 `json:"requestedFullGpus"`
	PendingFullGPUs   int64 `json:"pendingFullGpus"`
	BoundFullGPUs     int64 `json:"boundFullGpus"`
	CountUnknownPods  int   `json:"countUnknownPods"`
}

type Workload struct {
	Namespace         string             `json:"namespace"`
	Owner             Owner              `json:"owner"`
	Managed           bool               `json:"managed"`
	Queue             string             `json:"queue,omitempty"`
	Workspace         string             `json:"workspace,omitempty"`
	Team              string             `json:"team,omitempty"`
	RunID             string             `json:"runId,omitempty"`
	Lane              string             `json:"lane,omitempty"`
	Profile           string             `json:"profile,omitempty"`
	GPUClass          string             `json:"gpuClass,omitempty"`
	PodCount          int                `json:"podCount"`
	ReadyPods         int                `json:"readyPods"`
	PendingPods       int                `json:"pendingPods"`
	BoundPods         int                `json:"boundPods"`
	Phases            PhaseCounts        `json:"phases"`
	Nodes             []string           `json:"nodes"`
	Resources         []ResourceQuantity `json:"resources"`
	PendingResources  []ResourceQuantity `json:"pendingResources"`
	BoundResources    []ResourceQuantity `json:"boundResources"`
	RequestedFullGPUs int64              `json:"requestedFullGpus"`
	PendingFullGPUs   int64              `json:"pendingFullGpus"`
	BoundFullGPUs     int64              `json:"boundFullGpus"`
	CountUnknown      bool               `json:"countUnknown"`
	CountUnknownPods  int                `json:"countUnknownPods"`
	Diagnostics       []Diagnostic       `json:"diagnostics"`
	Detail            *DetailReference   `json:"detail,omitempty"`
}

type Owner struct {
	Kind         string `json:"kind"`
	Name         string `json:"name"`
	UID          string `json:"uid,omitempty"`
	Resolution   string `json:"resolution"`
	RawOwnerKind string `json:"rawOwnerKind,omitempty"`
	RawOwnerName string `json:"rawOwnerName,omitempty"`
	RawOwnerUID  string `json:"rawOwnerUid,omitempty"`
}

type PhaseCounts struct {
	Pending int `json:"pending"`
	Running int `json:"running"`
}

type ResourceQuantity struct {
	Name  string `json:"name"`
	Count int64  `json:"count"`
}

type Diagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type DetailReference struct {
	ResourceUID string `json:"resourceUid"`
	Path        string `json:"path"`
}

type SourceDiagnostic struct {
	Source  string `json:"source"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
}
