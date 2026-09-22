// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package metricsoffload owns shared metrics delivery defaults and budgets.
package metricsoffload

import (
	"fmt"
	"time"
)

const (
	MaxADXAttempts     = 10
	DefaultADXAttempts = 3

	DefaultADXRetryBackoff       = time.Second
	DefaultADXFinalStatusTimeout = 10 * time.Minute
	TerminalDrainGrace           = 30 * time.Second
	MaxTerminalDrainTimeout      = 2 * time.Hour
)

// TerminalDrainTimeout returns the finite workload-side deadline that covers
// every configured ADX attempt, exponential retry backoff, and a small
// coordination grace period. Each attempt's timeout covers both queued
// submission and final-status confirmation.
func TerminalDrainTimeout(maxAttempts int, retryBackoff, finalStatusTimeout time.Duration) (time.Duration, error) {
	if maxAttempts < 0 || maxAttempts > MaxADXAttempts {
		return 0, fmt.Errorf("adx_max_attempts must be between 0 and %d", MaxADXAttempts)
	}
	attempts := maxAttempts
	if attempts == 0 {
		attempts = DefaultADXAttempts
	}
	if retryBackoff < 0 {
		return 0, fmt.Errorf("adx_retry_backoff must not be negative")
	}
	if retryBackoff == 0 {
		retryBackoff = DefaultADXRetryBackoff
	}
	if retryBackoff > MaxTerminalDrainTimeout {
		return 0, fmt.Errorf(
			"ADX delivery budget exceeds maximum terminal drain timeout %s",
			MaxTerminalDrainTimeout,
		)
	}
	if finalStatusTimeout < 0 {
		return 0, fmt.Errorf("adx_final_status_timeout must not be negative")
	}
	if finalStatusTimeout == 0 {
		finalStatusTimeout = DefaultADXFinalStatusTimeout
	}
	if finalStatusTimeout > MaxTerminalDrainTimeout {
		return 0, fmt.Errorf(
			"ADX delivery budget exceeds maximum terminal drain timeout %s",
			MaxTerminalDrainTimeout,
		)
	}

	backoffWindows := time.Duration((1 << (attempts - 1)) - 1)
	timeout := time.Duration(attempts)*finalStatusTimeout +
		backoffWindows*retryBackoff +
		TerminalDrainGrace
	if timeout > MaxTerminalDrainTimeout {
		return 0, fmt.Errorf(
			"ADX delivery budget %s exceeds maximum terminal drain timeout %s",
			timeout,
			MaxTerminalDrainTimeout,
		)
	}
	return timeout, nil
}
