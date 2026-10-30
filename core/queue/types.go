// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package queue

import (
	"sort"
	"time"

	"github.com/Azure/taugrid/core/kueueapi"
	runtopology "github.com/Azure/taugrid/core/topology"
)

const (
	DefaultNamespace        = runtopology.DefaultLocalQueueNamespace
	GPUResource             = kueueapi.GPUResource
	GPUResourceDevicePlugin = kueueapi.GPUResourceDevicePlugin
)

type Options struct {
	Namespace string
	Team      string
	Lane      string
	GPUClass  string
}

type Snapshot struct {
	Namespace string   `json:"namespace"`
	Groups    []Group  `json:"groups"`
	Hints     []string `json:"hints,omitempty"`
	Warnings  []string `json:"warnings,omitempty"`
}

type Group struct {
	Namespace               string            `json:"namespace,omitempty"`
	Workspace               string            `json:"workspace,omitempty"`
	GPUClass                string            `json:"gpuClass"`
	Team                    string            `json:"team"`
	Lane                    string            `json:"lane"`
	Queue                   string            `json:"queue"`
	ClusterQueue            string            `json:"clusterQueue"`
	Cohort                  string            `json:"cohort,omitempty"`
	QuotaScope              string            `json:"quotaScope,omitempty"`
	ResourceFlavor          string            `json:"resourceFlavor"`
	Presets                 []string          `json:"presets"`
	QueueFound              bool              `json:"queueFound"`
	QuotaFound              bool              `json:"quotaFound"`
	Pending                 int               `json:"pending"`
	Admitted                int               `json:"admitted"`
	Reserving               int               `json:"reserving"`
	GPUReserved             int64             `json:"gpuReserved"`
	GPUUsed                 int64             `json:"gpuUsed"`
	GPUBorrowed             int64             `json:"gpuBorrowed"`
	GPUNominal              int64             `json:"gpuNominal"`
	GPUHeadroom             int64             `json:"gpuHeadroom"`
	GPUNominalAvailable     bool              `json:"gpuNominalAvailable"`
	GPUReservationAvailable bool              `json:"gpuReservationAvailable"`
	Conditions              []Condition       `json:"conditions,omitempty"`
	PendingWorkloads        []PendingWorkload `json:"pendingWorkloads,omitempty"`
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type PendingWorkload struct {
	Name                       string    `json:"name"`
	Namespace                  string    `json:"namespace"`
	Queue                      string    `json:"queue"`
	ClusterQueue               string    `json:"clusterQueue,omitempty"`
	Team                       string    `json:"team,omitempty"`
	Lane                       string    `json:"lane,omitempty"`
	GPUClass                   string    `json:"gpuClass,omitempty"`
	Shape                      string    `json:"shape,omitempty"`
	Preset                     string    `json:"preset,omitempty"`
	GPURequested               int64     `json:"gpuRequested,omitempty"`
	AdmissionPriorityClass     string    `json:"admissionPriorityClass,omitempty"`
	AdmissionPriorityClassKind string    `json:"admissionPriorityClassKind,omitempty"`
	AdmissionPriority          *int32    `json:"admissionPriority,omitempty"`
	PodPriorityClasses         []string  `json:"podPriorityClasses,omitempty"`
	Reason                     string    `json:"reason,omitempty"`
	Message                    string    `json:"message,omitempty"`
	CreatedAt                  time.Time `json:"createdAt,omitempty"`
}

func SortPendingWorkloads(workloads []PendingWorkload) {
	sort.Slice(workloads, func(i, j int) bool {
		a, b := workloads[i], workloads[j]
		if a.AdmissionPriority != nil || b.AdmissionPriority != nil {
			if a.AdmissionPriority == nil {
				return false
			}
			if b.AdmissionPriority == nil {
				return true
			}
			if *a.AdmissionPriority != *b.AdmissionPriority {
				return *a.AdmissionPriority > *b.AdmissionPriority
			}
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
}
