// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/resume"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/status"
)

type retryLoopOptions struct {
	name           string
	namespace      string
	kubeContext    string
	configPath     string
	maxRetries     int
	retryOn        []string
	checkpointPath string
	backoffInitial time.Duration
	backoffMax     time.Duration
	cleanup        managerCleanupOptions
	dispatch       unresolvedRunOptions
}

type retryHooks struct {
	waitForTerminal func() (status.Snapshot, terminalState, error)
	deleteWorkload  func() error
	prepareResubmit func(attempt int, reason string) error
	awaitCapacity   func() error
	resubmit        func(attempt int, reason string) error
	emitEvent       func(attempt int, reason string, message string) error
	sleep           func(d time.Duration) error
}

const retryPollInterval = 5 * time.Second

func retryLoop(cmd *cobra.Command, opts retryLoopOptions) error {
	r := kube.New(opts.kubeContext)
	ns, err := requireWorkloadNamespace(opts.namespace)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	var preparedTarget resolvedRunTarget
	var preparedExperiment runExperimentMetadata
	var preparedInput unresolvedRunOptions
	preparedDeferred := false
	resolveProfile := func(ctx context.Context, dispatch unresolvedRunOptions) (unresolvedRunOptions, error) {
		if strings.TrimSpace(dispatch.workloadProfileSnapshot) != "" {
			return resolveSnapshotRunWorkloadProfile(ctx, dispatch)
		}
		return resolveClusterRunWorkloadProfile(ctx, dispatch)
	}

	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return pollForTerminal(cmd.Context(), r, ns, opts.name)
		},
		deleteWorkload: func() error {
			return deleteWorkloadAndWaitForManagerCleanup(cmd.Context(), r, opts.name, ns, w, opts.cleanup, newManagerCleanupHooks(r, ns, opts.name))
		},
		prepareResubmit: func(attempt int, reason string) error {
			checkpointDir := opts.checkpointPath
			if checkpointDir == "" {
				checkpointDir = storage.DurableFinetuneDir(opts.name)
			}
			retryDispatch := opts.dispatch
			retryDispatch.env = appendRetryEnv(retryDispatch.env, checkpointDir, attempt, opts.maxRetries, reason)
			retryDispatch.artifactPublicationID = ""
			retryDispatch.submissionID = ""
			retryDispatch.nameFromConfig = true
			retryDispatch.deferHotVolumeCapacity = true
			preparedInput = retryDispatch
			var err error
			retryDispatch, err = resolveProfile(cmd.Context(), retryDispatch)
			if err != nil {
				return fmt.Errorf("resolving current workload profile for retry: %w", err)
			}
			if err := validateRunDispatchOptions(retryDispatch); err != nil {
				return fmt.Errorf("validating retry dispatch: %w", err)
			}
			preparedTarget, err = resolveRunTarget(retryDispatch, opts.name)
			if err != nil {
				return err
			}
			preparedExperiment = retryDispatch.experiment
			preparedDeferred = hotVolumeCapacityDeferred(retryDispatch)
			return nil
		},
		awaitCapacity: func() error {
			if !preparedDeferred {
				return nil
			}
			retryDispatch, err := awaitHotVolumeCapacity(cmd.Context(), w, resolveProfile, preparedInput,
				hotVolumeCapacityWaitTimeout, hotVolumeCapacityPollInterval)
			if err != nil {
				return err
			}
			if err := validateRunDispatchOptions(retryDispatch); err != nil {
				return fmt.Errorf("validating retry dispatch: %w", err)
			}
			preparedTarget, err = resolveRunTarget(retryDispatch, opts.name)
			if err != nil {
				return err
			}
			preparedExperiment = retryDispatch.experiment
			return nil
		},
		resubmit: func(attempt int, _ string) error {
			if preparedTarget == nil {
				return fmt.Errorf("retry replacement workload was not prepared")
			}
			captureCommand := fmt.Sprintf("tau run --config %s (retry %d/%d)", opts.configPath, attempt, opts.maxRetries)
			err := executeRunTarget(cmd, preparedTarget, captureCommand, preparedExperiment)
			preparedTarget = nil
			return err
		},
		emitEvent: func(attempt int, reason string, message string) error {
			return emitRetryEvent(cmd.Context(), r, ns, opts.name, attempt, reason, message)
		},
		sleep: func(d time.Duration) error {
			timer := time.NewTimer(d)
			select {
			case <-cmd.Context().Done():
				timer.Stop()
				return cmd.Context().Err()
			case <-timer.C:
				return nil
			}
		},
	}

	return retryLoopWithHooks(w, opts, hooks)
}

func retryLoopWithHooks(w io.Writer, opts retryLoopOptions, hooks retryHooks) error {
	ns, err := requireWorkloadNamespace(opts.namespace)
	if err != nil {
		return err
	}
	var lastFailureReason string
	var lastFailureSignature string

	fmt.Fprintf(w, "resilience: watching %s/%s (max %d retries, retry on: %s)\n",
		ns, opts.name, opts.maxRetries, strings.Join(opts.retryOn, ", "))

	for attempt := 0; ; {
		snap, terminal, err := hooks.waitForTerminal()
		if err != nil {
			return err
		}
		if terminal == terminalSuccess {
			if attempt > 0 && hooks.emitEvent != nil {
				if err := hooks.emitEvent(attempt, retryEventReasonSucceeded, retrySuccessEventMessage(opts.name, attempt, lastFailureReason, lastFailureSignature)); err != nil {
					fmt.Fprintf(w, "warning: failed to emit retry event: %v\n", err)
				}
			}
			fmt.Fprintf(w, "workload %s/%s completed successfully\n", ns, opts.name)
			return nil
		}

		reason := resume.ClassifyFailure(snap)
		signature := retryFailureSignature(snap, reason)
		lastFailureReason = reason.String()
		lastFailureSignature = signature
		if !retryOnMatch(opts.retryOn, reason) {
			return fmt.Errorf("workload %s/%s failed (%s) — not in retry_on list %v", ns, opts.name, reason, opts.retryOn)
		}

		attempt++
		if attempt > opts.maxRetries {
			if hooks.emitEvent != nil {
				if err := hooks.emitEvent(opts.maxRetries, retryEventReasonExhausted, retryExhaustedEventMessage(opts.name, opts.maxRetries, reason.String(), signature)); err != nil {
					fmt.Fprintf(w, "warning: failed to emit retry event: %v\n", err)
				}
			}
			return fmt.Errorf("workload %s/%s exhausted all %d retries", ns, opts.name, opts.maxRetries)
		}

		backoff := retryBackoff(opts.backoffInitial, opts.backoffMax, attempt)
		fmt.Fprintf(w, "attempt %d/%d failed (%s) — retrying in %s\n", attempt, opts.maxRetries, reason, backoff)

		if err := hooks.sleep(backoff); err != nil {
			return err
		}

		if hooks.emitEvent != nil {
			if err := hooks.emitEvent(attempt, retryEventReasonAttempt, retryAttemptEventMessage(opts.name, attempt, opts.maxRetries, reason.String(), signature)); err != nil {
				fmt.Fprintf(w, "warning: failed to emit retry event: %v\n", err)
			}
		}

		if hooks.prepareResubmit != nil {
			if err := hooks.prepareResubmit(attempt, reason.String()); err != nil {
				return fmt.Errorf("preparing retry %d/%d before deleting failed workload: %w", attempt, opts.maxRetries, err)
			}
		}
		fmt.Fprintf(w, "deleting failed workload %s/%s...\n", ns, opts.name)
		if err := hooks.deleteWorkload(); err != nil {
			return fmt.Errorf("deleting workload for retry: %w", err)
		}
		if hooks.awaitCapacity != nil {
			if err := hooks.awaitCapacity(); err != nil {
				return fmt.Errorf("waiting for hot-volume capacity for retry %d/%d: %w", attempt, opts.maxRetries, err)
			}
		}

		if err := hooks.resubmit(attempt, reason.String()); err != nil {
			return fmt.Errorf("submitting retry %d/%d: %w", attempt, opts.maxRetries, err)
		}
		fmt.Fprintf(w, "retry %d/%d submitted, watching...\n", attempt, opts.maxRetries)
	}
}

type terminalState int

const (
	terminalSuccess terminalState = iota
	terminalFailed
)

func pollForTerminal(ctx context.Context, r *kube.Runner, ns, name string) (status.Snapshot, terminalState, error) {
	return pollForTerminalWithFetch(ctx, func() (status.Snapshot, error) {
		return status.Fetch(ctx, r, ns, name)
	}, retryPollInterval)
}

func pollForTerminalWithFetch(
	ctx context.Context,
	fetch func() (status.Snapshot, error),
	interval time.Duration,
) (status.Snapshot, terminalState, error) {
	var observedPods []status.Pod
	var observedEvents []status.Event
	for {
		snap, err := fetch()
		if err != nil {
			return status.Snapshot{}, 0, fmt.Errorf("fetching status for retry watch: %w", err)
		}
		observedPods = mergeRetryPods(observedPods, snap.Pods)
		observedEvents = mergeRetryEvents(observedEvents, snap.Events)
		if status.WorkloadSucceeded(snap) {
			return snap, terminalSuccess, nil
		}
		if status.WorkloadFailed(snap) {
			snap.Pods = observedPods
			snap.Events = observedEvents
			return snap, terminalFailed, nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status.Snapshot{}, 0, ctx.Err()
		case <-timer.C:
		}
	}
}

func mergeRetryPods(existing, current []status.Pod) []status.Pod {
	byKey := make(map[string]status.Pod, len(existing)+len(current))
	order := make([]string, 0, len(existing)+len(current))
	for _, pod := range append(existing, current...) {
		key := pod.UID
		if key == "" {
			key = pod.Name
		}
		if key == "" {
			continue
		}
		if _, ok := byKey[key]; !ok {
			order = append(order, key)
		}
		byKey[key] = pod
	}
	out := make([]status.Pod, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

func mergeRetryEvents(existing, current []status.Event) []status.Event {
	seen := make(map[string]bool, len(existing)+len(current))
	out := make([]status.Event, 0, len(existing)+len(current))
	for _, event := range append(existing, current...) {
		key := strings.Join([]string{
			event.InvolvedKind,
			event.InvolvedName,
			event.Reason,
			event.Message,
			event.FirstSeen.String(),
		}, "\x00")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, event)
	}
	return out
}

func retryOnMatch(retryOn []string, reason resume.FailureReason) bool {
	reasonStr := reason.String()
	for _, allowed := range retryOn {
		if strings.EqualFold(allowed, reasonStr) {
			return true
		}
	}
	return false
}

func retryBackoff(initial, max time.Duration, attempt int) time.Duration {
	d := initial
	for i := 1; i < attempt; i++ {
		d *= 2
		if d > max {
			return max
		}
	}
	if d > max {
		return max
	}
	return d
}

func appendRetryEnv(env []string, checkpointDir string, attempt, maxRetries int, reason string) []string {
	out := make([]string, 0, len(env)+4)
	for _, e := range env {
		if !strings.HasPrefix(e, "TAU_RESUME_FROM=") &&
			!strings.HasPrefix(e, "TAU_RETRY_ATTEMPT=") &&
			!strings.HasPrefix(e, "TAU_RETRY_MAX=") &&
			!strings.HasPrefix(e, "TAU_RETRY_REASON=") {
			out = append(out, e)
		}
	}
	out = append(out,
		"TAU_RESUME_FROM="+checkpointDir,
		fmt.Sprintf("TAU_RETRY_ATTEMPT=%d", attempt),
		fmt.Sprintf("TAU_RETRY_MAX=%d", maxRetries),
		"TAU_RETRY_REASON="+reason,
	)
	return out
}
