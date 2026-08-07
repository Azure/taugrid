package kustoquery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

const commandWaitDelay = 2 * time.Second

// processTreeReapDelay bounds how long cleanupProcessTree spends actively
// reaping a process group after it has already been SIGKILLed. It is shorter
// than commandWaitDelay so cleanup cannot double the total worst-case delay.
const processTreeReapDelay = 1 * time.Second

// RunCommand executes name/args like exec.CommandContext followed by Output,
// but owns the command's process group. Unix children run in an isolated
// process group; Linux additionally adopts and reaps orphaned descendants that
// remain in that group.
func RunCommand(ctx context.Context, name string, args []string, stdin io.Reader) ([]byte, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := configureProcessTree(cmd); err != nil {
		return nil, "", fmt.Errorf("configure kusto query process tree: %w", err)
	}
	cmd.WaitDelay = commandWaitDelay
	cmd.Cancel = func() error {
		return cancelProcessTree(cmd)
	}
	out, err := cmd.Output()
	if cleanupErr := cleanupProcessTree(cmd); cleanupErr != nil {
		err = errors.Join(err, cleanupErr)
	}
	return out, strings.TrimSpace(stderr.String()), err
}
