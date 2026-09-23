// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metricsoffload owns the reusable metrics producer configuration
// shared by managed RayJobs and direct Jobs.
package metricsoffload

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/runconfig"
)

const (
	DefaultSource     = "stellar-online"
	DefaultInterval   = 10 * time.Second
	DefaultADXTable   = "TauExpMetricEventsV1"
	DefaultADXMapping = "TauExpMetricEventsV1Json"

	RuntimeCollectorV1 = runconfig.MetricsOffloadRuntimeCollectorV1

	DeliveryADXRequired = runconfig.MetricsOffloadDeliveryADXRequired
	MaxADXAttempts      = runconfig.MetricsOffloadMaxADXAttempts
)

// Options contains platform-owned offload settings plus experiment scope.
type Options struct {
	Runtime               string
	Image                 string
	Project               string
	Experiment            string
	Group                 string
	Tags                  map[string]string
	Source                string
	Store                 string
	Out                   string
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

// Runtime is a fully resolved, credential-free metrics producer contract.
type Runtime struct {
	Runtime                 string
	Image                   string
	RunID                   string
	Project                 string
	Experiment              string
	Group                   string
	Tags                    map[string]string
	Source                  string
	Store                   string
	Out                     string
	History                 []string
	CompletionFile          string
	Interval                time.Duration
	ArtifactURI             string
	CheckpointURI           string
	BaselineExistingHistory bool
	ReadyFile               string
	ReadyTimeout            time.Duration
	DoneFile                string
	DoneTimeout             time.Duration // Zero derives the ADX delivery budget.
	DeliveryMode            string
	ADXClusterURI           string
	ADXDatabase             string
	ADXTable                string
	ADXMapping              string
	ADXClientID             string
	ADXMaxAttempts          int
	ADXRetryBackoff         time.Duration
	ADXFinalStatusTimeout   time.Duration
}

func (r Runtime) Enabled() bool {
	return strings.TrimSpace(r.Image) != ""
}

func (r Runtime) Validate() error {
	runtime, err := ResolveRuntime(r.Runtime)
	if err != nil {
		return err
	}
	if !r.Enabled() {
		return nil
	}
	if err := ValidateRuntimeImage(runtime, r.Image); err != nil {
		return err
	}
	for field, value := range map[string]string{
		"run":             r.RunID,
		"project":         r.Project,
		"experiment":      r.Experiment,
		"group":           r.Group,
		"store":           r.Store,
		"out":             r.Out,
		"completion file": r.CompletionFile,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("metrics offload %s is required", field)
		}
	}
	if len(r.History) == 0 {
		return fmt.Errorf("metrics offload history requires at least one path")
	}
	for i, history := range r.History {
		if strings.TrimSpace(history) == "" {
			return fmt.Errorf("metrics offload history[%d] must not be empty", i)
		}
	}
	if r.Interval <= 0 {
		return fmt.Errorf("metrics offload interval must be positive")
	}
	deliveryMode, err := ResolveDeliveryMode(r.DeliveryMode)
	if err != nil {
		return err
	}
	if runtime != RuntimeCollectorV1 {
		return fmt.Errorf("metrics offload runtime must be %q", RuntimeCollectorV1)
	}
	for field, value := range map[string]string{
		"ADX cluster URI": r.ADXClusterURI,
		"ADX database":    r.ADXDatabase,
		"ADX client ID":   r.ADXClientID,
	} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("metrics offload %s is required for delivery mode %q", field, deliveryMode)
		}
	}
	if r.ADXMaxAttempts < 0 || r.ADXMaxAttempts > MaxADXAttempts {
		return fmt.Errorf("metrics offload ADX max attempts must be between 0 and %d", MaxADXAttempts)
	}
	if r.ADXRetryBackoff < 0 {
		return fmt.Errorf("metrics offload ADX retry backoff must not be negative")
	}
	if r.ADXFinalStatusTimeout < 0 {
		return fmt.Errorf("metrics offload ADX final status timeout must not be negative")
	}
	if _, err := TerminalDrainTimeout(
		r.ADXMaxAttempts,
		r.ADXRetryBackoff,
		r.ADXFinalStatusTimeout,
	); err != nil {
		return fmt.Errorf("metrics offload terminal drain: %w", err)
	}
	if r.BaselineExistingHistory && strings.TrimSpace(r.ReadyFile) == "" {
		return fmt.Errorf("metrics offload ready file is required when existing history is baselined")
	}
	if r.DoneTimeout < 0 {
		return fmt.Errorf("metrics offload done timeout must not be negative")
	}
	return nil
}

func TerminalDrainTimeout(maxAttempts int, retryBackoff, finalStatusTimeout time.Duration) (time.Duration, error) {
	return runconfig.MetricsOffloadTerminalDrainTimeout(maxAttempts, retryBackoff, finalStatusTimeout)
}

// ShutdownGracePeriodSeconds keeps Kubernetes from sending SIGKILL before the
// collector's bounded final drain can finish and the pod can exit cleanly.
func ShutdownGracePeriodSeconds(doneTimeout time.Duration, minimum int64) int64 {
	if doneTimeout <= 0 {
		return minimum
	}
	timeout := doneTimeout + runconfig.MetricsOffloadTerminalDrainGrace
	seconds := int64((timeout + time.Second - 1) / time.Second)
	if seconds > minimum {
		return seconds
	}
	return minimum
}

func ResolveRuntime(value string) (string, error) {
	return runconfig.ResolveMetricsOffloadRuntime(value)
}

func ResolveDeliveryMode(value string) (string, error) {
	return runconfig.ResolveMetricsOffloadDeliveryMode(value)
}

// ValidateRuntimeImage validates the explicit executable contract and pinned
// image reference without guessing compatibility from the repository name.
func ValidateRuntimeImage(runtime, image string) error {
	return runconfig.ValidateMetricsOffloadRuntimeImage(runtime, image)
}

// ValidatePinnedImage rejects mutable or implicit sidecar image references.
func ValidatePinnedImage(image string) error {
	return ValidateRuntimeImage(RuntimeCollectorV1, image)
}

// MergeTags applies experiment overrides and then protected platform scope.
func MergeTags(base, overrides, protected map[string]string) map[string]string {
	out := map[string]string{}
	for _, tags := range []map[string]string{base, overrides, protected} {
		for key, value := range tags {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			out[key] = strings.TrimSpace(value)
		}
	}
	return out
}

func CompactTags(tags map[string]string) map[string]string {
	return MergeTags(tags, nil, nil)
}

func TagArgs(tags map[string]string) []string {
	tags = CompactTags(tags)
	keys := make([]string, 0, len(tags))
	for key := range tags {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+tags[key])
	}
	return out
}

func FormatTags(tags map[string]string) string {
	return strings.Join(TagArgs(tags), ",")
}
