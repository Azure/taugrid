// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kueueapi"
)

type wlList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			QueueName string `json:"queueName"`
		} `json:"spec"`
		Status struct {
			Conditions            []kueueapi.Condition `json:"conditions"`
			ClusterName           string               `json:"clusterName"`
			NominatedClusterNames []string             `json:"nominatedClusterNames"`
			AdmissionChecks       []struct {
				Name               string    `json:"name"`
				State              string    `json:"state"`
				Message            string    `json:"message"`
				LastTransitionTime time.Time `json:"lastTransitionTime"`
			} `json:"admissionChecks"`
		} `json:"status"`
	} `json:"items"`
}

type admissionCheckObj struct {
	Spec struct {
		ControllerName string `json:"controllerName"`
	} `json:"spec"`
}

type admissionCheckLookup struct {
	ControllerName string
	LookupFailed   bool
}

func hydrateWorkloads(data []byte) []Workload {
	var l wlList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil
	}
	out := make([]Workload, 0, len(l.Items))
	for _, it := range l.Items {
		w := Workload{Name: it.Metadata.Name, Queue: it.Spec.QueueName, Phase: "Pending"}
		for _, c := range it.Status.Conditions {
			switch c.Type {
			case "Admitted":
				if c.Status == "True" {
					w.Admitted = true
					if w.Phase == "Pending" {
						w.Phase = "Admitted"
					}
				}
			case "Finished":
				if c.Status == "True" {
					w.Phase = "Finished"
					if c.Reason != "" {
						w.Reason = c.Reason
					}
				}
			case "QuotaReserved":
				if c.Status == "True" && w.Phase == "Pending" {
					w.Phase = "QuotaReserved"
				}
			}
			if strings.Contains(strings.ToLower(c.Type), "evict") || strings.Contains(strings.ToLower(c.Reason), "preempt") {
				w.Preemption = firstNonEmpty(c.Reason, c.Message, c.Type)
			}
		}
		if w.waiting() {
			w.Reason, w.Message = kueueapi.PendingCause(it.Status.Conditions)
		}
		w.ClusterName = it.Status.ClusterName
		w.NominatedClusterNames = sortedUniqueStrings(it.Status.NominatedClusterNames)
		for _, ac := range it.Status.AdmissionChecks {
			if ac.Name == "" {
				continue
			}
			w.AdmissionChecks = append(w.AdmissionChecks, AdmissionCheck{
				Name:               ac.Name,
				State:              ac.State,
				Message:            ac.Message,
				LastTransitionTime: ac.LastTransitionTime,
			})
		}
		sort.Slice(w.AdmissionChecks, func(i, j int) bool {
			return w.AdmissionChecks[i].Name < w.AdmissionChecks[j].Name
		})
		out = append(out, w)
	}
	return out
}

func hydrateAdmissionCheckControllers(ctx context.Context, r rawRunner, workloads []Workload) {
	if r == nil || len(workloads) == 0 {
		return
	}
	controllers := fetchAdmissionCheckControllers(ctx, r, admissionCheckNames(workloads))
	if len(controllers) == 0 {
		return
	}
	for wi := range workloads {
		for ci := range workloads[wi].AdmissionChecks {
			lookup, ok := controllers[workloads[wi].AdmissionChecks[ci].Name]
			if !ok {
				continue
			}
			workloads[wi].AdmissionChecks[ci].ControllerName = lookup.ControllerName
			workloads[wi].AdmissionChecks[ci].ControllerLookupFailed = lookup.LookupFailed
		}
	}
}

// markRunLogsMultiKueueAdmissionCheckFallbacks preserves the pinned exact-name
// MultiKueue fallback for minimal run-logs snapshots, which intentionally skip
// AdmissionCheck object lookups to match the read-only manager viewer RBAC.
func markRunLogsMultiKueueAdmissionCheckFallbacks(workloads []Workload) {
	for wi := range workloads {
		for ci := range workloads[wi].AdmissionChecks {
			if workloads[wi].AdmissionChecks[ci].Name != multiKueueAdmissionCheckName {
				continue
			}
			if strings.TrimSpace(workloads[wi].AdmissionChecks[ci].ControllerName) != "" {
				continue
			}
			workloads[wi].AdmissionChecks[ci].ControllerLookupFailed = true
		}
	}
}

func admissionCheckNames(workloads []Workload) []string {
	seen := make(map[string]bool)
	names := make([]string, 0)
	for _, workload := range workloads {
		for _, check := range workload.AdmissionChecks {
			if check.Name == "" || seen[check.Name] {
				continue
			}
			seen[check.Name] = true
			names = append(names, check.Name)
		}
	}
	sort.Strings(names)
	return names
}

func fetchAdmissionCheckControllers(ctx context.Context, r rawRunner, names []string) map[string]admissionCheckLookup {
	controllers := make(map[string]admissionCheckLookup, len(names))
	for _, name := range names {
		out, err := r.Raw(ctx, []string{"get", "admissioncheck", name, "-o", "json"}, nil)
		if err != nil {
			controllers[name] = admissionCheckLookup{LookupFailed: true}
			continue
		}
		var obj admissionCheckObj
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			controllers[name] = admissionCheckLookup{LookupFailed: true}
			continue
		}
		controllers[name] = admissionCheckLookup{ControllerName: strings.TrimSpace(obj.Spec.ControllerName)}
	}
	return controllers
}

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}
