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
	defaultMetricsOffloadProject  = "tau-finetune"
	defaultMetricsOffloadSource   = metricsoffload.DefaultSource
	defaultMetricsOffloadInterval = metricsoffload.DefaultInterval
)

// MetricsOffloadOptions configures the sidecar-only RayJob metrics offload
// path. Image is the enabling field and must be pinned.
type MetricsOffloadOptions = metricsoffload.Options

type metricsOffloadRuntime struct {
	Enabled               bool
	Runtime               string
	Image                 string
	Project               string
	Experiment            string
	Group                 string
	Tags                  map[string]string
	Source                string
	Store                 string
	Out                   string
	History               string
	CompletionFile        string
	DoneFile              string
	DoneTimeout           time.Duration
	Interval              time.Duration
	DeliveryMode          string
	ADXClusterURI         string
	ADXDatabase           string
	ADXTable              string
	ADXMapping            string
	ADXClientID           string
	ADXMaxAttempts        int
	ADXRetryBackoff       time.Duration
	ADXFinalStatusTimeout time.Duration
}

type metricsOffloadTemplateData struct {
	Enabled                   bool
	CollectorV1               bool
	CommandYAML               string
	ArgsPrefixYAML            string
	ImageYAML                 string
	ProjectYAML               string
	ExperimentYAML            string
	GroupYAML                 string
	TagsYAML                  string
	SourceYAML                string
	StoreYAML                 string
	OutYAML                   string
	HistoryYAML               string
	CompletionFileShell       string
	CompletionFileYAML        string
	DoneFileShell             string
	DoneFileYAML              string
	DoneTimeoutSeconds        int64
	ShutdownGraceSeconds      int64
	IntervalYAML              string
	DeliveryModeYAML          string
	ADXClusterURIYAML         string
	ADXDatabaseYAML           string
	ADXTableYAML              string
	ADXMappingYAML            string
	ADXClientIDYAML           string
	ADXMaxAttemptsYAML        string
	ADXRetryBackoffYAML       string
	ADXFinalStatusTimeoutYAML string
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
	runtime, err := metricsoffload.ResolveRuntime(strings.TrimSpace(mo.Runtime))
	if err != nil {
		return metricsOffloadRuntime{}, err
	}
	if err := metricsoffload.ValidateRuntimeImage(runtime, mo.Image); err != nil {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload-image: %w", err)
	}
	runDir := storage.DurableFinetuneDir(opts.Manifest.Name)
	project := firstNonEmpty(mo.Project, defaultMetricsOffloadProject)
	group := firstNonEmpty(mo.Group, opts.Manifest.ResearchExperiment(), "default")
	experimentID := firstNonEmpty(mo.Experiment, opts.Manifest.ResearchExperiment(), group)
	store := firstNonEmpty(mo.Store, runDir+"/metrics-expstore")
	out := firstNonEmpty(mo.Out, runDir+"/metrics-offload")
	source := firstNonEmpty(mo.Source, defaultMetricsOffloadSource)
	deliveryMode, err := metricsoffload.ResolveDeliveryMode(mo.DeliveryMode)
	if err != nil {
		return metricsOffloadRuntime{}, err
	}
	if runtime != metricsoffload.RuntimeCollectorV1 {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload runtime must be %q", metricsoffload.RuntimeCollectorV1)
	}
	for field, value := range map[string]string{
		"ADX cluster URI": mo.ADXClusterURI,
		"ADX database":    mo.ADXDatabase,
		"ADX client ID":   mo.ADXClientID,
	} {
		if strings.TrimSpace(value) == "" {
			return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload %s is required for delivery mode %q", field, deliveryMode)
		}
	}
	if mo.ADXMaxAttempts < 0 || mo.ADXMaxAttempts > metricsoffload.MaxADXAttempts {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload ADX max attempts must be between 0 and %d", metricsoffload.MaxADXAttempts)
	}
	if mo.ADXRetryBackoff < 0 {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload ADX retry backoff must not be negative")
	}
	if mo.ADXFinalStatusTimeout < 0 {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload ADX final status timeout must not be negative")
	}
	doneTimeout, err := metricsoffload.TerminalDrainTimeout(
		mo.ADXMaxAttempts,
		mo.ADXRetryBackoff,
		mo.ADXFinalStatusTimeout,
	)
	if err != nil {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload terminal drain: %w", err)
	}
	interval := mo.Interval
	if interval == 0 {
		interval = defaultMetricsOffloadInterval
	}
	if interval < 0 {
		return metricsOffloadRuntime{}, fmt.Errorf("--metrics-offload-interval must be positive")
	}
	return metricsOffloadRuntime{
		Enabled:               true,
		Runtime:               runtime,
		Image:                 mo.Image,
		Project:               project,
		Experiment:            experimentID,
		Group:                 group,
		Tags:                  compactMetricsOffloadTags(mo.Tags),
		Source:                source,
		Store:                 store,
		Out:                   out,
		History:               runDir + "/metrics-history.jsonl",
		CompletionFile:        runDir + "/metrics-completion.json",
		DoneFile:              runDir + "/metrics-done.json",
		DoneTimeout:           doneTimeout,
		Interval:              interval,
		DeliveryMode:          deliveryMode,
		ADXClusterURI:         strings.TrimSpace(mo.ADXClusterURI),
		ADXDatabase:           strings.TrimSpace(mo.ADXDatabase),
		ADXTable:              firstNonEmpty(mo.ADXTable, metricsoffload.DefaultADXTable),
		ADXMapping:            firstNonEmpty(mo.ADXMapping, metricsoffload.DefaultADXMapping),
		ADXClientID:           strings.TrimSpace(mo.ADXClientID),
		ADXMaxAttempts:        mo.ADXMaxAttempts,
		ADXRetryBackoff:       mo.ADXRetryBackoff,
		ADXFinalStatusTimeout: mo.ADXFinalStatusTimeout,
	}, nil
}

func (m metricsOffloadRuntime) templateData() metricsOffloadTemplateData {
	if !m.Enabled {
		return metricsOffloadTemplateData{}
	}
	command, prefixArgs, err := metricsoffload.RuntimeCommand(m.Runtime)
	if err != nil {
		panic(err)
	}
	quotedArgs := make([]string, 0, len(prefixArgs))
	for _, arg := range prefixArgs {
		quotedArgs = append(quotedArgs, quoteYAMLString(arg))
	}
	return metricsOffloadTemplateData{
		Enabled:                   true,
		CollectorV1:               true,
		CommandYAML:               quoteYAMLString(command),
		ArgsPrefixYAML:            strings.Join(quotedArgs, ", "),
		ImageYAML:                 quoteYAMLString(m.Image),
		ProjectYAML:               quoteYAMLString(m.Project),
		ExperimentYAML:            quoteYAMLString(m.Experiment),
		GroupYAML:                 quoteYAMLString(m.Group),
		TagsYAML:                  quoteYAMLString(formatMetricsOffloadTags(m.Tags)),
		SourceYAML:                quoteYAMLString(m.Source),
		StoreYAML:                 quoteYAMLString(m.Store),
		OutYAML:                   quoteYAMLString(m.Out),
		HistoryYAML:               quoteYAMLString(m.History),
		CompletionFileShell:       shellQuote(m.CompletionFile),
		CompletionFileYAML:        quoteYAMLString(m.CompletionFile),
		DoneFileShell:             shellQuote(m.DoneFile),
		DoneFileYAML:              quoteYAMLString(m.DoneFile),
		DoneTimeoutSeconds:        int64(m.DoneTimeout / time.Second),
		ShutdownGraceSeconds:      metricsoffload.ShutdownGracePeriodSeconds(m.DoneTimeout, 30),
		IntervalYAML:              quoteYAMLString(m.Interval.String()),
		DeliveryModeYAML:          quoteYAMLString(m.DeliveryMode),
		ADXClusterURIYAML:         quoteYAMLString(m.ADXClusterURI),
		ADXDatabaseYAML:           quoteYAMLString(m.ADXDatabase),
		ADXTableYAML:              quoteYAMLString(m.ADXTable),
		ADXMappingYAML:            quoteYAMLString(m.ADXMapping),
		ADXClientIDYAML:           quoteYAMLString(m.ADXClientID),
		ADXMaxAttemptsYAML:        quoteYAMLString(formatPositiveInt(m.ADXMaxAttempts)),
		ADXRetryBackoffYAML:       quoteYAMLString(formatPositiveDuration(m.ADXRetryBackoff)),
		ADXFinalStatusTimeoutYAML: quoteYAMLString(formatPositiveDuration(m.ADXFinalStatusTimeout)),
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

func formatPositiveInt(value int) string {
	if value <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", value)
}

func formatPositiveDuration(value time.Duration) string {
	if value <= 0 {
		return ""
	}
	return value.String()
}
