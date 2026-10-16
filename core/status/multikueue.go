// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"sort"
	"strings"
)

const multiKueueControllerName = "kueue.x-k8s.io/multikueue"

const multiKueueAdmissionCheckName = "multikueue"

const multiKueueManagedBy = multiKueueControllerName

type AdmissionCheckSummary struct {
	WorkloadName string
	AdmissionCheck
}

type MultiKueueState string

const (
	MultiKueueStatePending   MultiKueueState = "Pending"
	MultiKueueStateNominated MultiKueueState = "Nominated"
	MultiKueueStateSelected  MultiKueueState = "Selected"
	MultiKueueStateReady     MultiKueueState = "Ready"
	MultiKueueStateRetry     MultiKueueState = "Retry"
	MultiKueueStateRejected  MultiKueueState = "Rejected"
)

func (s Snapshot) IsMultiKueue() bool {
	if s.JobFound {
		if s.JobManagedBy == multiKueueManagedBy {
			return true
		}
	} else if primaryRayJob(s).ManagedBy == multiKueueManagedBy || s.JobManagedBy == multiKueueManagedBy {
		return true
	}
	for _, w := range s.Workloads {
		for _, check := range w.AdmissionChecks {
			if isMultiKueueAdmissionCheck(check) {
				return true
			}
		}
		if w.ClusterName != "" || len(w.NominatedClusterNames) > 0 {
			return true
		}
	}
	return false
}

func (s Snapshot) MultiKueueState() MultiKueueState {
	if !s.IsMultiKueue() {
		return ""
	}
	anyRejected := false
	anyRetry := false
	allReady := len(s.Workloads) > 0
	anySelected := false
	anyNominated := false
	for _, workload := range s.Workloads {
		mkChecks := multiKueueAdmissionChecks(workload)
		for _, check := range mkChecks {
			switch check.State {
			case "Rejected":
				anyRejected = true
			case "Retry":
				anyRetry = true
			}
		}
		workloadReady := workloadMultiKueueReady(workload)
		allReady = allReady && workloadReady
		if workloadReady {
			continue
		}
		if workload.ClusterName != "" {
			anySelected = true
			continue
		}
		if len(workload.NominatedClusterNames) > 0 {
			anyNominated = true
		}
	}
	if anyRejected {
		return MultiKueueStateRejected
	}
	if anyRetry {
		return MultiKueueStateRetry
	}
	if allReady {
		return MultiKueueStateReady
	}
	if anySelected {
		return MultiKueueStateSelected
	}
	if anyNominated {
		return MultiKueueStateNominated
	}
	return MultiKueueStatePending
}

func (s Snapshot) ManagerOnlyMultiKueueView() bool {
	return s.IsMultiKueue() && len(s.Pods) == 0
}

func workloadMultiKueueReady(workload Workload) bool {
	checks := multiKueueAdmissionChecks(workload)
	if len(checks) == 0 {
		return false
	}
	for _, check := range checks {
		if check.State != "Ready" {
			return false
		}
	}
	return true
}

func workloadAllAdmissionChecksReady(workload Workload) bool {
	if len(workload.AdmissionChecks) == 0 {
		return false
	}
	for _, check := range workload.AdmissionChecks {
		if check.State != "Ready" {
			return false
		}
	}
	return true
}

func (s Snapshot) AllAdmissionChecksReady() bool {
	if len(s.Workloads) == 0 {
		return false
	}
	for _, workload := range s.Workloads {
		if !workloadAllAdmissionChecksReady(workload) {
			return false
		}
	}
	return true
}

func (s Snapshot) AnyAdmissionCheckRejected() bool {
	for _, workload := range s.Workloads {
		for _, check := range workload.AdmissionChecks {
			if check.State == "Rejected" {
				return true
			}
		}
	}
	return false
}

func (s Snapshot) PlacementWorkerCluster() string {
	state := s.MultiKueueState()
	if state == "" {
		return ""
	}
	var worker string
	for _, workload := range s.Workloads {
		if workload.ClusterName == "" {
			continue
		}
		if state != MultiKueueStateReady && workloadMultiKueueReady(workload) {
			continue
		}
		if worker == "" {
			worker = workload.ClusterName
			continue
		}
		if worker != workload.ClusterName {
			return ""
		}
	}
	return worker
}

func (s Snapshot) SelectedWorkerCluster() string {
	for _, w := range s.Workloads {
		if w.ClusterName != "" {
			return w.ClusterName
		}
	}
	return ""
}

func (s Snapshot) NominatedWorkerClusters() []string {
	seen := make(map[string]bool)
	var out []string
	for _, w := range s.Workloads {
		for _, name := range w.NominatedClusterNames {
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func (s Snapshot) AdmissionCheckSummaries() []AdmissionCheckSummary {
	var out []AdmissionCheckSummary
	for _, w := range s.Workloads {
		for _, ac := range w.AdmissionChecks {
			out = append(out, AdmissionCheckSummary{WorkloadName: w.Name, AdmissionCheck: ac})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].WorkloadName != out[j].WorkloadName {
			return out[i].WorkloadName < out[j].WorkloadName
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func multiKueueAdmissionChecks(workload Workload) []AdmissionCheck {
	out := make([]AdmissionCheck, 0, len(workload.AdmissionChecks))
	for _, check := range workload.AdmissionChecks {
		if isMultiKueueAdmissionCheck(check) {
			out = append(out, check)
		}
	}
	return out
}

func isMultiKueueAdmissionCheck(check AdmissionCheck) bool {
	controller := strings.TrimSpace(check.ControllerName)
	if controller != "" {
		return controller == multiKueueControllerName
	}
	return check.ControllerLookupFailed && check.Name == multiKueueAdmissionCheckName
}
