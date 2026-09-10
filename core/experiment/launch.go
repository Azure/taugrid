// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package experiment

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"strings"
)

const LaunchTag = "tau.launch"

var launchValue = regexp.MustCompile(`^[a-zA-Z0-9_./:@+-]*$`)
var launchCommandValue = regexp.MustCompile(`^[a-zA-Z0-9_./: -]+$`)
var launchMIGProfile = regexp.MustCompile(`^[0-9]+g\.[0-9]+gb$`)

// Launch records resolved submission intent, never an observed allocation.
// Deliberately exclude raw argv, environment, pip URLs and arbitrary config values:
// all can carry credentials. Entrypoint is a path, not a workload command.
type Launch struct {
	Version         int    `json:"version"`
	WorkloadKind    string `json:"workload_kind,omitempty"`
	GPUsPerWorker   *int   `json:"gpus_per_worker,omitempty"`
	Workers         *int   `json:"workers,omitempty"`
	CPUWorkers      *int   `json:"cpu_workers,omitempty"`
	GPUTotal        *int   `json:"gpu_total,omitempty"`
	GPUClass        string `json:"gpu_class,omitempty"`
	GPUResourceMode string `json:"gpu_resource_mode,omitempty"`
	GPUResourceName string `json:"gpu_resource_name,omitempty"`
	MIGProfile      string `json:"mig_profile,omitempty"`
	Image           string `json:"image,omitempty"`
	Entrypoint      string `json:"entrypoint,omitempty"`
	Launcher        string `json:"launcher,omitempty"`
	Profile         string `json:"profile,omitempty"`
	Workspace       string `json:"workspace,omitempty"`
	Namespace       string `json:"namespace,omitempty"`
	TauCommand      string `json:"tau_command,omitempty"`
}

// JSON validates and bounds the allowlisted metadata before persistence.
func (l Launch) JSON() string {
	if l.Version != 1 {
		return ""
	}
	for _, count := range []*int{l.GPUsPerWorker, l.Workers, l.CPUWorkers, l.GPUTotal} {
		if count != nil && *count < 0 {
			return ""
		}
	}
	for _, value := range []*string{&l.WorkloadKind, &l.GPUClass, &l.Image, &l.Entrypoint, &l.Launcher, &l.Profile, &l.Workspace, &l.Namespace} {
		// No URLs with credentials/query strings, shell fragments or multiline
		// values. Runtime arguments and env are intentionally not accepted.
		if !launchValue.MatchString(*value) || strings.Contains(*value, "://") ||
			(strings.Contains(*value, "@") && !strings.Contains(*value, "@sha256:")) {
			*value = ""
		}
	}
	if !safeLaunchCommand(l.TauCommand) {
		l.TauCommand = ""
	}
	switch l.GPUResourceMode {
	case "", "none", "device-plugin", "dra", "mig":
	default:
		l.GPUResourceMode = ""
	}
	if !launchMIGProfile.MatchString(l.MIGProfile) {
		l.MIGProfile = ""
	}
	if l.GPUResourceName != "nvidia.com/gpu" &&
		!(strings.HasPrefix(l.GPUResourceName, "nvidia.com/mig-") && launchMIGProfile.MatchString(strings.TrimPrefix(l.GPUResourceName, "nvidia.com/mig-"))) {
		l.GPUResourceName = ""
	}
	if (l.GPUResourceMode != "" && l.GPUResourceMode != "mig" && (l.MIGProfile != "" || strings.HasPrefix(l.GPUResourceName, "nvidia.com/mig-"))) ||
		(l.GPUResourceMode == "mig" && l.GPUResourceName == "nvidia.com/gpu") ||
		(l.MIGProfile != "" && l.GPUResourceName != "" && l.GPUResourceName != "nvidia.com/mig-"+l.MIGProfile) ||
		((l.GPUResourceMode == "none" || l.GPUResourceMode == "dra") && l.GPUResourceName != "") {
		l.GPUResourceMode, l.GPUResourceName, l.MIGProfile = "", "", ""
	}
	raw, err := json.Marshal(l)
	if err != nil || len(raw) > maxAnnotationValueBytes {
		return ""
	}
	return string(raw)
}

// Tag avoids commas in the legacy metrics-offload comma-separated tag carrier.
func (l Launch) Tag() string {
	if raw := l.JSON(); raw != "" {
		return "base64url:" + base64.RawURLEncoding.EncodeToString([]byte(raw))
	}
	return ""
}

func ParseLaunch(raw string) *Launch {
	if strings.HasPrefix(raw, "base64url:") {
		if len(raw) > maxAnnotationValueBytes*2 {
			return nil
		}
		decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(raw, "base64url:"))
		if err != nil {
			return nil
		}
		raw = string(decoded)
	}
	var launch Launch
	if len(raw) > maxAnnotationValueBytes || json.Unmarshal([]byte(raw), &launch) != nil {
		return nil
	}
	clean := launch.JSON()
	if clean == "" {
		return nil
	}
	var sanitized Launch
	_ = json.Unmarshal([]byte(clean), &sanitized)
	return &sanitized
}

func safeLaunchCommand(command string) bool {
	if !launchCommandValue.MatchString(command) {
		return false
	}
	if !strings.HasPrefix(command, "tau run ") && !strings.HasPrefix(command, "tau submit ") {
		return false
	}
	for _, arg := range strings.Fields(command) {
		if strings.ContainsAny(arg, "=;|&`$?\n\r") || strings.Contains(arg, "://") {
			return false
		}
		if strings.HasPrefix(arg, "-") {
			switch arg {
			case "--config", "--namespace", "--context", "--dry-run", "--force", "--from":
			default:
				return false
			}
		}
	}
	return true
}
