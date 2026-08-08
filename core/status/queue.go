// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import runtopology "github.com/Azure/taugrid/core/topology"

func (s Snapshot) ManagerLocalQueue() string {
	return firstNonEmpty(label(s, runtopology.QueueLabel), annotationOrDefault(s, runtopology.AnnotationTopologyQueue, ""))
}

func (s Snapshot) EffectiveLocalQueue() string {
	return firstNonEmpty(workloadQueue(s), s.ManagerLocalQueue())
}

func (s Snapshot) IsKueueManaged() bool {
	return s.ManagerLocalQueue() != ""
}
