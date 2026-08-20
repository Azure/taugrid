// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package topology

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/Azure/taugrid/core/workloadmeta"
)

const LabelPreset = workloadmeta.LabelPreset

// DefaultLocalQueueNamespace is the default workspace namespace used when a
// live queue observation does not provide an explicit namespace.
const DefaultLocalQueueNamespace = workloadmeta.DefaultWorkspaceName

// Policy is an in-memory projection used only to aggregate live LocalQueue and
// ClusterQueue observations. It is not a workload-profile catalog, file format,
// loader, or fallback source. Authoritative workload intent comes from the
// singleton TauCluster spec/status contract.
type Policy struct {
	Presets map[string]Preset
}

// Preset is one observed queue grouping in Policy. It deliberately remains a
// small projection for the existing queue-pressure API; it is not loaded from
// disk and must not be used for workload-profile selection.
type Preset struct {
	Name                      string
	Description               string
	Team                      string
	Lane                      string
	Mode                      string
	Placement                 string
	Shape                     string
	GPUClass                  string
	QueueName                 string
	ClusterQueue              string
	Namespace                 string
	ResourceFlavor            string
	TopologyName              string
	WorkloadPriorityClassName string
	PodPriorityClassName      string
	Workers                   int
	Disabled                  bool
	Explain                   string
}

// Names returns stable observation-group ordering.
func (p Policy) Names() []string {
	names := make([]string, 0, len(p.Presets))
	for name := range p.Presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GPUCountFromShape parses a shape such as "2xa100-80gb".
func GPUCountFromShape(shape string) (int, bool, error) {
	count, _, ok := strings.Cut(shape, "x")
	if !ok || count == "" {
		return 0, false, nil
	}
	n, err := strconv.Atoi(count)
	if err != nil || n <= 0 {
		return 0, false, fmt.Errorf("shape %q: GPU count prefix must be a positive integer", shape)
	}
	return n, true, nil
}
