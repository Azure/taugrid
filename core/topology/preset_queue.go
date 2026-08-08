// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package topology

import (
	"fmt"
	"strings"

	"github.com/Azure/taugrid/core/workloadmeta"
)

// DefaultLocalQueueNamespace is where Tau expects workload LocalQueues when a
// preset or command does not name a namespace.
//
// This is the default workspace's namespace. It previously named a
// "taugrid-team" namespace that no TauGrid install ever created, so any
// preset-based lookup that reached this fallback searched somewhere empty.
const DefaultLocalQueueNamespace = workloadmeta.DefaultWorkspaceName

// QueueOverrideReconcileResult is the effective team and warning produced by a
// preset queue override.
type QueueOverrideReconcileResult struct {
	Team    string
	Warning string
}

// ReconcilePresetQueueOverride keeps an explicit queue override consistent with
// the team implied by a topology preset.
func ReconcilePresetQueueOverride(preset ResolvedPreset, queueName, team, lane string, queueChanged, teamChanged bool) (QueueOverrideReconcileResult, error) {
	result := QueueOverrideReconcileResult{Team: team}
	if !queueChanged {
		return result, nil
	}
	queue := strings.TrimSpace(queueName)
	if queue == "" || queue == preset.Options.QueueName {
		return result, nil
	}
	trimmedTeam := strings.TrimSpace(team)
	if queueMatchesTeam(queue, trimmedTeam) {
		return result, nil
	}
	if teamChanged {
		return result, fmt.Errorf("--queue=%q conflicts with --team=%q; queue overrides must keep the Kueue LocalQueue and team intent consistent", queue, trimmedTeam)
	}
	if inferred := inferTeamFromQueue(queue, lane); inferred != "" {
		result.Team = inferred
		result.Warning = fmt.Sprintf("warning: --queue=%q overrides preset %s queue %q; inferred --team=%s so the Kueue LocalQueue and team intent stay consistent", queue, preset.Preset.Name, preset.Options.QueueName, inferred)
		return result, nil
	}
	return result, fmt.Errorf("--queue=%q overrides preset %s queue %q but leaves team=%q from the preset; pass --team for the queue owner so Kueue LocalQueue and team intent stay consistent", queue, preset.Preset.Name, preset.Options.QueueName, trimmedTeam)
}

func queueMatchesTeam(queue, team string) bool {
	if queue == "" || team == "" {
		return false
	}
	return queue == team || strings.HasPrefix(queue, team+"-")
}

func inferTeamFromQueue(queue, lane string) string {
	queue = strings.TrimSpace(queue)
	lane = strings.TrimSpace(lane)
	if queue == "" {
		return ""
	}
	if lane != "" {
		suffix := "-" + lane
		if strings.HasSuffix(queue, suffix) {
			return strings.TrimSuffix(queue, suffix)
		}
	}
	if before, _, ok := strings.Cut(queue, "-"); ok {
		return before
	}
	return ""
}

// PresetLocalQueueNamespace returns the namespace used for validating a preset
// LocalQueue.
func PresetLocalQueueNamespace(namespace string, preset ResolvedPreset) string {
	if namespace != "" {
		return namespace
	}
	if preset.Preset.Namespace != "" {
		return preset.Preset.Namespace
	}
	return DefaultLocalQueueNamespace
}

// MissingPresetLocalQueueError reports a missing effective LocalQueue while
// distinguishing preset defaults from explicit queue overrides.
func MissingPresetLocalQueueError(preset ResolvedPreset, namespace, queueName, detail string) error {
	source := fmt.Sprintf("topology preset %s targets LocalQueue %q", preset.Preset.Name, queueName)
	if queueName != preset.Options.QueueName {
		source = fmt.Sprintf("--queue override selects LocalQueue %q while using topology preset %s", queueName, preset.Preset.Name)
	}
	return fmt.Errorf("%s in namespace %q, but it was not found (%s); ask the platform owner to validate preset %s, pick a usable preset, or pass a matching --queue/--team override", source, namespace, detail, preset.Preset.Name)
}
