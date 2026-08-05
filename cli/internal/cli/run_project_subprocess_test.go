package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

func TestTauRoutingHelperProcess(t *testing.T) {
	if os.Getenv("TAU_ROUTING_HELPER") != "1" {
		return
	}
	separator := -1
	for index, arg := range os.Args {
		if arg == "--" {
			separator = index
			break
		}
	}
	if separator < 0 {
		fmt.Fprintln(os.Stderr, "missing helper argument separator")
		os.Exit(2)
	}
	root := NewRoot()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.SetArgs(os.Args[separator+1:])
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	os.Exit(0)
}

func TestTauRoutingSubprocessMatrix(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	configureRunRoutingProfile(t)
	writeRunRoutingFile(t, filepath.Join(root, "beta", "train.sh"), "#!/bin/sh\necho train\n")
	writeRunRoutingFile(t, filepath.Join(root, "beta", "tau", "eval.yaml"), `name: beta-eval
engine: job
entrypoint: ../train.sh
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  queue: jobqueue
`)
	writeRunRoutingFile(t, filepath.Join(root, "alpha", "train.sh"), "#!/bin/sh\necho smoke\n")
	projectSmoke := filepath.Join(root, "alpha", "tau", "smoke.yaml")
	writeRunRoutingFile(t, projectSmoke, `name: alpha-project-smoke
engine: job
entrypoint: ../train.sh
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  queue: jobqueue
`)
	installFakeRoutingKubectl(t, "catalog-namespace")
	installCachedRoutingConnection(t, root, "catalog-namespace")
	symlinkConfig := filepath.Join(root, "alpha", "experiments", "actual", "tau.yaml")
	writeRunRoutingFile(t, symlinkConfig, fmt.Sprintf(`name: symlink-job
engine: job
entrypoint: %s
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  queue: jobqueue
`, filepath.Join(root, "alpha", "train.sh")))

	t.Run("unique target", func(t *testing.T) {
		result := runTauRoutingSubprocess(t, root, "run", "eval", "--context", "explicit", "--dry-run=client")
		if result.err != nil {
			t.Fatalf("unique target: %v\nstderr:\n%s", result.err, result.stderr)
		}
		if !strings.Contains(result.stdout, "name: beta-eval") || !strings.Contains(result.stdout, "kind: Job") {
			t.Fatalf("unique target output:\n%s", result.stdout)
		}
	})
	t.Run("duplicate target", func(t *testing.T) {
		result := runTauRoutingSubprocess(t, root, "run", "train", "--context", "explicit", "--dry-run=client")
		if result.err == nil ||
			!strings.Contains(result.stderr, `run target "train" exists in projects: alpha, beta`) ||
			!strings.Contains(result.stderr, "--project") {
			t.Fatalf("duplicate target err=%v\nstderr:\n%s", result.err, result.stderr)
		}
	})
	t.Run("built-in smoke ambiguity", func(t *testing.T) {
		result := runTauRoutingSubprocess(t, root, "run", "smoke")
		if result.err == nil || !strings.Contains(result.stderr, "Valid projects: alpha, beta") {
			t.Fatalf("smoke ambiguity err=%v\nstderr:\n%s", result.err, result.stderr)
		}
	})
	t.Run("stale configured connection refreshes for project smoke", func(t *testing.T) {
		result := runTauRoutingSubprocess(t, root, "run", "smoke", "--project", "alpha", "--dry-run=client")
		if result.err != nil {
			t.Fatalf("stale project smoke: %v\nstderr:\n%s", result.err, result.stderr)
		}
		if !strings.Contains(result.stdout, "kind: Job") ||
			!strings.Contains(result.stdout, "namespace: catalog-namespace") ||
			!strings.Contains(result.stdout, "kueue.x-k8s.io/queue-name: jobqueue") {
			t.Fatalf("stale project smoke did not consume refreshed connection:\n%s", result.stdout)
		}
	})
	t.Run("explicit project smoke config", func(t *testing.T) {
		result := runTauRoutingSubprocess(
			t,
			root,
			"run",
			"--project",
			"alpha",
			"--config",
			projectSmoke,
			"--context",
			"explicit",
			"--dry-run=client",
		)
		if result.err != nil {
			t.Fatalf("project smoke config: %v\nstderr:\n%s", result.err, result.stderr)
		}
		if !strings.Contains(result.stdout, "name: alpha-project-smoke") || strings.Contains(result.stdout, "Platform reachable:") {
			t.Fatalf("project smoke did not render its file:\n%s", result.stdout)
		}
	})
	t.Run("lifecycle project connection", func(t *testing.T) {
		result := runTauRoutingSubprocess(t, root, "run", "list", "--project", "alpha")
		if result.err != nil {
			t.Fatalf("project lifecycle: %v\nstderr:\n%s", result.err, result.stderr)
		}
		if !strings.Contains(result.stdout, `No jobs found in namespace "catalog-namespace"`) {
			t.Fatalf("project lifecycle output:\n%s", result.stdout)
		}
	})
	t.Run("lifecycle explicit context bypass", func(t *testing.T) {
		result := runTauRoutingSubprocess(t, root, "run", "list", "--context", "explicit", "--namespace", "explicit-ns")
		if result.err != nil {
			t.Fatalf("explicit lifecycle: %v\nstderr:\n%s", result.err, result.stderr)
		}
		if !strings.Contains(result.stdout, `No jobs found in namespace "explicit-ns"`) {
			t.Fatalf("explicit lifecycle output:\n%s", result.stdout)
		}
	})
	t.Run("resume mismatch before cluster access", func(t *testing.T) {
		result := runTauRoutingSubprocess(
			t,
			root,
			"run",
			"resume",
			"resume-job",
			"--project",
			"beta",
			"--config",
			projectSmoke,
		)
		if result.err == nil || !strings.Contains(result.stderr, `--project "beta" does not own`) {
			t.Fatalf("resume mismatch err=%v\nstderr:\n%s", result.err, result.stderr)
		}
	})
	if runtime.GOOS != "windows" {
		t.Run("internal config symlink", func(t *testing.T) {
			link := filepath.Join(root, "alpha", "experiments", "linked", "tau.yaml")
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(symlinkConfig, link); err != nil {
				t.Fatal(err)
			}
			result := runTauRoutingSubprocess(
				t,
				root,
				"run",
				"--config",
				link,
				"--context",
				"explicit",
				"--dry-run=client",
			)
			if result.err != nil || !strings.Contains(result.stdout, "name: symlink-job") {
				t.Fatalf("internal symlink err=%v\nstderr:\n%s\nstdout:\n%s", result.err, result.stderr, result.stdout)
			}
		})
		t.Run("external config symlink", func(t *testing.T) {
			external := filepath.Join(t.TempDir(), "tau.yaml")
			if err := os.Symlink(symlinkConfig, external); err != nil {
				t.Fatal(err)
			}
			result := runTauRoutingSubprocess(
				t,
				filepath.Dir(external),
				"run",
				"--config",
				external,
				"--context",
				"explicit",
				"--dry-run=client",
			)
			if result.err == nil || !strings.Contains(result.stderr, "not owned by any Tau project") {
				t.Fatalf("external symlink err=%v\nstderr:\n%s", result.err, result.stderr)
			}
		})
		t.Run("config symlink escape", func(t *testing.T) {
			outside := filepath.Join(t.TempDir(), "tau.yaml")
			writeRunRoutingFile(t, outside, fmt.Sprintf(`name: outside
engine: job
entrypoint: %s
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
`, filepath.Join(root, "alpha", "train.sh")))
			link := filepath.Join(root, "alpha", "experiments", "escape", "tau.yaml")
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			result := runTauRoutingSubprocess(
				t,
				root,
				"run",
				"--config",
				link,
				"--context",
				"explicit",
				"--dry-run=client",
			)
			if result.err == nil || !strings.Contains(result.stderr, "not owned by any Tau project") {
				t.Fatalf("escape symlink err=%v\nstderr:\n%s", result.err, result.stderr)
			}
		})
		t.Run("config directory symlink escape", func(t *testing.T) {
			outsideDir := t.TempDir()
			writeRunRoutingFile(t, filepath.Join(outsideDir, "tau.yaml"), fmt.Sprintf(`name: outside-directory
engine: job
entrypoint: %s
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
`, filepath.Join(root, "alpha", "train.sh")))
			linkDir := filepath.Join(root, "alpha", "experiments", "directory-escape")
			if err := os.Symlink(outsideDir, linkDir); err != nil {
				t.Fatal(err)
			}
			result := runTauRoutingSubprocess(
				t,
				root,
				"run",
				"--config",
				filepath.Join(linkDir, "tau.yaml"),
				"--context",
				"explicit",
				"--dry-run=client",
			)
			if result.err == nil || !strings.Contains(result.stderr, "not owned by any Tau project") {
				t.Fatalf("directory escape err=%v\nstderr:\n%s", result.err, result.stderr)
			}
		})
	}
}

func TestTauRoutingMixedBoundarySubprocess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	configureRunRoutingProfile(t)

	t.Run("Git CWD to no-Git config", func(t *testing.T) {
		gitRoot := t.TempDir()
		initRunRoutingRepo(t, gitRoot)
		writeRunRoutingFile(t, filepath.Join(gitRoot, "tau", "workspace.connection.yaml"), runRoutingDescriptor)
		outside := t.TempDir()
		script := filepath.Join(outside, "run.sh")
		writeRunRoutingFile(t, script, "#!/bin/sh\necho mixed\n")
		writeRunRoutingFile(t, filepath.Join(outside, "tau.yaml"), fmt.Sprintf(`name: mixed-boundary
engine: job
entrypoint: %s
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  queue: jobqueue
`, script))
		linkDir := filepath.Join(gitRoot, "linked")
		if err := os.Symlink(outside, linkDir); err != nil {
			t.Fatal(err)
		}

		result := runTauRoutingSubprocess(
			t,
			gitRoot,
			"run",
			"--config",
			filepath.Join(linkDir, "tau.yaml"),
			"--dry-run=client",
		)
		if result.err != nil || !strings.Contains(result.stdout, "name: mixed-boundary") {
			t.Fatalf("no-Git side was not selected: err=%v\nstderr:\n%s\nstdout:\n%s", result.err, result.stderr, result.stdout)
		}

		writeRunRoutingFile(t, filepath.Join(outside, "tau", "workspace.connection.yaml"), "schema: wrong\n")
		result = runTauRoutingSubprocess(
			t,
			gitRoot,
			"run",
			"--config",
			filepath.Join(linkDir, "tau.yaml"),
			"--dry-run=client",
		)
		if result.err == nil || !strings.Contains(result.stderr, `schema "wrong" is unsupported`) {
			t.Fatalf("no-Git descriptor was not used: err=%v\nstderr:\n%s", result.err, result.stderr)
		}

		result = runTauRoutingSubprocess(
			t,
			gitRoot,
			"run",
			"--config",
			filepath.Join(linkDir, "tau.yaml"),
			"--context",
			"operator-context",
			"--dry-run=client",
		)
		if result.err != nil || !strings.Contains(result.stdout, "name: mixed-boundary") {
			t.Fatalf("explicit context did not bypass descriptor: err=%v\nstderr:\n%s", result.err, result.stderr)
		}
	})

	t.Run("no-Git CWD to Git config", func(t *testing.T) {
		gitTarget := t.TempDir()
		initRunRoutingRepo(t, gitTarget)
		writeRunRoutingFile(t, filepath.Join(gitTarget, "tau", "workspace.connection.yaml"), runRoutingDescriptor)
		script := filepath.Join(gitTarget, "run.sh")
		writeRunRoutingFile(t, script, "#!/bin/sh\necho inverse\n")
		writeRunRoutingFile(t, filepath.Join(gitTarget, "tau.yaml"), fmt.Sprintf(`name: inverse-boundary
engine: job
entrypoint: %s
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  queue: jobqueue
`, script))
		noGitRoot := t.TempDir()
		linkDir := filepath.Join(noGitRoot, "linked")
		if err := os.Symlink(gitTarget, linkDir); err != nil {
			t.Fatal(err)
		}
		result := runTauRoutingSubprocess(
			t,
			noGitRoot,
			"run",
			"--config",
			filepath.Join(linkDir, "tau.yaml"),
			"--dry-run=client",
		)
		if result.err != nil || !strings.Contains(result.stdout, "name: inverse-boundary") {
			t.Fatalf("inverse no-Git side was not selected: err=%v\nstderr:\n%s\nstdout:\n%s", result.err, result.stderr, result.stdout)
		}
	})
}

type tauRoutingSubprocessResult struct {
	stdout string
	stderr string
	err    error
}

func runTauRoutingSubprocess(t *testing.T, cwd string, args ...string) tauRoutingSubprocessResult {
	t.Helper()
	commandArgs := []string{"-test.run=^TestTauRoutingHelperProcess$", "--"}
	commandArgs = append(commandArgs, args...)
	command := exec.Command(os.Args[0], commandArgs...)
	command.Dir = cwd
	command.Env = append(os.Environ(), "TAU_ROUTING_HELPER=1")
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	return tauRoutingSubprocessResult{stdout: stdout.String(), stderr: stderr.String(), err: err}
}

func installFakeRoutingKubectl(t *testing.T, namespace string) {
	t.Helper()
	binDir := t.TempDir()
	path := filepath.Join(binDir, "kubectl")
	script := fmt.Sprintf(`#!/bin/sh
case " $* " in
  *" get workspace.tau.azure.com sample "*|*" get workspaces.tau.azure.com sample "*)
    printf '%%s\n' '{"metadata":{"name":"sample","uid":"workspace-uid","generation":1},"spec":{"queue":"jobqueue"},"status":{"phase":"Ready","observedGeneration":1,"target":{"resolvedNamespace":%q},"queue":{"localQueue":"jobqueue"}}}'
    ;;
  *" get localqueue.kueue.x-k8s.io jobqueue "*)
    printf '%%s\n' 'localqueue.kueue.x-k8s.io/jobqueue'
    ;;
  *" auth can-i "*)
    printf '%%s\n' 'yes'
    ;;
  *)
    printf '%%s\n' '{"items":[]}'
    ;;
esac
`, namespace)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installCachedRoutingConnection(t *testing.T, root, namespace string) {
	t.Helper()
	descriptor, err := workspaceconnection.Parse([]byte(runRoutingDescriptor))
	if err != nil {
		t.Fatal(err)
	}
	digest, err := workspaceconnection.Digest(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	configDir := t.TempDir()
	kubeconfigPath := filepath.Join(configDir, "kubeconfig.yaml")
	writeRunRoutingFile(t, kubeconfigPath, "apiVersion: v1\nkind: Config\n")
	descriptorPath, err := filepath.EvalSymlinks(filepath.Join(root, "connections", "shared.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	repositoryRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"schema":               "tau.workspace.connection-state.v1",
		"workspace":            descriptor.Workspace,
		"resource_id":          descriptor.Cluster.ResourceID,
		"authorization_mode":   descriptor.Authorization.Mode,
		"context_name":         descriptor.Cluster.ContextName,
		"kubeconfig_path":      kubeconfigPath,
		"namespace":            namespace,
		"queue":                "jobqueue",
		"required_role":        "",
		"descriptor_path":      descriptorPath,
		"descriptor_digest":    digest,
		"workspace_revision":   "1",
		"verified_at":          time.Now().UTC().Add(-10 * time.Minute),
		"repository_root":      repositoryRoot,
		"private_cluster":      false,
		"network_instructions": "",
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", workspaceconnection.ConnectionKey(descriptor)+".json")
	writeRunRoutingFile(t, statePath, string(raw))
	t.Setenv("TAU_CONFIG_DIR", configDir)
}
