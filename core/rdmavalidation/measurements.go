// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package rdmavalidation

import (
	"sort"
)

func CalculateSummary(values []float64) SummaryStats {
	if len(values) == 0 {
		return SummaryStats{}
	}
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	sum := 0.0
	for _, value := range sorted {
		sum += value
	}
	median := sorted[len(sorted)/2]
	if len(sorted)%2 == 0 {
		median = (sorted[len(sorted)/2-1] + sorted[len(sorted)/2]) / 2
	}
	minimum, maximum, mean := sorted[0], sorted[len(sorted)-1], sum/float64(len(sorted))
	return SummaryStats{
		Count: len(sorted),
		Min:   &minimum, Max: &maximum, Mean: &mean, Median: &median,
	}
}

func NewMeasurements(samples []BandwidthMeasurement, maxRankTimeSeconds float64) Measurements {
	alg := make([]float64, 0, len(samples))
	bus := make([]float64, 0, len(samples))
	for _, sample := range samples {
		alg = append(alg, sample.AlgBWGbps)
		bus = append(bus, sample.BusBWGbps)
	}
	return Measurements{
		Samples:            append([]BandwidthMeasurement(nil), samples...),
		AlgBWGbps:          CalculateSummary(alg),
		BusBWGbps:          CalculateSummary(bus),
		MaxRankTimeSeconds: &maxRankTimeSeconds,
	}
}
