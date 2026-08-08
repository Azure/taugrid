// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build unix && !linux

package kustoquery

import (
	"bytes"
	"context"
	"io"
	"os/exec"
	"strings"
)

func runCommand(ctx context.Context, name string, args []string, stdin io.Reader) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.WaitDelay = commandWaitDelay
	err := cmd.Run()
	return stdout.Bytes(), strings.TrimSpace(stderr.String()), err
}
