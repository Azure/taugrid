// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package queue builds a researcher-facing view of Kueue queue pressure.
package queue

import (
	"time"

	"github.com/Azure/taugrid/core/kueueapi"
	runtopology "github.com/Azure/taugrid/core/topology"
)

const (
	// DefaultNamespace is the default Kueue LocalQueue namespace for Tau workloads.
	DefaultNamespace = runtopology.DefaultLocalQueueNamespace
	// GPUResource re-exports the Kueue DRA GPU accounting resource name so
	// queue consumers do not need a second import for the common case.
	GPUResource = kueueapi.GPUResource
	// GPUResourceDevicePlugin re-exports the NVIDIA device-plugin resource name.
	GPUResourceDevicePlugin = kueueapi.GPUResourceDevicePlugin
)

// Options controls live queue fetches and post-fetch filtering.
type Options struct {
	Namespace string
	Team      string
	Lane      string
	GPUClass  string
}

// Snapshot is the machine-readable queue/capacity view returned by Tau.
type Snapshot struct {
	Namespace string   `json:"namespace"`
	Groups    []Group  `json:"groups"`
	Hints     []string `json:"hints,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

// Group is one user-facing queue slice projected from live Kueue objects.
type Group struct {
	// Namespace is the LocalQueue namespace this group reports on. It is set
	// on every group so a cluster-wide snapshot can distinguish the same queue
	// name across namespaces.
	Namespace        string            `json:"namespace,omitempty"`
	GPUClass         string            `json:"gpuClass"`
	Team             string            `json:"team"`
	Lane             string            `json:"lane"`
	Queue            string            `json:"queue"`
	ClusterQueue     string            `json:"clusterQueue"`
	ResourceFlavor   string            `json:"resourceFlavor"`
	Presets          []string          `json:"presets"`
	QueueFound       bool              `json:"queueFound"`
	QuotaFound       bool              `json:"quotaFound"`
	Pending          int               `json:"pending"`
	Admitted         int               `json:"admitted"`
	Reserving        int               `json:"reserving"`
	GPUReserved      int64             `json:"gpuReserved"`
	GPUUsed          int64             `json:"gpuUsed"`
	GPUBorrowed      int64             `json:"gpuBorrowed"`
	GPUNominal       int64             `json:"gpuNominal"`
	GPUHeadroom      int64             `json:"gpuHeadroom"`
	Conditions       []Condition       `json:"conditions,omitempty"`
	PendingWorkloads []PendingWorkload `json:"pendingWorkloads,omitempty"`
}

// Condition mirrors the small Kueue condition subset exposed in queue output.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// PendingWorkload is a Kueue Workload that has not been admitted or finished.
type PendingWorkload struct {
	Name         string    `json:"name"`
	Namespace    string    `json:"namespace"`
	Queue        string    `json:"queue"`
	ClusterQueue string    `json:"clusterQueue,omitempty"`
	Team         string    `json:"team,omitempty"`
	Lane         string    `json:"lane,omitempty"`
	GPUClass     string    `json:"gpuClass,omitempty"`
	Shape        string    `json:"shape,omitempty"`
	Preset       string    `json:"preset,omitempty"`
	GPURequested int64     `json:"gpuRequested,omitempty"`
	Reason       string    `json:"reason,omitempty"`
	Message      string    `json:"message,omitempty"`
	CreatedAt    time.Time `json:"createdAt,omitempty"`
}
