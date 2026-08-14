// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package checks implements the individual DCGM health check readers.
package checks

import (
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/reader"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// All returns all available readers.
func All() []reader.Reader {
	return []reader.Reader{
		&XIDReader{},
		&ECCReader{},
		&NVLinkReader{},
		&HealthReader{},
		&ThermalReader{},
		&PCIeReader{},
		&ClocksReader{},
		&InfoReader{},
	}
}

// ByName returns the reader with the given name, or nil if not found.
func ByName(name string) reader.Reader {
	for _, r := range All() {
		if r.Name() == name {
			return r
		}
	}
	return nil
}

type gpuDelta struct {
	gpu   int
	value float64
}

// deltasAboveThreshold returns GPU counter deltas strictly greater than threshold.
func deltasAboveThreshold(samples []store.Sample, threshold float64) []gpuDelta {
	var result []gpuDelta
	for gpu, delta := range reader.ComputeDeltas(samples) {
		if delta > threshold {
			result = append(result, gpuDelta{gpu: gpu, value: delta})
		}
	}
	return result
}
