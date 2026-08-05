package cli

import (
	"context"
	"time"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/status"
)

func waitStatusInterval(ctx context.Context, interval time.Duration) error {
	timer := time.NewTimer(interval)
	defer func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func newManagerCleanupHooks(r *kube.Runner, namespace, name string) managerCleanupHooks {
	return managerCleanupHooks{
		fetchSnapshot: func(ctx context.Context) (status.Snapshot, error) {
			return fetchManagerCleanupStatusSnapshot(ctx, r, namespace, name)
		},
		wait: waitStatusInterval,
		now:  time.Now,
	}
}

func fetchManagerCleanupStatusSnapshot(ctx context.Context, r kubeRawRunner, namespace, name string) (status.Snapshot, error) {
	snap, err := status.FetchManagerCleanup(ctx, r, namespace, name)
	if err != nil {
		return snap, err
	}
	if err := ctx.Err(); err != nil {
		return snap, err
	}
	return snap, nil
}
