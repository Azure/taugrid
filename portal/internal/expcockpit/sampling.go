package expcockpit

import (
	"math"
	"sort"
	"strings"
)

type SamplingMetadata struct {
	Algorithm               string `json:"algorithm"`
	SourcePoints            int    `json:"source_points"`
	ServerPreselectedPoints int    `json:"server_preselected_points"`
	PreselectedPoints       int    `json:"preselected_points"`
	RenderedPoints          int    `json:"rendered_points"`
	RequestedBudget         int    `json:"requested_budget"`
	EffectiveBudget         int    `json:"effective_budget"`
	StartStep               int64  `json:"start_step,omitempty"`
	EndStep                 int64  `json:"end_step,omitempty"`
	FirstRetained           bool   `json:"first_retained"`
	LastRetained            bool   `json:"last_retained"`
	MinRetained             bool   `json:"min_retained"`
	MaxRetained             bool   `json:"max_retained"`
	MilestonePoints         int    `json:"milestone_points,omitempty"`
	MilestonesRetained      int    `json:"milestones_retained,omitempty"`
	Truncated               bool   `json:"truncated,omitempty"`
}

func sampleMetricPointsByDensity(points []metricPoint, target int) []metricPoint {
	sampled, _ := sampleMetricPoints(points, target, nil)
	return sampled
}

func sampleMetricPoints(points []metricPoint, target int, milestoneSteps map[int64]bool) ([]metricPoint, SamplingMetadata) {
	normalized := normalizeMetricPoints(points)
	metadata := samplingMetadata(normalized, len(points), target, milestoneSteps)
	if target <= 0 || len(normalized) == 0 {
		return nil, metadata
	}
	if len(normalized) <= target {
		metadata.Algorithm = "none"
		metadata.PreselectedPoints = len(normalized)
		metadata.RenderedPoints = len(normalized)
		metadata.EffectiveBudget = len(normalized)
		setSamplingRetention(&metadata, normalized, normalized, milestoneSteps)
		return normalized, metadata
	}
	if target == 1 {
		sampled := []metricPoint{normalized[len(normalized)-1]}
		metadata.PreselectedPoints = len(normalized)
		metadata.RenderedPoints = 1
		metadata.EffectiveBudget = 1
		setSamplingRetention(&metadata, normalized, sampled, milestoneSteps)
		return sampled, metadata
	}

	mandatory := mandatoryPointIndices(normalized, milestoneSteps)
	if len(mandatory) > target {
		mandatory = trimMandatoryIndices(normalized, mandatory, target, milestoneSteps)
	}
	preselected := minMaxPreselect(normalized, target, mandatory)
	metadata.PreselectedPoints = len(preselected)

	selected := lttbIndices(normalized, preselected, target)
	selectedSet := make(map[int]bool, len(selected)+len(mandatory))
	for _, idx := range selected {
		selectedSet[idx] = true
	}
	mandatorySet := make(map[int]bool, len(mandatory))
	for _, idx := range mandatory {
		selectedSet[idx] = true
		mandatorySet[idx] = true
	}
	if len(selectedSet) > target {
		trimNonMandatoryIndices(normalized, selectedSet, mandatorySet, len(selectedSet)-target)
	}
	selected = selected[:0]
	for idx := range selectedSet {
		selected = append(selected, idx)
	}
	sort.Ints(selected)

	sampled := make([]metricPoint, 0, len(selected))
	for _, idx := range selected {
		sampled = append(sampled, normalized[idx])
	}
	metadata.RenderedPoints = len(sampled)
	metadata.EffectiveBudget = len(sampled)
	setSamplingRetention(&metadata, normalized, sampled, milestoneSteps)
	return sampled, metadata
}

func normalizeMetricPoints(points []metricPoint) []metricPoint {
	if len(points) < 2 {
		return points
	}
	normalized := append([]metricPoint(nil), points...)
	sort.SliceStable(normalized, func(i, j int) bool {
		return normalized[i].Step < normalized[j].Step
	})
	out := normalized[:0]
	for _, point := range normalized {
		if len(out) > 0 && out[len(out)-1].Step == point.Step {
			out[len(out)-1] = point
			continue
		}
		out = append(out, point)
	}
	return out
}

func samplingMetadata(points []metricPoint, sourcePoints, target int, milestoneSteps map[int64]bool) SamplingMetadata {
	metadata := SamplingMetadata{
		Algorithm:       "minmax_lttb",
		SourcePoints:    sourcePoints,
		RequestedBudget: target,
		Truncated:       len(points) > target && target > 0,
	}
	if len(points) > 0 {
		metadata.StartStep = points[0].Step
		metadata.EndStep = points[len(points)-1].Step
		for _, point := range points {
			if milestoneSteps[point.Step] {
				metadata.MilestonePoints++
			}
		}
	}
	return metadata
}

func mandatoryPointIndices(points []metricPoint, milestoneSteps map[int64]bool) []int {
	if len(points) == 0 {
		return nil
	}
	minIdx, maxIdx := 0, 0
	for idx := 1; idx < len(points); idx++ {
		if points[idx].Value < points[minIdx].Value {
			minIdx = idx
		}
		if points[idx].Value > points[maxIdx].Value {
			maxIdx = idx
		}
	}
	set := map[int]bool{0: true, len(points) - 1: true, minIdx: true, maxIdx: true}
	for idx, point := range points {
		if milestoneSteps[point.Step] {
			set[idx] = true
		}
	}
	out := make([]int, 0, len(set))
	for idx := range set {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

func trimMandatoryIndices(points []metricPoint, mandatory []int, target int, milestoneSteps map[int64]bool) []int {
	priority := []int{0, len(points) - 1}
	minIdx, maxIdx := 0, 0
	for idx := 1; idx < len(points); idx++ {
		if points[idx].Value < points[minIdx].Value {
			minIdx = idx
		}
		if points[idx].Value > points[maxIdx].Value {
			maxIdx = idx
		}
	}
	priority = append(priority, minIdx, maxIdx)
	for _, idx := range mandatory {
		if milestoneSteps[points[idx].Step] {
			priority = append(priority, idx)
		}
	}
	out := make([]int, 0, target)
	seen := map[int]bool{}
	for _, idx := range priority {
		if seen[idx] {
			continue
		}
		seen[idx] = true
		out = append(out, idx)
		if len(out) == target {
			break
		}
	}
	sort.Ints(out)
	return out
}

func minMaxPreselect(points []metricPoint, target int, mandatory []int) []int {
	if len(points) <= target*4 {
		out := make([]int, len(points))
		for idx := range points {
			out[idx] = idx
		}
		return out
	}
	buckets := target * 2
	bucketSize := int(math.Ceil(float64(len(points)) / float64(buckets)))
	selected := make(map[int]bool, target*4+len(mandatory))
	for start := 0; start < len(points); start += bucketSize {
		end := min(start+bucketSize, len(points))
		minIdx, maxIdx := start, start
		for idx := start + 1; idx < end; idx++ {
			if points[idx].Value < points[minIdx].Value {
				minIdx = idx
			}
			if points[idx].Value > points[maxIdx].Value {
				maxIdx = idx
			}
		}
		selected[minIdx] = true
		selected[maxIdx] = true
	}
	selected[0] = true
	selected[len(points)-1] = true
	for _, idx := range mandatory {
		selected[idx] = true
	}
	out := make([]int, 0, len(selected))
	for idx := range selected {
		out = append(out, idx)
	}
	sort.Ints(out)
	return out
}

func lttbIndices(points []metricPoint, candidates []int, target int) []int {
	if len(candidates) <= target {
		return append([]int(nil), candidates...)
	}
	if target == 1 {
		return []int{candidates[len(candidates)-1]}
	}
	if target == 2 {
		return []int{candidates[0], candidates[len(candidates)-1]}
	}

	sampled := make([]int, 0, target)
	sampled = append(sampled, candidates[0])
	bucketSize := float64(len(candidates)-2) / float64(target-2)
	a := 0
	for bucket := 0; bucket < target-2; bucket++ {
		avgStart := int(math.Floor(float64(bucket+1)*bucketSize)) + 1
		avgEnd := int(math.Floor(float64(bucket+2)*bucketSize)) + 1
		if avgEnd > len(candidates) {
			avgEnd = len(candidates)
		}
		if avgStart >= avgEnd {
			avgStart = min(avgStart, len(candidates)-1)
			avgEnd = avgStart + 1
		}
		var avgX, avgY float64
		for idx := avgStart; idx < avgEnd; idx++ {
			point := points[candidates[idx]]
			avgX += float64(point.Step)
			avgY += point.Value
		}
		averageCount := float64(avgEnd - avgStart)
		avgX /= averageCount
		avgY /= averageCount

		rangeStart := int(math.Floor(float64(bucket)*bucketSize)) + 1
		rangeEnd := int(math.Floor(float64(bucket+1)*bucketSize)) + 1
		rangeEnd = min(rangeEnd, len(candidates)-1)
		pointA := points[candidates[a]]
		maxArea := -1.0
		nextA := rangeStart
		for idx := rangeStart; idx < rangeEnd; idx++ {
			point := points[candidates[idx]]
			area := math.Abs((float64(pointA.Step)-avgX)*(point.Value-pointA.Value) -
				(float64(pointA.Step)-float64(point.Step))*(avgY-pointA.Value))
			if area > maxArea {
				maxArea = area
				nextA = idx
			}
		}
		sampled = append(sampled, candidates[nextA])
		a = nextA
	}
	sampled = append(sampled, candidates[len(candidates)-1])
	return sampled
}

func trimNonMandatoryIndices(points []metricPoint, selected, mandatory map[int]bool, remove int) {
	type candidate struct {
		index int
		area  float64
	}
	ordered := make([]int, 0, len(selected))
	for idx := range selected {
		ordered = append(ordered, idx)
	}
	sort.Ints(ordered)
	removable := make([]candidate, 0, len(ordered))
	for pos := 1; pos < len(ordered)-1; pos++ {
		idx := ordered[pos]
		if mandatory[idx] {
			continue
		}
		removable = append(removable, candidate{
			index: idx,
			area:  triangleArea(points[ordered[pos-1]], points[idx], points[ordered[pos+1]]),
		})
	}
	sort.SliceStable(removable, func(i, j int) bool {
		if removable[i].area == removable[j].area {
			return removable[i].index < removable[j].index
		}
		return removable[i].area < removable[j].area
	})
	for idx := 0; idx < remove && idx < len(removable); idx++ {
		delete(selected, removable[idx].index)
	}
}

func triangleArea(a, b, c metricPoint) float64 {
	return math.Abs((float64(a.Step)-float64(c.Step))*(b.Value-a.Value) -
		(float64(a.Step)-float64(b.Step))*(c.Value-a.Value))
}

func setSamplingRetention(metadata *SamplingMetadata, source, sampled []metricPoint, milestoneSteps map[int64]bool) {
	if len(source) == 0 {
		return
	}
	minValue, maxValue := source[0].Value, source[0].Value
	for _, point := range source[1:] {
		minValue = math.Min(minValue, point.Value)
		maxValue = math.Max(maxValue, point.Value)
	}
	for _, point := range sampled {
		if point.Step == source[0].Step {
			metadata.FirstRetained = true
		}
		if point.Step == source[len(source)-1].Step {
			metadata.LastRetained = true
		}
		if point.Value == minValue {
			metadata.MinRetained = true
		}
		if point.Value == maxValue {
			metadata.MaxRetained = true
		}
		if milestoneSteps[point.Step] {
			metadata.MilestonesRetained++
		}
	}
}

func validationMilestoneSteps(points []metricPoint, selectedMetric string) map[string]map[int64]bool {
	out := map[string]map[int64]bool{}
	for _, point := range points {
		if point.MetricName == selectedMetric && point.Milestone {
			if out[point.RunID] == nil {
				out[point.RunID] = map[int64]bool{}
			}
			out[point.RunID][point.Step] = true
			continue
		}
		if point.MetricName == selectedMetric || !isValidationMilestoneMetric(point.MetricName) {
			continue
		}
		if out[point.RunID] == nil {
			out[point.RunID] = map[int64]bool{}
		}
		out[point.RunID][point.Step] = true
	}
	return out
}

func isValidationMilestoneMetric(metric string) bool {
	metric = strings.ToLower(strings.TrimSpace(metric))
	for _, prefix := range []string{"eval/", "validation/", "val/", "test/", "final/"} {
		if strings.HasPrefix(metric, prefix) {
			return true
		}
	}
	return false
}

func includeMilestonePoints(points, source []metricPoint, milestones map[int64]bool) []metricPoint {
	if len(milestones) == 0 {
		return points
	}
	included := make(map[int64]bool, len(points))
	for _, point := range points {
		included[point.Step] = true
	}
	out := append([]metricPoint(nil), points...)
	for _, point := range source {
		if milestones[point.Step] && !included[point.Step] {
			out = append(out, point)
			included[point.Step] = true
		}
	}
	return normalizeMetricPoints(out)
}
