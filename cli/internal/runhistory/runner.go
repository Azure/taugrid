// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runhistory

import (
	"context"
	"fmt"
	"time"
)

func Run(ctx context.Context, reconciler *Reconciler, namespace string, interval time.Duration, once bool, health *Health) error {
	if interval <= 0 {
		return fmt.Errorf("run history interval must be > 0")
	}
	for {
		result, err := reconciler.Reconcile(ctx, namespace)
		if err != nil {
			// A continuously running recorder must tolerate transient ADX failures.
			// In particular, schema management is asynchronous: the recorder can
			// start before adx-mon has created its table and mapping. Keep readiness
			// false until a later reconciliation succeeds instead of terminating the
			// Deployment and relying on a Kubernetes restart.
			if once {
				return err
			}
		} else if health != nil {
			health.MarkSuccess(result, time.Now().UTC().Format(time.RFC3339Nano))
		}
		if once {
			return nil
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
