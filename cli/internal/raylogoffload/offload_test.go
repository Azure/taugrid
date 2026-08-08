// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package raylogoffload

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestHeadPodAnnotationsAddsContainerLogDestination(t *testing.T) {
	t.Parallel()

	base := map[string]string{"existing": "value"}
	got := HeadPodAnnotations(base)

	if got["existing"] != "value" {
		t.Fatalf("expected existing annotation to be preserved, got %#v", got)
	}
	if got[AnnotationKey] != AnnotationValue {
		t.Fatalf("expected %s=%s, got %#v", AnnotationKey, AnnotationValue, got)
	}
	if _, ok := base[AnnotationKey]; ok {
		t.Fatalf("expected base map to remain unchanged, got %#v", base)
	}
}

func TestHeadPodAnnotationsPreservesExistingLogDestination(t *testing.T) {
	t.Parallel()

	base := map[string]string{
		AnnotationKey: "Logs:CustomDestination",
		"existing":    "value",
	}
	got := HeadPodAnnotations(base)

	if got[AnnotationKey] != "Logs:CustomDestination" {
		t.Fatalf("expected existing %s to be preserved, got %#v", AnnotationKey, got)
	}
	if got["existing"] != "value" {
		t.Fatalf("expected existing annotation to be preserved, got %#v", got)
	}
	if base[AnnotationKey] != "Logs:CustomDestination" {
		t.Fatalf("expected base map to remain unchanged, got %#v", base)
	}
}

func TestPrepareInitContainerPreparesSharedRayTmpVolume(t *testing.T) {
	t.Parallel()

	got := PrepareInitContainer("example.com/ray:test")
	if got["name"] != PrepareInitName {
		t.Fatalf("name=%v want %s", got["name"], PrepareInitName)
	}
	if got["image"] != "example.com/ray:test" {
		t.Fatalf("image=%v", got["image"])
	}
	if cmd := got["command"]; !strings.Contains(strings.Join(toStrings(t, cmd), " "), "chmod 1777 /tmp/ray") {
		t.Fatalf("prepare command missing chmod contract: %v", cmd)
	}
	securityContext, ok := got["securityContext"].(map[string]any)
	if !ok {
		t.Fatalf("securityContext missing: %v", got)
	}
	if securityContext["runAsUser"] != int64(0) || securityContext["runAsGroup"] != int64(0) {
		t.Fatalf("prepare init should run as root: %v", securityContext)
	}
}

func TestScriptTailsDriverLogsWithoutDuplicatingSessionLatest(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash sidecar script test requires Unix signal semantics")
	}

	root := t.TempDir()
	scriptPath := filepath.Join(root, "offload.sh")
	if err := os.WriteFile(scriptPath, []byte(Script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	if output, err := exec.Command("bash", "-n", scriptPath).CombinedOutput(); err != nil {
		t.Fatalf("bash -n failed: %v\n%s", err, output)
	}

	sessionOneLogs := filepath.Join(root, "session_001", "logs")
	if err := os.MkdirAll(sessionOneLogs, 0o755); err != nil {
		t.Fatalf("mkdir session one: %v", err)
	}
	logOne := filepath.Join(sessionOneLogs, "job-driver-main.log")
	if err := os.WriteFile(logOne, []byte("first-line\n"), 0o644); err != nil {
		t.Fatalf("write first log: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "session_001"), filepath.Join(root, "session_latest")); err != nil {
		t.Fatalf("create session_latest symlink: %v", err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"TAU_RAY_TMP="+root,
		"TAU_RAY_LOG_POLL_SECONDS=0.1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	cmd.Stderr = os.Stderr

	var (
		mu    sync.Mutex
		lines []string
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start script: %v", err)
	}
	var waitOnce sync.Once
	stop := func() {
		if cmd.Process == nil {
			return
		}
		waitOnce.Do(func() {
			// Kubernetes signals container PID 1, not its whole process group.
			// Signal only bash so the test catches TERM traps that return to the
			// polling loop and consume the full pod termination grace period.
			_ = cmd.Process.Signal(syscall.SIGTERM)
			waitCh := make(chan struct{})
			go func() {
				_ = cmd.Wait()
				close(waitCh)
			}()
			select {
			case <-waitCh:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
				<-waitCh
			}
		})
	}
	t.Cleanup(stop)

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, readErr := stdout.Read(buf)
			if n > 0 {
				chunk := string(buf[:n])
				mu.Lock()
				for _, line := range strings.Split(chunk, "\n") {
					if line != "" {
						lines = append(lines, line)
					}
				}
				mu.Unlock()
			}
			if readErr != nil {
				done <- readErr
				return
			}
		}
	}()

	waitForCount := func(target string, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			count := 0
			for _, line := range lines {
				if line == target {
					count++
				}
			}
			mu.Unlock()
			if count >= want {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("timed out waiting for %q %d time(s); got %v", target, want, lines)
	}

	waitForExactCount := func(target string, want int) {
		t.Helper()
		time.Sleep(300 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		count := 0
		for _, line := range lines {
			if line == target {
				count++
			}
		}
		if count != want {
			t.Fatalf("expected %q %d time(s), got %d in %v", target, want, count, lines)
		}
	}

	waitForCount("first-line", 1)
	waitForExactCount("first-line", 1)

	sessionTwoLogs := filepath.Join(root, "session_002", "logs")
	if err := os.MkdirAll(sessionTwoLogs, 0o755); err != nil {
		t.Fatalf("mkdir session two: %v", err)
	}
	logTwo := filepath.Join(sessionTwoLogs, "job-driver-main.log")
	if err := os.WriteFile(logTwo, []byte("second-line\n"), 0o644); err != nil {
		t.Fatalf("write second log: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "session_latest")); err != nil {
		t.Fatalf("remove session_latest symlink: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "session_002"), filepath.Join(root, "session_latest")); err != nil {
		t.Fatalf("update session_latest symlink: %v", err)
	}

	waitForCount("second-line", 1)
	waitForExactCount("second-line", 1)

	stop()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for offload script stdout to close")
	}
}

func TestScriptExitsAfterCompletionAndDrainsFinalLogs(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash sidecar script test requires Unix signal semantics")
	}

	root := t.TempDir()
	scriptPath := filepath.Join(root, "offload.sh")
	if err := os.WriteFile(scriptPath, []byte(Script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	logDir := filepath.Join(root, "session_001", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	logPath := filepath.Join(logDir, "job-driver-main.log")
	if err := os.WriteFile(logPath, []byte("first-line\n"), 0o644); err != nil {
		t.Fatalf("write initial log: %v", err)
	}
	if err := os.Symlink(filepath.Join(root, "session_001"), filepath.Join(root, "session_latest")); err != nil {
		t.Fatalf("create session_latest symlink: %v", err)
	}

	completionPath := filepath.Join(root, "tau-driver-complete")
	cmd := exec.Command("bash", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"TAU_RAY_TMP="+root,
		"TAU_RAY_LOG_POLL_SECONDS=0.05",
		"TAU_RAY_LOG_COMPLETION_FILE="+completionPath,
		"TAU_RAY_LOG_DRAIN_SECONDS=0.2",
	)
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	if err := cmd.Start(); err != nil {
		t.Fatalf("start script: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})

	time.Sleep(150 * time.Millisecond)
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open final log: %v", err)
	}
	if _, err := f.WriteString("final-line\n"); err != nil {
		_ = f.Close()
		t.Fatalf("append final log: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close final log: %v", err)
	}
	if err := os.WriteFile(completionPath, []byte("0\n"), 0o644); err != nil {
		t.Fatalf("write completion marker: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("script exited with error: %v\n%s", err, output.String())
		}
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatalf("script did not exit after completion marker:\n%s", output.String())
	}

	for _, want := range []string{"first-line", "final-line"} {
		if strings.Count(output.String(), want) != 1 {
			t.Fatalf("expected exactly one %q after bounded drain, got:\n%s", want, output.String())
		}
	}
}

func TestScriptExitsWhenContainerPIDReceivesSIGTERM(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash sidecar script test requires Unix signal semantics")
	}

	root := t.TempDir()
	scriptPath := filepath.Join(root, "offload.sh")
	if err := os.WriteFile(scriptPath, []byte(Script), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}
	logsDir := filepath.Join(root, "session_001", "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatalf("mkdir logs: %v", err)
	}
	if err := os.WriteFile(filepath.Join(logsDir, "job-driver-ready.log"), []byte("ready\n"), 0o644); err != nil {
		t.Fatalf("write readiness log: %v", err)
	}

	cmd := exec.Command("bash", scriptPath)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(),
		"TAU_RAY_TMP="+root,
		"TAU_RAY_LOG_POLL_SECONDS=0.1",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start script: %v", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			<-waitCh
		}
	})

	readyCh := make(chan bool, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "ready" {
				readyCh <- true
				return
			}
		}
		readyCh <- false
	}()
	select {
	case ready := <-readyCh:
		if !ready {
			t.Fatal("sidecar exited before tailing the readiness log")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for sidecar readiness")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal sidecar PID: %v", err)
	}

	select {
	case err := <-waitCh:
		if err != nil {
			t.Fatalf("sidecar exited with an error after SIGTERM: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("sidecar did not exit after its container PID received SIGTERM")
	}
}

func TestWrapShellScriptWritesCompletionAndPreservesExitCode(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash entrypoint wrapper test requires Unix shell semantics")
	}

	for _, tc := range []struct {
		name     string
		command  string
		exitCode int
	}{
		{name: "success", command: "printf 'done\\n'", exitCode: 0},
		{name: "failure", command: "printf 'failed\\n'; exit 23", exitCode: 23},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			completionPath := filepath.Join(t.TempDir(), "tau-driver-complete")
			cmd := exec.Command("bash", "-c", WrapShellScript(tc.command))
			cmd.Env = append(os.Environ(), "TAU_RAY_LOG_COMPLETION_FILE="+completionPath)
			output, err := cmd.CombinedOutput()
			if tc.exitCode == 0 && err != nil {
				t.Fatalf("wrapped command failed: %v\n%s", err, output)
			}
			if tc.exitCode != 0 {
				exitErr, ok := err.(*exec.ExitError)
				if !ok || exitErr.ExitCode() != tc.exitCode {
					t.Fatalf("wrapped exit = %v, want %d\n%s", err, tc.exitCode, output)
				}
			}
			marker, err := os.ReadFile(completionPath)
			if err != nil {
				t.Fatalf("read completion marker: %v", err)
			}
			want := fmt.Sprintf(`{"exit_code":%d}`+"\n", tc.exitCode)
			if string(marker) != want {
				t.Fatalf("completion marker = %q, want %q", marker, want)
			}
		})
	}
}

func TestWrapShellScriptForwardsTermToWorkload(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash entrypoint wrapper test requires Unix signal semantics")
	}

	completionPath := filepath.Join(t.TempDir(), "tau-driver-complete")
	command := "trap 'exit 42' TERM INT\nwhile true; do sleep 1; done"
	cmd := exec.Command("bash", "-c", WrapShellScript(command))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "TAU_RAY_LOG_COMPLETION_FILE="+completionPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapped command: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})

	time.Sleep(100 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal wrapper: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 42 {
			t.Fatalf("wrapped exit = %v, want forwarded child exit 42", err)
		}
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatal("wrapped workload did not receive TERM within 2s")
	}

	marker, err := os.ReadFile(completionPath)
	if err != nil {
		t.Fatalf("read completion marker: %v", err)
	}
	if string(marker) != `{"exit_code":42}`+"\n" {
		t.Fatalf("completion marker = %q, want forwarded exit code 42", marker)
	}
}

func TestWrapShellScriptTerminatesNestedForegroundProcess(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("bash entrypoint wrapper test requires Unix signal semantics")
	}

	root := t.TempDir()
	completionPath := filepath.Join(root, "tau-driver-complete")
	childPIDPath := filepath.Join(root, "child.pid")
	command := fmt.Sprintf(
		"python3 -c %s\nprintf 'finalizer should not run\\n'",
		shellTestQuote("import os,time; open("+strconv.Quote(childPIDPath)+",'w').write(str(os.getpid())); time.sleep(30)"),
	)
	cmd := exec.Command("bash", "-c", WrapShellScript(command))
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append(os.Environ(), "TAU_RAY_LOG_COMPLETION_FILE="+completionPath)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wrapped command: %v", err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	})

	var childPID int
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(childPIDPath)
		if err == nil {
			childPID, err = strconv.Atoi(string(raw))
			if err != nil {
				t.Fatalf("parse child pid: %v", err)
			}
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("nested workload did not publish its pid")
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal wrapper: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != 143 {
			t.Fatalf("wrapped exit = %v, want 143", err)
		}
	case <-time.After(2 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
		t.Fatal("wrapper did not terminate after forwarding TERM")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(childPID, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("nested workload process %d survived wrapper TERM", childPID)
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func toStrings(t *testing.T, v any) []string {
	t.Helper()
	items, ok := v.([]any)
	if !ok {
		t.Fatalf("want []any, got %T", v)
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		s, ok := item.(string)
		if !ok {
			t.Fatalf("want string element, got %T", item)
		}
		out = append(out, s)
	}
	return out
}
