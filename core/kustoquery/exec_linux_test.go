//go:build linux

package kustoquery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const helperArgPrefix = "--kustoquery-helper="

func TestMain(m *testing.M) {
	for _, arg := range os.Args {
		if mode, ok := strings.CutPrefix(arg, helperArgPrefix); ok {
			os.Exit(runProcessHelper(mode))
		}
	}
	os.Exit(m.Run())
}

func runProcessHelper(mode string) int {
	switch mode {
	case "success", "error", "cancel", "pipe":
		child := exec.Command(os.Args[0], helperArgPrefix+"descendant")
		if mode == "pipe" {
			child.Stdout = os.Stdout
			child.Stderr = os.Stderr
		}
		if err := child.Start(); err != nil {
			fmt.Fprintf(os.Stderr, "start descendant: %v\n", err)
			return 2
		}
		if mode == "success" {
			fmt.Printf(`[{"pid":%d,"pgid":%d}]`, child.Process.Pid, syscall.Getpgrp())
			return 0
		}
		fmt.Fprintf(os.Stderr, "descendant-pid=%d helper-pgid=%d\n", child.Process.Pid, syscall.Getpgrp())
		if mode == "error" {
			return 7
		}
		if mode == "pipe" {
			fmt.Print(`[]`)
			return 0
		}
		time.Sleep(30 * time.Second)
		return 0
	case "descendant":
		time.Sleep(30 * time.Second)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown helper mode %q\n", mode)
		return 2
	}
}

func TestClientQueryReapsAdoptedDescendantOnSuccess(t *testing.T) {
	requireChildSubreaper(t)
	rows, err := (Client{
		Command: os.Args[0],
		Args:    []string{helperArgPrefix + "success"},
	}).Query(context.Background(), "GpuHealth()")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	pid := rowInt(t, rows[0], "pid")
	pgid := rowInt(t, rows[0], "pgid")
	t.Cleanup(func() { terminateAndReap(pid) })

	if processExists(pid) {
		t.Errorf("descendant pid %d still exists after successful query", pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("query process group = portal process group %d, want isolation", pgid)
	}
	assertNoChildInGroup(t, pgid)
}

func TestClientQueryReapsAdoptedDescendantOnCommandError(t *testing.T) {
	requireChildSubreaper(t)
	_, err := (Client{
		Command: os.Args[0],
		Args:    []string{helperArgPrefix + "error"},
	}).Query(context.Background(), "GpuHealth()")
	if err == nil {
		t.Fatal("Query err = nil, want command error")
	}

	match := regexp.MustCompile(`descendant-pid=(\d+) helper-pgid=(\d+)`).FindStringSubmatch(err.Error())
	if len(match) != 3 {
		t.Fatalf("Query error %q does not include helper process metadata", err)
	}
	pid, _ := strconv.Atoi(match[1])
	pgid, _ := strconv.Atoi(match[2])
	t.Cleanup(func() { terminateAndReap(pid) })

	if processExists(pid) {
		t.Errorf("descendant pid %d still exists after command error", pid)
	}
	assertNoChildInGroup(t, pgid)
}

func TestClientQueryReapsAdoptedDescendantOnCancellation(t *testing.T) {
	requireChildSubreaper(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := (Client{
		Command: os.Args[0],
		Args:    []string{helperArgPrefix + "cancel"},
	}).Query(ctx, "GpuHealth()")
	if err == nil {
		t.Fatal("Query err = nil, want context cancellation")
	}

	match := regexp.MustCompile(`descendant-pid=(\d+) helper-pgid=(\d+)`).FindStringSubmatch(err.Error())
	if len(match) != 3 {
		t.Fatalf("Query error %q does not include helper process metadata", err)
	}
	pid, _ := strconv.Atoi(match[1])
	pgid, _ := strconv.Atoi(match[2])
	t.Cleanup(func() { terminateAndReap(pid) })

	if processExists(pid) {
		t.Errorf("descendant pid %d still exists after canceled query", pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("query process group = portal process group %d, want isolation", pgid)
	}
	assertNoChildInGroup(t, pgid)
}

func TestClientQueryBoundsWaitForDescendantHoldingPipes(t *testing.T) {
	requireChildSubreaper(t)
	started := time.Now()
	_, err := (Client{
		Command: os.Args[0],
		Args:    []string{helperArgPrefix + "pipe"},
	}).Query(context.Background(), "GpuHealth()")
	elapsed := time.Since(started)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Query err = %v, want exec.ErrWaitDelay", err)
	}
	// The bound only needs to rule out the descendant's unbounded 30s sleep;
	// the race detector slows signal delivery around the WaitDelay path.
	const tolerance = commandWaitDelay + 3*time.Second
	if elapsed > tolerance {
		t.Fatalf("Query took %s with inherited pipes, want at most %s", elapsed, tolerance)
	}

	match := regexp.MustCompile(`descendant-pid=(\d+) helper-pgid=(\d+)`).FindStringSubmatch(err.Error())
	if len(match) != 3 {
		t.Fatalf("Query error %q does not include helper process metadata", err)
	}
	pid, _ := strconv.Atoi(match[1])
	pgid, _ := strconv.Atoi(match[2])
	t.Cleanup(func() { terminateAndReap(pid) })
	if processExists(pid) {
		t.Errorf("pipe-holding descendant pid %d still exists after query", pid)
	}
	assertNoChildInGroup(t, pgid)
}

func requireChildSubreaper(t *testing.T) {
	t.Helper()
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Fatalf("set child subreaper: %v", err)
	}
}

func rowInt(t *testing.T, row Row, column string) int {
	t.Helper()
	value, ok := row.Num(column)
	if !ok {
		t.Fatalf("row column %q = %#v, want number", column, row[column])
	}
	return int(value)
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func assertNoChildInGroup(t *testing.T, pgid int) {
	t.Helper()
	var status syscall.WaitStatus
	pid, err := syscall.Wait4(-pgid, &status, syscall.WNOHANG, nil)
	if pid > 0 {
		t.Fatalf("reaped leaked child pid %d from process group %d", pid, pgid)
	}
	if !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("wait4 process group %d = (%d, %v), want ECHILD", pgid, pid, err)
	}
}

func terminateAndReap(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var status syscall.WaitStatus
		waited, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil)
		if waited == pid || errors.Is(err, syscall.ECHILD) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}
