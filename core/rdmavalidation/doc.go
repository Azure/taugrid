// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package rdmavalidation defines the stable, dependency-neutral result contract
// for TauGrid's point-in-time inter-node RDMA validation.
//
// A validation result is historical evidence, finalized only after the cleanup
// attempt completes. Freshness is derived separately using the fixed 24-hour
// observed_at, stale_after_seconds, and valid_until relationship; an expired
// result does not mutate its recorded pass, fail, or unknown status. This
// contract is not a continuous GPU-health signal.
package rdmavalidation
