// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package kustoquery

import (
	"context"
	"io"
	"time"
)

const commandWaitDelay = 2 * time.Second

func RunCommand(ctx context.Context, name string, args []string, stdin io.Reader) ([]byte, string, error) {
	return runCommand(ctx, name, args, stdin)
}
