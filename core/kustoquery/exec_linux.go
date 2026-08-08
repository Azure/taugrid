// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//go:build linux

package kustoquery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	supervisorArg       = "--tau-kusto-command-supervisor"
	supervisorReadyFD   = 3
	supervisorExitError = 125
)

func init() {
	if len(os.Args) < 3 || os.Args[1] != supervisorArg {
		return
	}
	os.Exit(runCommandSupervisor(os.Args[2], os.Args[3:]))
}

func runCommand(ctx context.Context, name string, args []string, stdin io.Reader) ([]byte, string, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, "", fmt.Errorf("locate kusto command supervisor: %w", err)
	}
	readyReader, readyWriter, err := os.Pipe()
	if err != nil {
		return nil, "", fmt.Errorf("create kusto command supervisor ready pipe: %w", err)
	}
	defer readyReader.Close()

	supervisorArgs := append([]string{supervisorArg, name}, args...)
	cmd := exec.CommandContext(ctx, executable, supervisorArgs...)
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.ExtraFiles = []*os.File{readyWriter}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = commandWaitDelay + processTreeReapDelay
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := cmd.Process.Signal(syscall.SIGTERM)
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	if err := cmd.Start(); err != nil {
		readyWriter.Close()
		return nil, "", fmt.Errorf("start kusto command supervisor: %w", err)
	}
	readyWriter.Close()

	readyErr := waitForSupervisorReady(ctx, readyReader)
	var stopForcedTermination func() bool
	if readyErr != nil {
		// The supervisor writes readiness before starting the command. SIGTERM
		// therefore either stops it before any descendants exist or reaches its
		// installed handler and lets it perform normal descendant cleanup.
		_ = cmd.Process.Signal(syscall.SIGTERM)
		stopForcedTermination = time.AfterFunc(commandWaitDelay, func() {
			_ = cmd.Process.Kill()
		}).Stop
	}
	waitErr := cmd.Wait()
	if stopForcedTermination != nil {
		stopForcedTermination()
	}
	if readyErr != nil {
		return stdout.Bytes(), strings.TrimSpace(stderr.String()), errors.Join(readyErr, waitErr)
	}
	return stdout.Bytes(), strings.TrimSpace(stderr.String()), waitErr
}

func waitForSupervisorReady(ctx context.Context, ready *os.File) error {
	result := make(chan error, 1)
	go func() {
		var token [1]byte
		_, err := io.ReadFull(ready, token[:])
		if err == nil && token[0] != 1 {
			err = fmt.Errorf("unexpected readiness token %d", token[0])
		}
		result <- err
	}()

	timer := time.NewTimer(commandWaitDelay)
	defer timer.Stop()
	select {
	case err := <-result:
		if err != nil {
			return fmt.Errorf("wait for kusto command supervisor readiness: %w", err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("wait for kusto command supervisor readiness: timed out")
	}
}

func runCommandSupervisor(name string, args []string) int {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		fmt.Fprintf(os.Stderr, "configure kusto command supervisor: %v\n", err)
		return supervisorExitError
	}

	ctx, stop := signalContext()
	defer stop()
	ready := os.NewFile(supervisorReadyFD, "kusto-command-supervisor-ready")
	if ready == nil {
		fmt.Fprintln(os.Stderr, "configure kusto command supervisor: ready pipe is unavailable")
		return supervisorExitError
	}
	if _, err := ready.Write([]byte{1}); err != nil {
		ready.Close()
		fmt.Fprintf(os.Stderr, "signal kusto command supervisor readiness: %v\n", err)
		return supervisorExitError
	}
	ready.Close()

	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.WaitDelay = commandWaitDelay
	cmd.Cancel = func() error {
		return terminateRunningCommand(cmd)
	}
	commandErr := cmd.Run()
	cleanupErr := cleanupSupervisorChildren()
	if cleanupErr != nil {
		fmt.Fprintf(os.Stderr, "cleanup kusto command descendants: %v\n", cleanupErr)
		return supervisorExitError
	}
	return commandExitCode(commandErr)
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), syscall.SIGTERM)
}

func terminateRunningCommand(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	// Kill through os.Process while cmd.Wait still owns the child. On Linux Go
	// uses a pidfd when available, avoiding a reusable numeric PID/PGID handle.
	// Descendants are adopted and terminated by this dedicated supervisor after
	// the direct child exits.
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return os.ErrProcessDone
	}
	return err
}

func cleanupSupervisorChildren() error {
	deadline := time.Now().Add(processTreeReapDelay)
	for {
		reaped, err := reapExitedChildren()
		if err != nil {
			return err
		}
		children, err := supervisorChildren()
		if err != nil {
			return err
		}
		if len(children) == 0 {
			if !reaped {
				return nil
			}
			continue
		}
		for _, pid := range children {
			if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return fmt.Errorf("kill adopted child %d: %w", pid, err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out with adopted children %v", children)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func reapExitedChildren() (bool, error) {
	reaped := false
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		switch {
		case pid > 0:
			reaped = true
		case pid == 0:
			return reaped, nil
		case errors.Is(err, syscall.ECHILD):
			return reaped, nil
		case errors.Is(err, syscall.EINTR):
			continue
		default:
			return reaped, fmt.Errorf("reap adopted child: %w", err)
		}
	}
}

func supervisorChildren() ([]int, error) {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return nil, fmt.Errorf("list supervisor tasks: %w", err)
	}
	seen := make(map[int]struct{})
	for _, task := range tasks {
		raw, err := os.ReadFile("/proc/self/task/" + task.Name() + "/children")
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read supervisor task %s children: %w", task.Name(), err)
		}
		for _, field := range strings.Fields(string(raw)) {
			pid, err := strconv.Atoi(field)
			if err != nil {
				return nil, fmt.Errorf("parse adopted child pid %q: %w", field, err)
			}
			seen[pid] = struct{}{}
		}
	}
	children := make([]int, 0, len(seen))
	for pid := range seen {
		children = append(children, pid)
	}
	return children, nil
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if code := exitErr.ExitCode(); code >= 0 {
			return code
		}
		if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal())
		}
	}
	fmt.Fprintf(os.Stderr, "execute kusto command: %v\n", err)
	return supervisorExitError
}
