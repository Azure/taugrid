//go:build !unix && !linux

package kustoquery

import (
	"errors"
	"os"
	"os/exec"
)

func configureProcessTree(*exec.Cmd) error { return nil }

func cancelProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return os.ErrProcessDone
	}
	return err
}

func cleanupProcessTree(*exec.Cmd) error { return nil }
