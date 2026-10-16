// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package queuequota

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"k8s.io/apimachinery/pkg/api/resource"
)

const SchemaVersion = "tau.workspace.quota.v1"

type Report struct {
	Schema       string    `json:"schema"`
	Workspace    string    `json:"workspace,omitempty"`
	Namespace    string    `json:"namespace,omitempty"`
	LocalQueue   string    `json:"localQueue,omitempty"`
	ClusterQueue string    `json:"clusterQueue"`
	Workloads    Workloads `json:"workloads"`
	Flavors      []Flavor  `json:"flavors"`
	Warnings     []string  `json:"warnings,omitempty"`
}

type Workloads struct {
	Found     bool `json:"found"`
	Pending   int  `json:"pending"`
	Admitted  int  `json:"admitted"`
	Reserving int  `json:"reserving"`
}

type Flavor struct {
	Name        string            `json:"name"`
	FlavorFound bool              `json:"flavorFound"`
	NodeLabels  map[string]string `json:"nodeLabels,omitempty"`
	Tolerations []Toleration      `json:"tolerations,omitempty"`
	Resources   []ResourceQuota   `json:"resources"`
}

type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

type ResourceQuota struct {
	Name           string `json:"name"`
	Nominal        string `json:"nominal"`
	BorrowingLimit string `json:"borrowingLimit,omitempty"`
	LendingLimit   string `json:"lendingLimit,omitempty"`
	Reserved       string `json:"reserved,omitempty"`
	Used           string `json:"used,omitempty"`
	Remaining      string `json:"remaining"`
	Borrowed       string `json:"borrowed,omitempty"`
}

type Input struct {
	Workspace       string
	Namespace       string
	LocalQueue      string
	ClusterQueue    string
	ClusterQueueRaw []byte
	LocalQueueRaw   []byte
	FlavorsRaw      map[string][]byte
}

func Build(in Input) (Report, error) {
	var cq clusterQueueDoc
	if err := json.Unmarshal(in.ClusterQueueRaw, &cq); err != nil {
		return Report{}, fmt.Errorf("parse ClusterQueue %s: %w", in.ClusterQueue, err)
	}

	name := strings.TrimSpace(in.ClusterQueue)
	if name == "" {
		name = strings.TrimSpace(cq.Metadata.Name)
	}
	report := Report{
		Schema:       SchemaVersion,
		Workspace:    strings.TrimSpace(in.Workspace),
		Namespace:    strings.TrimSpace(in.Namespace),
		LocalQueue:   strings.TrimSpace(in.LocalQueue),
		ClusterQueue: name,
		Flavors:      []Flavor{},
	}

	if len(in.LocalQueueRaw) > 0 {
		var lq localQueueDoc
		if err := json.Unmarshal(in.LocalQueueRaw, &lq); err != nil {
			return Report{}, fmt.Errorf("parse LocalQueue %s: %w", in.LocalQueue, err)
		}
		report.Workloads = Workloads{
			Found:     true,
			Pending:   lq.Status.PendingWorkloads,
			Admitted:  lq.Status.AdmittedWorkloads,
			Reserving: lq.Status.ReservingWorkloads,
		}
	} else if report.LocalQueue != "" {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"LocalQueue %q was not readable; admitted/pending counts are unavailable", report.LocalQueue))
	}

	reserved := indexFlavorStatus(cq.Status.FlavorsReservation)
	used := indexFlavorStatus(cq.Status.FlavorsUsage)

	for _, flavorName := range cq.flavorNames() {
		flavor := Flavor{Name: flavorName, Resources: []ResourceQuota{}}
		raw, ok := in.FlavorsRaw[flavorName]
		if ok && len(raw) > 0 {
			var doc resourceFlavorDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				return Report{}, fmt.Errorf("parse ResourceFlavor %s: %w", flavorName, err)
			}
			flavor.FlavorFound = true
			flavor.NodeLabels = doc.Spec.NodeLabels
			for _, t := range doc.Spec.Tolerations {
				flavor.Tolerations = append(flavor.Tolerations, Toleration(t))
			}
		} else {
			report.Warnings = append(report.Warnings, fmt.Sprintf(
				"ResourceFlavor %q was not readable; its node labels and tolerations are unavailable", flavorName))
		}

		for _, rq := range cq.quotasFor(flavorName) {
			flavor.Resources = append(flavor.Resources, buildResourceQuota(
				rq,
				reserved[flavorResourceKey{flavorName, rq.Name}],
				used[flavorResourceKey{flavorName, rq.Name}],
			))
		}
		report.Flavors = append(report.Flavors, flavor)
	}
	return report, nil
}

func buildResourceQuota(rq flavorResourceQuota, reserved, used string) ResourceQuota {
	out := ResourceQuota{
		Name:           rq.Name,
		Nominal:        canonical(rq.Name, rq.NominalQuota),
		BorrowingLimit: canonical(rq.Name, rq.BorrowingLimit),
		LendingLimit:   canonical(rq.Name, rq.LendingLimit),
		Reserved:       canonical(rq.Name, reserved),
		Used:           canonical(rq.Name, used),
	}
	out.Remaining = subtractFloored(rq.Name, out.Nominal, out.Reserved)
	if borrowed := subtractFloored(rq.Name, out.Reserved, out.Nominal); borrowed != "" && borrowed != "0" {
		out.Borrowed = borrowed
	}
	return out
}

func canonical(resourceName, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return value
	}
	if isByteScaleResource(resourceName) {
		return resource.NewQuantity(q.Value(), resource.BinarySI).String()
	}
	return q.String()
}

func isByteScaleResource(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	return name == "memory" ||
		name == "storage" ||
		name == "ephemeral-storage" ||
		strings.HasSuffix(name, "-storage")
}

func subtractFloored(resourceName, a, b string) string {
	if strings.TrimSpace(a) == "" {
		return ""
	}
	left, err := resource.ParseQuantity(a)
	if err != nil {
		return ""
	}
	if strings.TrimSpace(b) == "" {
		return canonical(resourceName, a)
	}
	right, err := resource.ParseQuantity(b)
	if err != nil {
		return canonical(resourceName, a)
	}
	if left.Cmp(right) <= 0 {
		return "0"
	}
	left.Sub(right)
	return canonical(resourceName, left.String())
}

func RenderTable(r Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Workspace:     %s\n", dash(r.Workspace))
	fmt.Fprintf(&b, "Namespace:     %s\n", dash(r.Namespace))
	fmt.Fprintf(&b, "LocalQueue:    %s\n", dash(r.LocalQueue))
	fmt.Fprintf(&b, "ClusterQueue:  %s\n", dash(r.ClusterQueue))
	if r.Workloads.Found {
		fmt.Fprintf(&b, "Workloads:     %d admitted, %d pending, %d reserving\n",
			r.Workloads.Admitted, r.Workloads.Pending, r.Workloads.Reserving)
	} else {
		fmt.Fprintf(&b, "Workloads:     -\n")
	}

	if len(r.Flavors) == 0 {
		b.WriteString("\nClusterQueue declares no resource flavors.\n")
		return b.String()
	}

	b.WriteString("\nQuota by flavor:\n")
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FLAVOR\tRESOURCE\tNOMINAL\tRESERVED\tUSED\tREMAINING\tBORROWING_LIMIT")
	for _, f := range r.Flavors {
		if len(f.Resources) == 0 {
			fmt.Fprintf(tw, "%s\t-\t-\t-\t-\t-\t-\n", f.Name)
			continue
		}
		for _, q := range f.Resources {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				f.Name, q.Name,
				dash(q.Nominal), dash(q.Reserved), dash(q.Used),
				dash(q.Remaining), dash(q.BorrowingLimit))
		}
	}
	tw.Flush()

	b.WriteString("\nFlavor placement:\n")
	for _, f := range r.Flavors {
		fmt.Fprintf(&b, "  %s\n", f.Name)
		if !f.FlavorFound {
			b.WriteString("    (ResourceFlavor not readable)\n")
			continue
		}
		if len(f.NodeLabels) == 0 {
			b.WriteString("    nodeLabels:  (none)\n")
		} else {
			fmt.Fprintf(&b, "    nodeLabels:  %s\n", strings.Join(sortedLabels(f.NodeLabels), ", "))
		}
		if len(f.Tolerations) == 0 {
			b.WriteString("    tolerations: (none)\n")
		} else {
			for i, t := range f.Tolerations {
				label := "tolerations:"
				if i > 0 {
					label = "            "
				}
				fmt.Fprintf(&b, "    %s %s\n", label, formatToleration(t))
			}
		}
	}

	if len(r.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  %s\n", w)
		}
	}
	return b.String()
}

func formatToleration(t Toleration) string {
	parts := []string{}
	for _, part := range []string{t.Key, t.Operator, t.Value, t.Effect} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "(empty)"
	}
	return strings.Join(parts, " ")
}

func sortedLabels(labels map[string]string) []string {
	out := make([]string, 0, len(labels))
	for k, v := range labels {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
