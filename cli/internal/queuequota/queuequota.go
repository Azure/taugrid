// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package queuequota builds a read-only researcher view of the Kueue quota
// backing one workspace: what the workspace's ClusterQueue nominally holds per
// ResourceFlavor, how much of it is currently reserved and used, and which
// nodes each flavor selects.
//
// It is deliberately separate from core/queue. That package is topology-policy
// driven — it groups Tau presets into queues and accounts only for GPUs. This
// one starts from a single named ClusterQueue and reports every resource in it
// (cpu, memory, and GPU alike), which is what a researcher needs in order to
// size a config's compute block. It has no portal consumer, so per cli/AGENTS.md
// it stays in the CLI module rather than widening core.
package queuequota

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/tabwriter"

	"k8s.io/apimachinery/pkg/api/resource"
)

// SchemaVersion identifies the JSON shape emitted by `tau workspace quota show`.
const SchemaVersion = "tau.workspace.quota.v1"

// Report is the machine-readable quota view for one workspace.
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

// Workloads is the LocalQueue's workload census. Counts are only meaningful
// when Found is true; a missing LocalQueue must not read as "zero queued work".
type Workloads struct {
	Found     bool `json:"found"`
	Pending   int  `json:"pending"`
	Admitted  int  `json:"admitted"`
	Reserving int  `json:"reserving"`
}

// Flavor is one ResourceFlavor's slice of the ClusterQueue.
type Flavor struct {
	Name string `json:"name"`
	// FlavorFound distinguishes "the ResourceFlavor object was not readable"
	// from "it selects no nodes". Reporting an empty nodeLabels map for an
	// unreadable flavor would claim it schedules anywhere.
	FlavorFound bool              `json:"flavorFound"`
	NodeLabels  map[string]string `json:"nodeLabels,omitempty"`
	Tolerations []Toleration      `json:"tolerations,omitempty"`
	Resources   []ResourceQuota   `json:"resources"`
}

// Toleration mirrors the ResourceFlavor toleration fields researchers need to
// understand why their pods landed on a tainted node pool.
type Toleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// ResourceQuota is one resource's quota within one flavor. Quantities are kept
// in canonical Kubernetes string form ("160Gi", not a byte count) so the output
// can be pasted straight back into a Tau config's compute block.
type ResourceQuota struct {
	Name           string `json:"name"`
	Nominal        string `json:"nominal"`
	BorrowingLimit string `json:"borrowingLimit,omitempty"`
	LendingLimit   string `json:"lendingLimit,omitempty"`
	Reserved       string `json:"reserved,omitempty"`
	Used           string `json:"used,omitempty"`
	// Remaining is nominal minus reserved, floored at zero. It is informational
	// only: Kueue is a queueing system, so a request larger than the remainder
	// is not an error, it simply waits.
	Remaining string `json:"remaining"`
	Borrowed  string `json:"borrowed,omitempty"`
}

// Input is the raw kubectl JSON the report is built from. Keeping Build a pure
// function of bytes is what makes the arithmetic and rendering testable without
// a cluster.
type Input struct {
	Workspace       string
	Namespace       string
	LocalQueue      string
	ClusterQueue    string
	ClusterQueueRaw []byte
	// LocalQueueRaw is optional. A researcher may be able to read the
	// ClusterQueue without being able to read the LocalQueue.
	LocalQueueRaw []byte
	// FlavorsRaw maps ResourceFlavor name to its JSON. Flavors named by the
	// ClusterQueue but absent here are reported as not found.
	FlavorsRaw map[string][]byte
}

// Build parses Kueue objects into a Report. It never fails on missing optional
// inputs; it records them as warnings so the researcher can tell "I lack RBAC
// for this" apart from "there is none".
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

// canonical normalizes a quantity to the form a researcher would write in a Tau
// config. Kueue echoes byte-scale resources in whichever suffix the manifest
// used, so a ClusterQueue can report memory as "17179869184" while the config
// next to it says "16Gi". Rendering those side by side is what makes quota
// output unusable, so byte-scale resources are always shown in binary SI.
func canonical(resourceName, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	q, err := resource.ParseQuantity(value)
	if err != nil {
		// Preserve whatever the API server said rather than dropping it; an
		// unparseable quantity is still more informative than a blank cell.
		return value
	}
	if isByteScaleResource(resourceName) {
		return resource.NewQuantity(q.Value(), resource.BinarySI).String()
	}
	return q.String()
}

// isByteScaleResource reports whether a Kubernetes resource name is measured in
// bytes, and therefore reads naturally in binary SI. Counted resources such as
// cpu and nvidia.com/gpu must not be reformatted that way — 2048 CPUs is not
// "2Ki".
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

// subtractFloored returns a-b, never negative. An empty minuend means the
// ClusterQueue declared no quota for this resource, which is not the same as
// zero remaining, so it stays empty.
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

// RenderTable renders the researcher-facing table.
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
