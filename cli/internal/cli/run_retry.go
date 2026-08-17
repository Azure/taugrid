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

	hooks := retryHooks{
		waitForTerminal: func() (status.Snapshot, terminalState, error) {
			return pollForTerminal(cmd.Context(), r, ns, opts.name)
		},
		deleteWorkload: func() error {
			return deleteWorkloadAndWaitForManagerCleanup(cmd.Context(), r, opts.name, ns, w, opts.cleanup, newManagerCleanupHooks(r, ns, opts.name))
		},
		resubmit: func(attempt int, reason string) error {
			checkpointDir := opts.checkpointPath
			if checkpointDir == "" {
				checkpointDir = storage.DurableFinetuneDir(opts.name)
			}
			retryDispatch := opts.dispatch
			retryDispatch.env = appendRetryEnv(retryDispatch.env, checkpointDir, attempt, opts.maxRetries, reason)
			retryDispatch.artifactPublicationID = ""
			retryDispatch.submissionID = ""
			retryDispatch.nameFromConfig = true
			if err := validateRunDispatchOptions(retryDispatch); err != nil {
				return fmt.Errorf("validating retry dispatch: %w", err)
			}
			target, err := resolveRunTarget(retryDispatch, opts.name)
			if err != nil {
				return err
			}
			captureCommand := fmt.Sprintf("tau run --config %s (retry %d/%d)", opts.configPath, attempt, opts.maxRetries)
			return executeRunTarget(cmd, target, captureCommand, retryDispatch.experiment)
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

		fmt.Fprintf(w, "deleting failed workload %s/%s...\n", ns, opts.name)
		if err := hooks.deleteWorkload(); err != nil {
			return fmt.Errorf("deleting workload for retry: %w", err)
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
	for {
		snap, err := status.Fetch(ctx, r, ns, name)
		if err != nil {
			return status.Snapshot{}, 0, fmt.Errorf("fetching status for retry watch: %w", err)
		}
		if status.WorkloadSucceeded(snap) {
			return snap, terminalSuccess, nil
		}
		if status.WorkloadFailed(snap) {
			return snap, terminalFailed, nil
		}
		timer := time.NewTimer(retryPollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return status.Snapshot{}, 0, ctx.Err()
		case <-timer.C:
		}
	}
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
