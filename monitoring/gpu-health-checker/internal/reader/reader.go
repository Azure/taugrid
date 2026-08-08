// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package reader implements the one-shot health check readers invoked by NPD.
// Each reader queries the SQLite database and returns an exit code indicating
// GPU health status.
package reader

import (
	"context"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

// Result holds the outcome of a reader health check.
type Result struct {
	ExitCode   int    // 0=OK, 1=warning (temporary), 2=critical (permanent)
	Message    string // Single-line stdout for NPD
	FailedGPUs []int  // GPU indices that failed this check
}

// Reader is implemented by each health check.
type Reader interface {
	Name() string
	Read(ctx context.Context, db *store.DB, cfg *config.Config) (Result, error)
}
