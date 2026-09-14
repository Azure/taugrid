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
	DefaultSource              = "stellar-online"
	DefaultRemoteWriteEndpoint = "http://${NODE_IP}:3100/receive"
	DefaultInterval            = 10 * time.Second
	DefaultDoneTimeout         = 2 * time.Minute
)

// Options contains platform-owned offload settings plus experiment scope.
type Options struct {
	Image               string
	Project             string
	Experiment          string
	Group               string
	Tags                map[string]string
	Source              string
	Store               string
	Out                 string
	RemoteWriteEndpoint string
	Interval            time.Duration
}

// Runtime is a fully resolved, credential-free metrics producer contract.
type Runtime struct {
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
	RemoteWriteEndpoint     string
	Interval                time.Duration
	ArtifactURI             string
	CheckpointURI           string
	BaselineExistingHistory bool
	ReadyFile               string
	ReadyTimeout            time.Duration
	DoneFile                string
	DoneTimeout             time.Duration // Zero uses DefaultDoneTimeout.
}

func (r Runtime) Enabled() bool {
	return strings.TrimSpace(r.Image) != ""
}

func (r Runtime) Validate() error {
	if !r.Enabled() {
		return nil
	}
	if err := ValidatePinnedImage(r.Image); err != nil {
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
	if strings.TrimSpace(r.RemoteWriteEndpoint) == "" {
		return fmt.Errorf("metrics offload remote-write endpoint is required")
	}
	if r.BaselineExistingHistory && strings.TrimSpace(r.ReadyFile) == "" {
		return fmt.Errorf("metrics offload ready file is required when existing history is baselined")
	}
	if r.DoneTimeout < 0 {
		return fmt.Errorf("metrics offload done timeout must not be negative")
	}
	return nil
}

// ValidatePinnedImage rejects mutable or implicit sidecar image references.
func ValidatePinnedImage(image string) error {
	return runconfig.ValidateMetricsOffloadImage(image)
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
