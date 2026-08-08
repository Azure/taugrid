package jobrender

import "github.com/Azure/taugrid/core/topology"

// ApplyTopologyOptions copies resolved topology options into Job render options.
func ApplyTopologyOptions(o *Options, top topology.Options) {
	o.Team = top.Team
	o.Lane = top.Lane
	o.Mode = top.Mode
	o.Topology = top.Placement
	o.Shape = top.Shape
	o.GPUClass = top.GPUClass
	o.CheckpointEvery = top.CheckpointEvery
	o.QueueName = top.QueueName
	o.PriorityTier = top.PriorityTier
	o.RequiredTopology = top.RequiredTopology
	o.WorkloadPriorityClassName = top.WorkloadPriorityClassName
	o.PodPriorityClassName = top.PodPriorityClassName
	o.DisableKueueTopologyAnnotations = top.DisableKueueTopologyAnnotations
	o.DisableDefaultPriorities = top.DisableDefaultPriorities
}
