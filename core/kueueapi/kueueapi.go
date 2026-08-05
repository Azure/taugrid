// Package kueueapi holds the Kueue API JSON shapes and GPU quota accounting
// shared by the CLI's queue validation path and the portal's queue snapshot
// path. Both decode the same ClusterQueue/LocalQueue/ResourceFlavor documents
// and must agree on how NVIDIA GPU quota is counted, so that logic lives here
// rather than being duplicated per consumer.
package kueueapi

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// GPUResource is the Kueue DRA accounting resource for NVIDIA GPUs.
	GPUResource = "gpu.nvidia.com"
	// GPUResourceDevicePlugin is the standard NVIDIA device-plugin resource
	// name. The finetune flow uses this after the DRA->device-plugin migration,
	// so queue accounting must count it alongside the DRA resource.
	GPUResourceDevicePlugin = "nvidia.com/gpu"
)

// GPUResourceNames lists every resource name that represents an NVIDIA GPU for
// queue accounting: the DRA resource and the device-plugin resource.
var GPUResourceNames = []string{GPUResource, GPUResourceDevicePlugin}

// isGPUResource reports whether a Kueue resource name accounts for NVIDIA GPUs.
func isGPUResource(name string) bool {
	for _, n := range GPUResourceNames {
		if name == n {
			return true
		}
	}
	return false
}

type Quantity struct {
	raw string
}

func (q *Quantity) UnmarshalJSON(data []byte) error {
	var s string
	if err := json.Unmarshal(data, &s); err == nil {
		q.raw = s
		return nil
	}
	var f float64
	if err := json.Unmarshal(data, &f); err != nil {
		return err
	}
	q.raw = strconv.FormatFloat(f, 'f', -1, 64)
	return nil
}

func (q Quantity) Int64() int64 {
	v := strings.TrimSpace(q.raw)
	if v == "" {
		return 0
	}
	parsed, err := resource.ParseQuantity(v)
	if err != nil {
		return 0
	}
	return parsed.Value()
}

type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type LocalQueueList struct {
	Items []LocalQueue `json:"items"`
}

type LocalQueue struct {
	Metadata struct {
		Name      string            `json:"name"`
		Namespace string            `json:"namespace"`
		Labels    map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ClusterQueue string `json:"clusterQueue"`
	} `json:"spec"`
	Status struct {
		PendingWorkloads   int         `json:"pendingWorkloads"`
		AdmittedWorkloads  int         `json:"admittedWorkloads"`
		ReservingWorkloads int         `json:"reservingWorkloads"`
		Conditions         []Condition `json:"conditions"`
	} `json:"status"`
}

type ClusterQueueList struct {
	Items []ClusterQueue `json:"items"`
}

type ClusterQueue struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		ResourceGroups []struct {
			Flavors []struct {
				Name      string `json:"name"`
				Resources []struct {
					Name           string   `json:"name"`
					NominalQuota   Quantity `json:"nominalQuota"`
					BorrowingLimit Quantity `json:"borrowingLimit"`
				} `json:"resources"`
			} `json:"flavors"`
		} `json:"resourceGroups"`
	} `json:"spec"`
	Status struct {
		Conditions         []Condition    `json:"conditions"`
		FlavorsReservation []FlavorStatus `json:"flavorsReservation"`
		FlavorsUsage       []FlavorStatus `json:"flavorsUsage"`
	} `json:"status"`
}

type FlavorStatus struct {
	Name      string `json:"name"`
	Resources []struct {
		Name     string   `json:"name"`
		Total    Quantity `json:"total"`
		Borrowed Quantity `json:"borrowed"`
	} `json:"resources"`
}

func (cq ClusterQueue) HasResourceFlavor(flavor string) bool {
	if flavor == "" {
		return false
	}
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if f.Name == flavor {
				return true
			}
		}
	}
	return false
}

// NominalGPU reports configured GPU quota. When flavor is empty it sums across
// every ResourceFlavor, which is a pool total and deliberately not the same
// number as MaxGPUCapacity's empty-flavor case: a pool total answers "how many
// GPUs does this queue own", never "how large a workload can this queue admit".
// Do not reuse it as a per-workload ceiling -- Kueue assigns a workload to
// exactly one flavor, so the ceiling is a single flavor's capacity. See
// MaxGPUCapacity for that, and for the bug summing here once caused there.
func (cq ClusterQueue) NominalGPU(flavor string) (int64, bool) {
	if flavor == "" {
		var total int64
		var found bool
		for _, rg := range cq.Spec.ResourceGroups {
			for _, f := range rg.Flavors {
				for _, r := range f.Resources {
					if isGPUResource(r.Name) {
						total += r.NominalQuota.Int64()
						found = true
					}
				}
			}
		}
		return total, found
	}
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if f.Name != flavor {
				continue
			}
			var total int64
			var found bool
			for _, r := range f.Resources {
				if isGPUResource(r.Name) {
					total += r.NominalQuota.Int64()
					found = true
				}
			}
			if found {
				return total, true
			}
		}
	}
	return 0, false
}

// MaxGPUCapacity reports the largest GPU request this queue could admit, which
// is what the submission preflight compares a workload against.
//
// When flavor is empty the workload is not pinned to one ResourceFlavor, but
// Kueue still assigns it to exactly one flavor -- it never splits a single
// workload's GPUs across flavors. The ceiling is therefore the largest single
// flavor's capacity, not the sum of all of them. Summing overstated the
// ceiling: live tau-cq has tau-system (0 GPU), nd-h200-v5 (16) and
// ndm-a100-v4 (16), which reported 32, so preflight admitted a 32-GPU request
// that could never be scheduled and the run sat Pending with no explanation
// instead of being rejected outright.
func (cq ClusterQueue) MaxGPUCapacity(flavor, resourceName string) (int64, bool) {
	if flavor == "" {
		var best int64
		var found bool
		for _, rg := range cq.Spec.ResourceGroups {
			for _, f := range rg.Flavors {
				cap, ok := gpuCapacityForFlavor(f, resourceName)
				if !ok {
					continue
				}
				if !found || cap > best {
					best = cap
				}
				found = true
			}
		}
		return best, found
	}
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if f.Name != flavor {
				continue
			}
			return gpuCapacityForFlavor(f, resourceName)
		}
	}
	return 0, false
}

// BestGPUFlavorFor returns the ResourceFlavor with the highest GPU capacity
// among this ClusterQueue's GPU-quota flavors that also satisfy the caller's
// gpu_class constraint.
//
// allowedFlavors is the caller-resolved allow-list of ResourceFlavor names
// that satisfy the request: nil means no gpu_class filter applies (for
// example gpu_class: any, or no gpu_class requested at all) and every
// GPU-quota flavor is a candidate; a non-nil map (even an empty one) means
// only the named flavors qualify. Callers must resolve allowedFlavors by
// comparing each candidate ResourceFlavor's spec.nodeLabels against the
// tau.azure.com/gpu-class contract -- never by matching a substring of the
// flavor's own name. A ClusterQueue's JSON representation does not carry
// ResourceFlavor node labels, so this function cannot do that matching
// itself; see queueresolve.gpuClassAllowedFlavors for the exact-match
// resolution this depends on.
func (cq ClusterQueue) BestGPUFlavorFor(allowedFlavors map[string]bool, resourceName string) (string, int64, bool) {
	type candidate struct {
		name string
		cap  int64
	}
	var candidates []candidate
	for _, rg := range cq.Spec.ResourceGroups {
		for _, f := range rg.Flavors {
			if allowedFlavors != nil && !allowedFlavors[f.Name] {
				continue
			}
			cap, ok := gpuCapacityForFlavor(f, resourceName)
			if !ok || cap <= 0 {
				continue
			}
			candidates = append(candidates, candidate{name: f.Name, cap: cap})
		}
	}
	if len(candidates) == 0 {
		return "", 0, false
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].cap != candidates[j].cap {
			return candidates[i].cap > candidates[j].cap
		}
		return candidates[i].name < candidates[j].name
	})
	return candidates[0].name, candidates[0].cap, true
}

func gpuCapacityForFlavor(f struct {
	Name      string `json:"name"`
	Resources []struct {
		Name           string   `json:"name"`
		NominalQuota   Quantity `json:"nominalQuota"`
		BorrowingLimit Quantity `json:"borrowingLimit"`
	} `json:"resources"`
}, resourceName string) (int64, bool) {
	var total int64
	var found bool
	for _, r := range f.Resources {
		if (resourceName == "" && isGPUResource(r.Name)) || r.Name == resourceName {
			total += r.NominalQuota.Int64() + r.BorrowingLimit.Int64()
			found = true
		}
	}
	return total, found
}

func (cq ClusterQueue) StatusGPU(statuses []FlavorStatus, flavor string) (total, borrowed int64, ok bool) {
	if flavor == "" {
		for _, f := range statuses {
			for _, r := range f.Resources {
				if isGPUResource(r.Name) {
					total += r.Total.Int64()
					borrowed += r.Borrowed.Int64()
					ok = true
				}
			}
		}
		return total, borrowed, ok
	}
	for _, f := range statuses {
		if f.Name != flavor {
			continue
		}
		for _, r := range f.Resources {
			if isGPUResource(r.Name) {
				total += r.Total.Int64()
				borrowed += r.Borrowed.Int64()
				ok = true
			}
		}
		if ok {
			return total, borrowed, true
		}
	}
	return 0, 0, false
}

func SelectorValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, value := range m {
		if strings.TrimSpace(value) != "" {
			out = append(out, strings.TrimSpace(value))
		}
	}
	return out
}

type ResourceFlavor struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		NodeLabels   map[string]string `json:"nodeLabels"`
		NodeTaints   []Taint           `json:"nodeTaints"`
		Tolerations  []Toleration      `json:"tolerations"`
		TopologyName string            `json:"topologyName"`
	} `json:"spec"`
}

type Taint struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Effect string `json:"effect"`
}

type Toleration struct {
	Key      string `json:"key"`
	Operator string `json:"operator"`
	Value    string `json:"value"`
	Effect   string `json:"effect"`
}

// PendingCause explains why Kueue has not admitted a workload, from its
// conditions. Both the CLI status view and the portal queue board show this
// text, so the choice of which condition to believe lives here.
//
// The reason and message always come from the same condition, so the pair
// describes one thing. QuotaReserved is preferred: its reason varies with the
// actual cause (Kueue passes "Pending", "Waiting", or "Inadmissible" to
// UnsetQuotaReservationWithCondition depending on the path) and its message
// carries the flavor-assignment detail — which flavors were tried, which
// nodes were excluded and why. Admitted is the weaker fallback because Kueue
// derives it mechanically from the other conditions, so it only ever reports
// that a reservation is missing ("NoReservation", "UnsatisfiedChecks"), never
// why. Any other non-True condition (Deactivated, Requeued) is the last
// resort. Kueue often sets just one of these while a workload waits, so none
// is assumed present, and each is read independently of its position in the
// list.
func PendingCause(conditions []Condition) (reason, message string) {
	var quota, admitted, other Condition
	for _, c := range conditions {
		if c.Status == "True" {
			continue
		}
		switch c.Type {
		case "QuotaReserved":
			quota = c
		case "Admitted":
			admitted = c
		default:
			if other.Reason == "" && other.Message == "" {
				other = c
			}
		}
	}
	for _, c := range []Condition{quota, admitted, other} {
		if c.Reason != "" || c.Message != "" {
			return c.Reason, c.Message
		}
	}
	return "", ""
}
