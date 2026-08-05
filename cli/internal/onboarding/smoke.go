// Package onboarding implements Tau's bounded zero-to-first-job smoke.
package onboarding

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/jobrender"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/kube"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	SmokeImage     = "mcr.microsoft.com/azurelinux/base/core@sha256:0cdd0c6a200fc2b5d6da711c34228126034bd428650b43dfb7e378214e6f2d32"
	smokeLogMarker = "tau-onboarding-smoke-ok"
)

type rawRunner interface {
	Raw(context.Context, []string, []byte) (string, error)
}

type SmokeOptions struct {
	Name           string
	Namespace      string
	Queue          string
	ServiceAccount string
	Workspace      string
	ResultScope    string
	Image          string
	DryRun         string
	Timeout        time.Duration
}

type SmokeResult struct {
	RunID    string
	Phase    string
	Logs     string
	Manifest []byte
}

type SmokeRunner struct {
	Runner rawRunner
}

func NewSmokeRunner(contextName string) SmokeRunner {
	return SmokeRunner{Runner: kube.New(contextName)}
}

func RenderSmoke(options SmokeOptions) ([]byte, error) {
	if strings.TrimSpace(options.Name) == "" {
		return nil, fmt.Errorf("smoke run name is required")
	}
	if strings.TrimSpace(options.Namespace) == "" {
		return nil, fmt.Errorf("smoke namespace is required")
	}
	if strings.TrimSpace(options.Queue) == "" {
		return nil, fmt.Errorf("smoke LocalQueue is required")
	}
	if strings.TrimSpace(options.Workspace) == "" {
		return nil, fmt.Errorf("smoke workspace is required")
	}
	image := firstNonEmpty(options.Image, SmokeImage)
	labels, annotations := experiment.MergeMetadata(
		workloadmeta.StampWorkspace(map[string]string{
			workloadmeta.LabelJob: options.Name,
			"run_id":              options.Name,
		}, options.Workspace),
		map[string]string{
			workloadmeta.LabelOnboardingSmoke: "true",
		},
		experiment.Metadata{
			RunID:        options.Name,
			Namespace:    options.Namespace,
			WorkspaceID:  options.Workspace,
			ResultScope:  options.ResultScope,
			WorkloadKind: experiment.WorkloadKindJob,
			Image:        image,
		},
	)
	smokeProfile := profile.Profile{
		Name: "tau-onboarding-smoke",
		Spec: map[string]any{
			"queue": map[string]any{"localQueue": options.Queue},
			"resources": map[string]any{
				"requests": map[string]any{"cpu": "50m", "memory": "64Mi"},
				"limits":   map[string]any{"cpu": "250m", "memory": "128Mi"},
			},
			"runtime": map[string]any{
				"image":           image,
				"imagePullPolicy": "IfNotPresent",
				"securityContext": map[string]any{
					"allowPrivilegeEscalation": false,
					"runAsNonRoot":             true,
					"runAsUser":                int64(65532),
					"runAsGroup":               int64(65532),
					"capabilities": map[string]any{
						"drop": []any{"ALL"},
					},
					"seccompProfile": map[string]any{"type": "RuntimeDefault"},
				},
			},
			"policy": map[string]any{"activeDeadlineSeconds": int64(300)},
		},
	}
	return jobrender.Render(smokeProfile, jobrender.Options{
		Name:                          options.Name,
		Namespace:                     options.Namespace,
		Image:                         image,
		Command:                       []string{"/bin/sh", "-lc", "printf '" + smokeLogMarker + "\\n'"},
		ServiceAccountName:            options.ServiceAccount,
		TerminationGracePeriodSeconds: 10,
		ActiveDeadlineSeconds:         300,
		TTLSecondsAfterFinished:       600,
		Labels:                        labels,
		Annotations:                   annotations,
	})
}

func (r SmokeRunner) Run(ctx context.Context, options SmokeOptions) (SmokeResult, error) {
	if r.Runner == nil {
		return SmokeResult{}, fmt.Errorf("smoke Kubernetes runner is not configured")
	}
	if options.Name == "" {
		name, err := newSmokeRunID()
		if err != nil {
			return SmokeResult{}, err
		}
		options.Name = name
	}
	if options.Timeout <= 0 {
		options.Timeout = 10 * time.Minute
	}
	if options.DryRun != "" && options.DryRun != "client" && options.DryRun != "server" {
		return SmokeResult{}, fmt.Errorf("smoke dry-run must be client or server")
	}
	manifest, err := RenderSmoke(options)
	if err != nil {
		return SmokeResult{}, err
	}
	if options.DryRun == "client" {
		return SmokeResult{RunID: options.Name, Phase: "DryRun", Manifest: manifest}, nil
	}
	applyArgs := []string{"apply", "-f", "-"}
	if options.DryRun == "server" {
		applyArgs = append(applyArgs, "--dry-run=server", "-o", "yaml")
		output, err := r.Runner.Raw(ctx, applyArgs, manifest)
		if err != nil {
			return SmokeResult{}, fmt.Errorf("server-validate onboarding smoke: %w", err)
		}
		return SmokeResult{RunID: options.Name, Phase: "DryRun", Manifest: []byte(output)}, nil
	}
	if _, err := r.Runner.Raw(ctx, applyArgs, manifest); err != nil {
		return SmokeResult{}, fmt.Errorf("submit onboarding smoke %s: %w", options.Name, err)
	}
	if _, err := r.Runner.Raw(ctx, []string{
		"-n", options.Namespace,
		"wait", "--for=condition=complete",
		"job/" + options.Name,
		"--timeout=" + options.Timeout.String(),
	}, nil); err != nil {
		logs, _ := r.Runner.Raw(ctx, []string{
			"-n", options.Namespace,
			"logs", "job/" + options.Name,
			"--all-containers=true", "--tail=200",
		}, nil)
		return SmokeResult{}, fmt.Errorf("wait for onboarding smoke %s: %w\n%s", options.Name, err, strings.TrimSpace(logs))
	}
	logs, err := r.Runner.Raw(ctx, []string{
		"-n", options.Namespace,
		"logs", "job/" + options.Name,
		"--all-containers=true", "--tail=200",
	}, nil)
	if err != nil {
		return SmokeResult{}, fmt.Errorf("read onboarding smoke logs %s: %w", options.Name, err)
	}
	if !strings.Contains(logs, smokeLogMarker) {
		return SmokeResult{}, fmt.Errorf("onboarding smoke %s completed without the expected output marker", options.Name)
	}
	return SmokeResult{RunID: options.Name, Phase: "Succeeded", Logs: logs}, nil
}

func newSmokeRunID() (string, error) {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", fmt.Errorf("generate smoke run ID: %w", err)
	}
	return "smoke-" + time.Now().UTC().Format("20060102-150405") + "-" + hex.EncodeToString(suffix[:]), nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
