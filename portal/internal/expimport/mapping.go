package expimport

import "strings"

// standardResearchMetrics is the canonical set of metric names Tau treats as
// first-class research signals. Membership drives the tau.metric.standard tag
// written by the JSONL importer.
var standardResearchMetrics = map[string]struct{}{
	"train/return":                {},
	"eval/score":                  {},
	"wm/loss":                     {},
	"world_model/loss":            {},
	"model/loss":                  {},
	"train/loss":                  {},
	"eval/loss":                   {},
	"train/lr":                    {},
	"train/learning_rate":         {},
	"train/grad_norm":             {},
	"train/gradient_norm":         {},
	"train/step_time_s":           {},
	"train/step_time":             {},
	"train/examples_seen":         {},
	"train/input_tokens":          {},
	"train/tokens":                {},
	"gpu/memory_allocated_gb":     {},
	"gpu/memory_reserved_gb":      {},
	"gpu/max_memory_allocated_gb": {},
	"checkpoint/file_count":       {},
	"checkpoint/bytes":            {},
	"inference/time_s":            {},
	"wm/perplexity":               {},
	"world_model/perplexity":      {},
	"model/perplexity":            {},
	"train/perplexity":            {},
	"eval/perplexity":             {},
	"wm/cross_entropy":            {},
	"wm/cross-entropy":            {},
	"world_model/cross_entropy":   {},
	"world_model/cross-entropy":   {},
	"model/cross_entropy":         {},
	"model/cross-entropy":         {},
	"model/kl":                    {},
	"policy/entropy":              {},
	"system/gpu_util":             {},
}

func knownResearchMetric(name string) bool {
	_, ok := standardResearchMetrics[strings.ToLower(strings.TrimSpace(name))]
	return ok
}

func IsStandardResearchMetric(name string) bool {
	return knownResearchMetric(name) || strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), "feature/")
}

func ResearchMetricCard(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	switch {
	case lower == "train/return",
		lower == "eval/score":
		return "Outcome"
	case lower == "train/loss",
		lower == "eval/loss",
		strings.HasPrefix(lower, "wm/"),
		strings.HasPrefix(lower, "world_model/"),
		strings.HasPrefix(lower, "model/"):
		return "World model"
	case strings.HasPrefix(lower, "train/lr"),
		strings.HasPrefix(lower, "train/learning_rate"),
		strings.HasPrefix(lower, "train/grad_norm"),
		strings.HasPrefix(lower, "train/gradient_norm"),
		strings.HasPrefix(lower, "train/step_time"):
		return "Optimization"
	case strings.HasPrefix(lower, "train/examples"),
		strings.Contains(lower, "tokens"),
		strings.Contains(lower, "throughput"):
		return "Throughput"
	case strings.HasPrefix(lower, "gpu/"),
		strings.HasPrefix(lower, "system/"):
		return "Systems"
	case strings.HasPrefix(lower, "checkpoint/"):
		return "Checkpoint"
	case strings.HasPrefix(lower, "feature/"),
		strings.HasPrefix(lower, "inference/"),
		strings.HasPrefix(lower, "eval/"):
		return "Model diagnostics"
	case strings.HasPrefix(lower, "policy/"):
		return "Behavior"
	default:
		return "Other metrics"
	}
}
