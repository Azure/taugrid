//go:build linux

package kustoquery

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	subreaperOnce sync.Once
	subreaperErr  error
)

func configureProcessTree(cmd *exec.Cmd) error {
	// PID 1 is already the container's adopter. PR_SET_CHILD_SUBREAPER gives
	// the same ownership when the portal runs under an init or in tests.
	subreaperOnce.Do(func() {
		subreaperErr = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
	})
	if subreaperErr != nil {
		return subreaperErr
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	return nil
}

func cancelProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func cleanupProcessTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid
	if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("kill kusto query process group %d: %w", pgid, err)
	}

	// A negative wait target owns only adopted descendants from this query;
	// it cannot reap direct children that another os/exec.Cmd is waiting for.
	deadline := time.Now().Add(processTreeReapDelay)
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-pgid, &status, syscall.WNOHANG, nil)
		switch {
		case pid > 0:
			continue
		case errors.Is(err, syscall.ECHILD):
			return nil
		case errors.Is(err, syscall.EINTR):
			continue
		case err != nil:
			return fmt.Errorf("reap kusto query process group %d: %w", pgid, err)
		case time.Now().After(deadline):
			return fmt.Errorf("reap kusto query process group %d: timed out", pgid)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}
