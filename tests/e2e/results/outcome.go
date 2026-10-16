// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package results

import "time"

type Status string

const (
	StatusPass Status = "pass"
	StatusFail Status = "fail"
	StatusSkip Status = "skip"
)

type Outcome struct {
	RunID       int64     `json:"run_id"`
	RunAttempt  int64     `json:"run_attempt"`
	TestName    string    `json:"test_name"`
	Suite       string    `json:"suite"`
	Status      Status    `json:"status"`
	DurationSec float64   `json:"duration_sec"`
	Timestamp   time.Time `json:"timestamp"`
	Branch      string    `json:"branch"`
}
