// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kustoquery

import (
	"context"
	"io"
	"time"
)

const commandWaitDelay = 2 * time.Second

// RunCommand executes an external Kusto command with bounded cancellation and
// pipe waits. Linux uses a dedicated child-subreaper process for each command,
// so cleanup owns every adopted descendant without making the portal itself a
// process-wide reaper.
func RunCommand(ctx context.Context, name string, args []string, stdin io.Reader) ([]byte, string, error) {
	return runCommand(ctx, name, args, stdin)
}
