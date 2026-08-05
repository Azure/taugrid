// Package metricsoffload owns the reusable metrics producer configuration
// shared by managed RayJobs and direct Jobs.
package metricsoffload

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/resourceprofile"
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

// OptionsFromProfile reads spec.metrics.offload from a resolved Profile.
func OptionsFromProfile(p profile.Profile) (Options, error) {
	rawMetrics, ok := p.Spec["metrics"]
	if !ok || rawMetrics == nil {
		return Options{}, nil
	}
	metrics, ok := rawMetrics.(map[string]any)
	if !ok {
		return Options{}, fmt.Errorf("profile %q spec.metrics must be a map", p.Name)
	}
	rawOffload, ok := metrics["offload"]
	if !ok || rawOffload == nil {
		return Options{}, nil
	}
	offload, ok := rawOffload.(map[string]any)
	if !ok {
		return Options{}, fmt.Errorf("profile %q spec.metrics.offload must be a map", p.Name)
	}
	var out Options
	for key, value := range offload {
		s, ok := value.(string)
		if !ok {
			return Options{}, fmt.Errorf("profile %q spec.metrics.offload.%s must be a string", p.Name, key)
		}
		s = strings.TrimSpace(s)
		switch normalizedKey(key) {
		case "image":
			out.Image = s
		case "project":
			out.Project = s
		case "experiment":
			out.Experiment = s
		case "group":
			out.Group = s
		case "tags":
			tags, err := ParseTags(s)
			if err != nil {
				return Options{}, fmt.Errorf("profile %q spec.metrics.offload.%s: %w", p.Name, key, err)
			}
			out.Tags = tags
		case "source":
			out.Source = s
		case "store":
			out.Store = s
		case "out", "output":
			out.Out = s
		case "remotewriteendpoint", "remotewrite", "endpoint":
			out.RemoteWriteEndpoint = s
		case "interval":
			if s == "" {
				continue
			}
			d, err := time.ParseDuration(s)
			if err != nil {
				return Options{}, fmt.Errorf("profile %q spec.metrics.offload.%s: %w", p.Name, key, err)
			}
			out.Interval = d
		default:
			return Options{}, fmt.Errorf("profile %q spec.metrics.offload.%s: unknown field", p.Name, key)
		}
	}
	return out, nil
}

// ValidatePinnedImage rejects mutable or implicit sidecar image references.
func ValidatePinnedImage(image string) error {
	image = strings.TrimSpace(image)
	if image == "" {
		return fmt.Errorf("metrics offload image is required when metrics offload is enabled")
	}
	if strings.ContainsAny(image, " \t\r\n") {
		return fmt.Errorf("metrics offload image must not contain whitespace")
	}
	if strings.Contains(image, "@sha256:") {
		return nil
	}
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon <= lastSlash || lastColon == len(image)-1 {
		return fmt.Errorf("metrics offload image %q must include an explicit non-latest tag or @sha256 digest", image)
	}
	if strings.EqualFold(image[lastColon+1:], "latest") {
		return fmt.Errorf("metrics offload image must not use the unpinned :latest tag")
	}
	return nil
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

// ParseTags parses a comma-separated profile tag string.
func ParseTags(raw string) (map[string]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	out := map[string]string{}
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return nil, fmt.Errorf("expected comma-separated key=value tags")
		}
		out[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return out, nil
}

func normalizedKey(key string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	key = strings.ReplaceAll(key, "_", "")
	return strings.ReplaceAll(key, "-", "")
}
