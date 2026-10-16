// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package reader

import (
	"context"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

type Result struct {
	ExitCode   int
	Message    string
	FailedGPUs []int
}

type Reader interface {
	Name() string
	Read(ctx context.Context, db *store.DB, cfg *config.Config) (Result, error)
}
