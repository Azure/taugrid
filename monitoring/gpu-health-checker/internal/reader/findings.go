// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reader

import (
	"fmt"
	"strings"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
)

// CheckFindings accumulates partial results from sub-checks within a reader.
type CheckFindings struct {
	Critical []int
	Warning  []int
	Parts    []string
}

func (f *CheckFindings) AddCritical(gpu int, msg string) {
	f.Critical = AppendUnique(f.Critical, gpu)
	f.Parts = append(f.Parts, msg)
}

func (f *CheckFindings) AddWarning(gpu int, msg string) {
	f.Warning = AppendUnique(f.Warning, gpu)
	f.Parts = append(f.Parts, msg)
}

func (f *CheckFindings) AddNote(msg string) {
	f.Parts = append(f.Parts, msg)
}

func (f *CheckFindings) buildResult(gpus []int, cfg *config.Config, fr FindingResult) Result {
	if fr.Label == "" {
		return Result{
			ExitCode:   fr.Base,
			Message:    truncateMsg(strings.Join(f.Parts, "; ")),
			FailedGPUs: gpus,
		}
	}
	code := applyTolerance(gpus, cfg.MaxFailedGPUs, fr.Base)
	msg := fmt.Sprintf("%s — %d/%d %s, tolerance %d",
		strings.Join(f.Parts, "; "), len(gpus), cfg.ExpectedGPUs, fr.Label, cfg.MaxFailedGPUs)
	return Result{ExitCode: code, Message: truncateMsg(msg), FailedGPUs: gpus}
}

func truncateMsg(msg string) string {
	const maxLen = 4096
	if len(msg) > maxLen {
		return msg[:maxLen] + " [TRUNCATED]"
	}
	return msg
}
