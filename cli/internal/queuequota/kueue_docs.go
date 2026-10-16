// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package queuequota

import (
	"encoding/json"
	"sort"
)

type clusterQueueDoc struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ResourceGroups []struct {
			CoveredResources []string `json:"coveredResources"`
			Flavors          []struct {
				Name      string                `json:"name"`
				Resources []flavorResourceQuota `json:"resources"`
			} `json:"flavors"`
		} `json:"resourceGroups"`
	} `json:"spec"`
	Status struct {
		FlavorsReservation []flavorStatus `json:"flavorsReservation"`
		FlavorsUsage       []flavorStatus `json:"flavorsUsage"`
	} `json:"status"`
}

type flavorResourceQuota struct {
	Name           string `json:"name"`
	NominalQuota   string `json:"nominalQuota"`
	BorrowingLimit string `json:"borrowingLimit,omitempty"`
	LendingLimit   string `json:"lendingLimit,omitempty"`
}

type flavorStatus struct {
	Name      string `json:"name"`
	Resources []struct {
		Name     string `json:"name"`
		Total    string `json:"total"`
		Borrowed string `json:"borrowed,omitempty"`
	} `json:"resources"`
}

type flavorResourceKey struct {
	flavor   string
	resource string
}

func indexFlavorStatus(entries []flavorStatus) map[flavorResourceKey]string {
	index := map[flavorResourceKey]string{}
	for _, entry := range entries {
		for _, r := range entry.Resources {
			index[flavorResourceKey{entry.Name, r.Name}] = r.Total
		}
	}
	return index
}

func (cq clusterQueueDoc) flavorNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if f.Name != "" && !seen[f.Name] {
				seen[f.Name] = true
				names = append(names, f.Name)
			}
		}
	}
	sort.Strings(names)
	return names
}

func (cq clusterQueueDoc) quotasFor(flavor string) []flavorResourceQuota {
	seen := map[string]bool{}
	var out []flavorResourceQuota
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if f.Name != flavor {
				continue
			}
			for _, r := range f.Resources {
				if r.Name == "" || seen[r.Name] {
					continue
				}
				seen[r.Name] = true
				out = append(out, r)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func flavorNamesFrom(raw []byte) ([]string, error) {
	var cq clusterQueueDoc
	if err := json.Unmarshal(raw, &cq); err != nil {
		return nil, err
	}
	return cq.flavorNames(), nil
}

type localQueueDoc struct {
	Status struct {
		PendingWorkloads   int `json:"pendingWorkloads"`
		AdmittedWorkloads  int `json:"admittedWorkloads"`
		ReservingWorkloads int `json:"reservingWorkloads"`
	} `json:"status"`
}

type resourceFlavorDoc struct {
	Spec struct {
		NodeLabels  map[string]string `json:"nodeLabels"`
		Tolerations []struct {
			Key      string `json:"key,omitempty"`
			Operator string `json:"operator,omitempty"`
			Value    string `json:"value,omitempty"`
			Effect   string `json:"effect,omitempty"`
		} `json:"tolerations"`
	} `json:"spec"`
}
