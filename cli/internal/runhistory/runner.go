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
			return err
		}
		if health != nil {
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
