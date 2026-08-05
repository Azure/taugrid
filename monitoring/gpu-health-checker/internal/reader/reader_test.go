package reader

import (
	"testing"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

func TestComputeDeltas_NormalIncrease(t *testing.T) {
	samples := []store.Sample{
		{Timestamp: 100, GPU: 0, Value: 10},
		{Timestamp: 110, GPU: 0, Value: 20},
		{Timestamp: 120, GPU: 0, Value: 30},
	}
	rates := ComputeDeltas(samples)
	if got := rates[0]; got != 20 {
		t.Errorf("GPU 0 rate = %v, want 20", got)
	}
}

func TestComputeDeltas_NoChange(t *testing.T) {
	samples := []store.Sample{
		{Timestamp: 100, GPU: 0, Value: 5},
		{Timestamp: 110, GPU: 0, Value: 5},
	}
	rates := ComputeDeltas(samples)
	if _, ok := rates[0]; ok {
		t.Errorf("GPU 0 should not appear in rates when delta=0")
	}
}

func TestComputeDeltas_CounterWraparound(t *testing.T) {
	// Simulates a driver reset: counter goes from 1000 down to 3.
	samples := []store.Sample{
		{Timestamp: 100, GPU: 0, Value: 1000},
		{Timestamp: 110, GPU: 0, Value: 3},
	}
	rates := ComputeDeltas(samples)
	if got := rates[0]; got != 3 {
		t.Errorf("GPU 0 rate = %v, want 3 (post-reset value)", got)
	}
}

func TestComputeDeltas_WraparoundToZero(t *testing.T) {
	// Counter resets to 0: should not report a rate.
	samples := []store.Sample{
		{Timestamp: 100, GPU: 0, Value: 500},
		{Timestamp: 110, GPU: 0, Value: 0},
	}
	rates := ComputeDeltas(samples)
	if _, ok := rates[0]; ok {
		t.Errorf("GPU 0 should not appear when counter resets to 0")
	}
}

func TestComputeDeltas_MultipleGPUs(t *testing.T) {
	samples := []store.Sample{
		{Timestamp: 100, GPU: 0, Value: 0},
		{Timestamp: 100, GPU: 1, Value: 10},
		{Timestamp: 110, GPU: 0, Value: 5},
		{Timestamp: 110, GPU: 1, Value: 10},
	}
	rates := ComputeDeltas(samples)
	if got := rates[0]; got != 5 {
		t.Errorf("GPU 0 rate = %v, want 5", got)
	}
	if _, ok := rates[1]; ok {
		t.Errorf("GPU 1 should not appear (no change)")
	}
}

func TestComputeDeltas_OutOfOrderTimestamps(t *testing.T) {
	// Samples arrive out of order; should still pick min/max correctly.
	samples := []store.Sample{
		{Timestamp: 120, GPU: 0, Value: 30},
		{Timestamp: 100, GPU: 0, Value: 10},
		{Timestamp: 110, GPU: 0, Value: 20},
	}
	rates := ComputeDeltas(samples)
	if got := rates[0]; got != 20 {
		t.Errorf("GPU 0 rate = %v, want 20", got)
	}
}

func TestComputeDeltas_EmptySamples(t *testing.T) {
	rates := ComputeDeltas(nil)
	if len(rates) != 0 {
		t.Errorf("expected empty rates, got %v", rates)
	}
}

func TestComputeDeltas_SingleSample(t *testing.T) {
	samples := []store.Sample{
		{Timestamp: 100, GPU: 0, Value: 42},
	}
	rates := ComputeDeltas(samples)
	if _, ok := rates[0]; ok {
		t.Errorf("single sample should not produce a rate")
	}
}

func TestAppendUnique(t *testing.T) {
	s := []int{1, 2, 3}
	s = AppendUnique(s, 2)
	if len(s) != 3 {
		t.Errorf("AppendUnique duplicate: len=%d, want 3", len(s))
	}
	s = AppendUnique(s, 4)
	if len(s) != 4 {
		t.Errorf("AppendUnique new: len=%d, want 4", len(s))
	}
}

func TestAppendUnique_Empty(t *testing.T) {
	s := AppendUnique(nil, 1)
	if len(s) != 1 || s[0] != 1 {
		t.Errorf("AppendUnique nil: got %v, want [1]", s)
	}
}

func TestApplyTolerance_NoFailures(t *testing.T) {
	code := applyTolerance(nil, 1, 2)
	if code != 0 {
		t.Errorf("no failures: code=%d, want 0", code)
	}
}

func TestApplyTolerance_WithinTolerance(t *testing.T) {
	code := applyTolerance([]int{0}, 1, 2)
	if code != 1 {
		t.Errorf("within tolerance: code=%d, want 1", code)
	}
}

func TestApplyTolerance_ExceedsTolerance(t *testing.T) {
	code := applyTolerance([]int{0, 1}, 1, 2)
	if code != 2 {
		t.Errorf("exceeds tolerance: code=%d, want 2", code)
	}
}

func TestApplyTolerance_ZeroTolerance(t *testing.T) {
	code := applyTolerance([]int{0}, 0, 2)
	if code != 2 {
		t.Errorf("zero tolerance: code=%d, want 2", code)
	}
}
