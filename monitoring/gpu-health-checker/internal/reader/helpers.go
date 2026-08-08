// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reader

import (
	"fmt"
	"time"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// applyTolerance determines the exit code based on the number of failed GPUs
// and the configured maxFailedGPUs tolerance. criticalBase is the base exit
// code for failures that exceed tolerance (2 for most checks, but some checks
// are always warning-level).
func applyTolerance(failedGPUs []int, maxFailed int, criticalBase int) int {
	if len(failedGPUs) == 0 {
		return 0
	}
	if len(failedGPUs) <= maxFailed {
		return 1
	}
	return criticalBase
}

// checkStaleness verifies that the most recent sample is not too old.
// Returns a non-OK Result if data is stale, nil otherwise.
func checkStaleness(db *store.DB, maxAge time.Duration) *Result {
	ts, err := db.LatestTimestamp()
	if err != nil || ts == 0 {
		return &Result{
			ExitCode: 1,
			Message:  "no data in database — collector may not be running",
		}
	}
	now := time.Now().Unix()
	if ts > now+60 {
		return &Result{
			ExitCode: 1,
			Message:  "database has future-dated samples (clock skew detected)",
		}
	}
	age := time.Duration(now-ts) * time.Second
	if age > maxAge {
		return &Result{
			ExitCode: 1,
			Message:  fmt.Sprintf("data is stale (age=%s, threshold=%s) — collector may be down", age.Round(time.Second), maxAge),
		}
	}
	return nil
}

// AppendUnique appends v to s only if it is not already present.
func AppendUnique(s []int, v int) []int {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

// ComputeDeltas computes the delta (last - first) of counter values per GPU.
// Returns a map of GPU index → delta. If the counter wraps around (last <
// first, indicating a driver reset), the last value is used as the delta
// since we can't know the pre-reset value.
func ComputeDeltas(samples []store.Sample) map[int]float64 {
	type minMax struct {
		first, last float64
		firstTs     int64
		lastTs      int64
	}
	perGPU := make(map[int]*minMax)
	for _, s := range samples {
		mm, ok := perGPU[s.GPU]
		if !ok {
			perGPU[s.GPU] = &minMax{first: s.Value, last: s.Value, firstTs: s.Timestamp, lastTs: s.Timestamp}
			continue
		}
		if s.Timestamp < mm.firstTs {
			mm.first = s.Value
			mm.firstTs = s.Timestamp
		}
		if s.Timestamp > mm.lastTs {
			mm.last = s.Value
			mm.lastTs = s.Timestamp
		}
	}
	rates := make(map[int]float64)
	for gpu, mm := range perGPU {
		delta := mm.last - mm.first
		if delta > 0 {
			rates[gpu] = delta
		} else if delta < 0 {
			// Counter wrapped around (driver reset). Use the post-reset
			// value as a conservative estimate of errors since reset.
			if mm.last > 0 {
				rates[gpu] = mm.last
			}
		}
	}
	return rates
}
