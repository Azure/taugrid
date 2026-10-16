// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package checks

import (
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/reader"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

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

func deltasAboveThreshold(samples []store.Sample, threshold float64) []gpuDelta {
	var result []gpuDelta
	for gpu, delta := range reader.ComputeDeltas(samples) {
		if delta > threshold {
			result = append(result, gpuDelta{gpu: gpu, value: delta})
		}
	}
	return result
}
