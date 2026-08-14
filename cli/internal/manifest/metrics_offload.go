// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest

import (
	"fmt"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/metricsoffload"
	"github.com/Azure/taugrid/cli/internal/storage"
)

const (
	defaultMetricsOffloadProject             = "tau-finetune"
	defaultMetricsOffloadSource              = metricsoffload.DefaultSource
	defaultMetricsOffloadInterval            = metricsoffload.DefaultInterval
	defaultMetricsOffloadRemoteWriteEndpoint = metricsoffload.DefaultRemoteWriteEndpoint
)

// MetricsOffloadOptions configures the sidecar-only RayJob metrics offload
// path. Image is the enabling field and must be pinned.
type MetricsOffloadOptions = metricsoffload.Options

type metricsOffloadRuntime struct {
	Enabled             bool
	Image               string
	Project             string
	Experiment          string
	Group               string
	Tags                map[string]string
	Source              string
	Store               string
	Out                 string
	History             string
	CompletionFile      string
	DoneFile            string
	DoneTimeout         time.Duration
	RemoteWriteEndpoint string
	Interval            time.Duration
}

type metricsOffloadTemplateData struct {
	Enabled                 bool
	ImageYAML               string
	ProjectYAML             string
	ExperimentYAML          string
	GroupYAML               string
	TagsYAML                string
	SourceYAML              string
	StoreYAML               string
	OutYAML                 string
	HistoryYAML             string
	CompletionFileShell     string
	CompletionFileYAML      string
	DoneFileShell           string
	DoneFileYAML            string
	DoneTimeoutSeconds      int64
	RemoteWriteEndpointYAML string
	IntervalYAML            string
}

func (opts RenderOptions) metricsOffloadRuntime(kind string) (metricsOffloadRuntime, error) {
	mo := opts.MetricsOffload
	mo.Image = strings.TrimSpace(mo.Image)
	if mo.Image == "" {
		return metricsOffloadRuntime{}, nil
	}
	if kind != WorkloadKindRayJob && kind != WorkloadKindRayJobEval {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload-image requires --workload-kind=%s or --workload-kind=%s (got %q)", WorkloadKindRayJob, WorkloadKindRayJobEval, kind)
	}
	if opts.Manifest.IsCPUOnly() {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload-image is only supported on GPU RayJob/RayJob-eval workloads today")
	}
	if err := ValidatePinnedMetricsOffloadImage(mo.Image); err != nil {
		return metricsOffloadRuntime{}, err
	}
	runDir := storage.DurableFinetuneDir(opts.Manifest.Name)
	project := firstNonEmpty(mo.Project, defaultMetricsOffloadProject)
	group := firstNonEmpty(mo.Group, opts.Manifest.ResearchExperiment(), "default")
	experimentID := firstNonEmpty(mo.Experiment, opts.Manifest.ResearchExperiment(), group)
	store := firstNonEmpty(mo.Store, runDir+"/metrics-expstore")
	out := firstNonEmpty(mo.Out, runDir+"/metrics-offload")
	source := firstNonEmpty(mo.Source, defaultMetricsOffloadSource)
	endpoint := firstNonEmpty(mo.RemoteWriteEndpoint, defaultMetricsOffloadRemoteWriteEndpoint)
	interval := mo.Interval
	if interval == 0 {
		interval = defaultMetricsOffloadInterval
	}
	if interval < 0 {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload-interval must be positive")
	}
	return metricsOffloadRuntime{
		Enabled:             true,
		Image:               mo.Image,
		Project:             project,
		Experiment:          experimentID,
		Group:               group,
		Tags:                compactMetricsOffloadTags(mo.Tags),
		Source:              source,
		Store:               store,
		Out:                 out,
		History:             runDir + "/metrics-history.jsonl",
		CompletionFile:      runDir + "/metrics-completion.json",
		DoneFile:            runDir + "/metrics-done.json",
		DoneTimeout:         metricsoffload.DefaultDoneTimeout,
		RemoteWriteEndpoint: endpoint,
		Interval:            interval,
	}, nil
}

// ValidatePinnedMetricsOffloadImage enforces the sidecar image supply-chain
// contract: callers must provide an explicit immutable digest or non-latest tag.
func ValidatePinnedMetricsOffloadImage(image string) error {
	if err := metricsoffload.ValidatePinnedImage(image); err != nil {
		return fmt.Errorf("--metrics-offload-image: %w", err)
	}
	return nil
}

func (m metricsOffloadRuntime) templateData() metricsOffloadTemplateData {
	if !m.Enabled {
		return metricsOffloadTemplateData{}
	}
	return metricsOffloadTemplateData{
		Enabled:                 true,
		ImageYAML:               quoteYAMLString(m.Image),
		ProjectYAML:             quoteYAMLString(m.Project),
		ExperimentYAML:          quoteYAMLString(m.Experiment),
		GroupYAML:               quoteYAMLString(m.Group),
		TagsYAML:                quoteYAMLString(formatMetricsOffloadTags(m.Tags)),
		SourceYAML:              quoteYAMLString(m.Source),
		StoreYAML:               quoteYAMLString(m.Store),
		OutYAML:                 quoteYAMLString(m.Out),
		HistoryYAML:             quoteYAMLString(m.History),
		CompletionFileShell:     shellQuote(m.CompletionFile),
		CompletionFileYAML:      quoteYAMLString(m.CompletionFile),
		DoneFileShell:           shellQuote(m.DoneFile),
		DoneFileYAML:            quoteYAMLString(m.DoneFile),
		DoneTimeoutSeconds:      int64(m.DoneTimeout / time.Second),
		RemoteWriteEndpointYAML: quoteYAMLString(m.RemoteWriteEndpoint),
		IntervalYAML:            quoteYAMLString(m.Interval.String()),
	}
}

func compactMetricsOffloadTags(tags map[string]string) map[string]string {
	return metricsoffload.CompactTags(tags)
}

func formatMetricsOffloadTags(tags map[string]string) string {
	return metricsoffload.FormatTags(tags)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
