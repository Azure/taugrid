// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"sort"
	"strings"
)

// multiKueueControllerName is the MultiKueue admission check controller
// name used by Kueue v0.18.x AdmissionCheck.spec.controllerName.
const multiKueueControllerName = "kueue.x-k8s.io/multikueue"

// multiKueueAdmissionCheckName is the current upstream AdmissionCheck name
// that Kueue's MultiKueue controller installs. When manager-side
// AdmissionCheck lookups fail (the current batch-user RBAC does not grant
// get on admissionchecks.kueue.x-k8s.io), placement logic falls back to this
// exact name only. Activation tests should keep this contract pinned.
const multiKueueAdmissionCheckName = "multikueue"

// multiKueueManagedBy is the spec.managedBy value Kueue's MultiKueue
// admission check controller sets on a manager-cluster Job/RayJob once
// the workload is routed to a worker cluster. See the Kueue and KubeRay
// CRDs (ray.io_rayjobs.yaml: "the managedBy field value must be either
// 'ray.io/kuberay-operator' or 'kueue.x-k8s.io/multikueue'").
const multiKueueManagedBy = multiKueueControllerName

// AdmissionCheckSummary pairs a Workload name with one of its admission
// checks, so callers can report check progress without assuming there is
// exactly one Workload for a job (Fetch tolerates multiples).
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

// IsMultiKueue reports whether this job is being scheduled through
// Kueue's MultiKueue admission check, as evidenced by:
//   - spec.managedBy == "kueue.x-k8s.io/multikueue" on the Job or RayJob, or
//   - manager-visible Workload placement fields (clusterName or
//     nominatedClusterNames), or
//   - any AdmissionCheck resolved to the MultiKueue controller, or
//   - a failed AdmissionCheck lookup on the exact upstream "multikueue"
//     check name (temporary fallback until platform RBAC grants get on
//     admissionchecks.kueue.x-k8s.io and activation tests pin the name).
//
// All of these are manager-visible fields; this never inspects worker
// cluster state. A job with none of these signals is a normal
// single-cluster submission and IsMultiKueue returns false.
func (s Snapshot) IsMultiKueue() bool {
	if s.JobManagedBy == multiKueueManagedBy || s.RayJob.ManagedBy == multiKueueManagedBy {
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

// MultiKueueState aggregates manager-visible placement progress. It never
// consults worker-cluster credentials or objects. Successful controller
// lookups are authoritative; exact-name fallback is used only when the
// controller lookup failed.
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

// ManagerOnlyMultiKueueView reports the manager-cluster view for a MultiKueue
// workload: placement state is visible but no local execution pods exist.
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

// PlacementWorkerCluster returns the unambiguous worker cluster for the current
// aggregate MultiKueue placement state. It matches Tau's renderer semantics:
// once all observed MultiKueue checks are ready it reports the common ready
// worker, otherwise it reports the selected worker from the unfinished
// placement workload. Conflicting cluster assignments return "".
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

// SelectedWorkerCluster returns the worker cluster name Kueue has
// selected for this workload (status.clusterName), or "" if no Workload
// has been assigned yet — either because admission hasn't happened, this
// isn't a MultiKueue job, or the Workload hasn't been fetched.
//
// When Fetch finds multiple Workloads for a job (tolerated but unusual),
// the first Workload reporting a non-empty ClusterName wins; Workloads
// are otherwise returned in API list order, so this is deterministic for
// the common single-Workload case.
func (s Snapshot) SelectedWorkerCluster() string {
	for _, w := range s.Workloads {
		if w.ClusterName != "" {
			return w.ClusterName
		}
	}
	return ""
}

// NominatedWorkerClusters returns the deduplicated, sorted union of
// worker cluster names nominated (but not yet selected) across all known
// Workloads for this job. Returns nil when there are no nominations —
// either because a cluster has already been selected, this isn't a
// MultiKueue job, or nomination hasn't happened yet.
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

// AdmissionCheckSummaries returns every admission check reported across
// all known Workloads, sorted by Workload name then check name for
// deterministic rendering. Returns nil when there are no admission
// checks reported (including for ordinary single-cluster jobs, which
// typically have none).
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
