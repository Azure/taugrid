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

const DefaultLocalQueueNamespace = workloadmeta.DefaultWorkspaceName

type Policy struct {
	Presets map[string]Preset
}

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

func (p Policy) Names() []string {
	names := make([]string, 0, len(p.Presets))
	for name := range p.Presets {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

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
