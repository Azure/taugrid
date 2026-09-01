// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/tools/clientcmd"

	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

func TestTauRoutingHelperProcess(t *testing.T) {
	if os.Getenv("TAU_ROUTING_HELPER") != "1" {
		return
	}
	installClusterProfileClientForTest(
		t,
		resolvedWorkloadProfileForTest("test-routing", "jobqueue", 0, 1),
	)
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
	writeRunRoutingFile(t, filepath.Join(root, "alpha", "train.sh"), "#!/bin/sh\necho health\n")
	projectHealth := filepath.Join(root, "alpha", "tau", "health.yaml")
	writeRunRoutingFile(t, projectHealth, `name: alpha-project-health
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
	t.Run("project health resolves catalog connection", func(t *testing.T) {
		result := runTauRoutingSubprocess(t, root, "run", "health", "--project", "alpha", "--dry-run=client")
		if result.err != nil {
			t.Fatalf("project health: %v\nstderr:\n%s", result.err, result.stderr)
		}
		if !strings.Contains(result.stdout, "kind: Job") ||
			!strings.Contains(result.stdout, "namespace: catalog-namespace") ||
			!strings.Contains(result.stdout, "kueue.x-k8s.io/queue-name: jobqueue") {
			t.Fatalf("project health did not resolve catalog connection:\n%s", result.stdout)
		}
	})
	t.Run("explicit project health config", func(t *testing.T) {
		result := runTauRoutingSubprocess(
			t,
			root,
			"run",
			"--project",
			"alpha",
			"--config",
			projectHealth,
			"--context",
			"explicit",
			"--dry-run=client",
		)
		if result.err != nil {
			t.Fatalf("project health config: %v\nstderr:\n%s", result.err, result.stderr)
		}
		if !strings.Contains(result.stdout, "name: alpha-project-health") {
			t.Fatalf("project health did not render its file:\n%s", result.stdout)
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
			projectHealth,
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
			"--context",
			"operator-context",
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
			"--context",
			"operator-context",
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
    printf '%%s\n' '{"metadata":{"name":"sample","uid":"workspace-uid","generation":1},"spec":{"queue":"jobqueue","authorization":{"mode":"workspace-rbac"},"role":"tau-researcher-v1"},"status":{"phase":"Ready","observedGeneration":1,"target":{"resolvedNamespace":%q},"queue":{"localQueue":"jobqueue"}}}'
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
	kubeconfig := `apiVersion: v1
kind: Config
clusters:
- name: cluster
  cluster:
    server: https://routing.test.invalid
contexts:
- name: taugrid-flex
  context:
    cluster: cluster
    user: researcher
current-context: taugrid-flex
users:
- name: researcher
  user:
    token: test-token
`
	writeRunRoutingFile(t, kubeconfigPath, kubeconfig)
	parsedKubeconfig, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		t.Fatal(err)
	}
	rawCluster, err := json.Marshal(parsedKubeconfig.Clusters["cluster"])
	if err != nil {
		t.Fatal(err)
	}
	fingerprintSum := sha256.Sum256(rawCluster)
	accessFingerprint := "sha256:" + hex.EncodeToString(fingerprintSum[:])
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
		"access_method":        descriptor.Access.Method,
		"access_identity":      descriptor.AccessIdentity(),
		"access_fingerprint":   accessFingerprint,
		"authorization_mode":   descriptor.Authorization.Mode,
		"context_name":         descriptor.Cluster.ContextName,
		"kubeconfig_path":      kubeconfigPath,
		"namespace":            namespace,
		"queue":                "jobqueue",
		"required_role":        descriptor.Authorization.RequiredRole,
		"descriptor_path":      descriptorPath,
		"descriptor_digest":    digest,
		"workspace_revision":   "1",
		"workspace_uid":        "workspace-uid",
		"configured_at":        time.Now().UTC().Add(-20 * time.Minute),
		"verified_at":          time.Now().UTC().Add(-10 * time.Minute),
		"repository_root":      repositoryRoot,
		"private_cluster":      false,
		"network_instructions": "",
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(configDir, "connections", workspaceconnection.ConnectionKeyForDiscovery(workspaceconnection.Discovery{
		Path:           descriptorPath,
		RepositoryRoot: repositoryRoot,
		Descriptor:     descriptor,
		Digest:         digest,
	})+".json")
	writeRunRoutingFile(t, statePath, string(raw))
	t.Setenv("TAU_CONFIG_DIR", configDir)
	t.Setenv("KUBECONFIG", kubeconfigPath)
}
