package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/resume"
	"github.com/Azure/taugrid/cli/internal/storage"
	tauworkspace "github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/experiment"
	"github.com/Azure/taugrid/core/status"
	"github.com/Azure/taugrid/core/workloadmeta"
)

func failedJobSnapshot(name string) status.Snapshot {
	return status.Snapshot{
		Name:      name,
		Namespace: "ray",
		JobFound:  true,
		JobConditions: []status.Condition{
			{Type: "Failed", Status: "True"},
		},
		Pods: []status.Pod{
			{Phase: "Failed"},
		},
	}
}

func multiKueueResumeCleanupSnapshot(name string, workloadNames ...string) status.Snapshot {
	workloads := make([]status.Workload, 0, len(workloadNames))
	for _, workloadName := range workloadNames {
		workloads = append(workloads, status.Workload{
			Name:        workloadName,
			ClusterName: "worker-a",
		})
	}
	return status.Snapshot{
		Name:         name,
		Namespace:    "ray",
		JobFound:     true,
		JobManagedBy: "kueue.x-k8s.io/multikueue",
		Workloads:    workloads,
	}
}

func TestResumePreflightNotFound(t *testing.T) {
	snap := status.Snapshot{Name: "missing", Namespace: "ray"}
	_, err := resumePreflight(snap, "missing", "", "", false)
	if err == nil {
		t.Fatal("expected error for missing workload")
	}
	if got := err.Error(); !strings.Contains(got, "no Job or RayJob") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestResolveResumeRoutingVerifiesProjectBeforeConnection(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	config := filepath.Join(root, "alpha", "experiments", "resume", "tau.yaml")
	writeRunRoutingFile(t, config, "name: resume-job\n")
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		ContextName: "catalog-context",
		Namespace:   "catalog-namespace",
	}}

	_, _, err := resolveResumeRouting(
		command,
		root,
		"beta",
		config,
		"ambient-context",
		"",
		false,
		false,
		ensurer,
	)
	if err == nil || !strings.Contains(err.Error(), "does not own") {
		t.Fatalf("expected project/config mismatch, got %v", err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("resume mismatch contacted connection manager %d times", ensurer.calls)
	}

	routing, restore, err := resolveResumeRouting(
		command,
		root,
		"alpha",
		config,
		"ambient-context",
		"",
		false,
		false,
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if routing.ConfigPath != config ||
		routing.KubeContext != "catalog-context" ||
		routing.Namespace != "catalog-namespace" {
		t.Fatalf("routing = %#v", routing)
	}
	if ensurer.calls != 1 || len(ensurer.discoveries) != 1 {
		t.Fatalf("calls=%d discoveries=%d", ensurer.calls, len(ensurer.discoveries))
	}
}

func TestResumeWorkspaceConflictHasNoRemoteSideEffects(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	withRunRoutingCWD(t, root)
	configureRunRoutingProfile(t)
	script := filepath.Join(root, "alpha", "resume.sh")
	writeRunRoutingFile(t, script, "#!/bin/sh\necho resume\n")
	config := filepath.Join(root, "alpha", "experiments", "resume-conflict.yaml")
	writeRunRoutingFile(t, config, `name: resume-job
engine: job
entrypoint: ../resume.sh
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  workspace: other
`)
	ensurer := &fakeRunConnectionEnsurer{}
	fetchCalls := 0
	deleteCalls := 0
	executeCalls := 0
	command := newResumeTestCommand(
		func(*cobra.Command) runConnectionEnsurer { return ensurer },
		resumeCommandHooks{
			fetchStatus: func(context.Context, string, string, string) (status.Snapshot, error) {
				fetchCalls++
				return status.Snapshot{}, nil
			},
			deleteOld: func(context.Context, string, string, string, io.Writer) error {
				deleteCalls++
				return nil
			},
			executeTarget: func(*cobra.Command, resolvedRunTarget, string, runExperimentMetadata) error {
				executeCalls++
				return nil
			},
		},
	)
	command.SetArgs([]string{"resume", "resume-job", "--project", "alpha", "--config", config})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), `policy.workspace "other"`) {
		t.Fatalf("expected workspace conflict, got %v", err)
	}
	if ensurer.calls != 0 || fetchCalls != 0 || deleteCalls != 0 || executeCalls != 0 {
		t.Fatalf(
			"workspace conflict made ensurer=%d fetch=%d delete=%d execute=%d calls",
			ensurer.calls,
			fetchCalls,
			deleteCalls,
			executeCalls,
		)
	}
}

func TestResumeMatchingWorkspaceActivatesBeforeRemoteWork(t *testing.T) {
	root := multiProjectRunRoutingRepo(t)
	withRunRoutingCWD(t, root)
	configureRunRoutingProfile(t)
	script := filepath.Join(root, "alpha", "resume.sh")
	writeRunRoutingFile(t, script, "#!/bin/sh\necho resume\n")
	config := filepath.Join(root, "alpha", "experiments", "resume-match.yaml")
	writeRunRoutingFile(t, config, `name: resume-job
engine: job
entrypoint: ../resume.sh
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
  workspace: sample
`)
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace:   "sample",
		ContextName: "catalog-context",
		Namespace:   "catalog-namespace",
	}}
	fetchCalls := 0
	workspaceCalls := 0
	validateCalls := 0
	deleteCalls := 0
	executeCalls := 0
	currentWorkspace := readyWorkspace()
	currentWorkspace.Spec.Queue = "jobqueue"
	currentWorkspace.Spec.Target.Namespace = "catalog-namespace"
	currentWorkspace.Spec.Defaults.OutputRoot = "/data/workspaces/sample"
	currentWorkspace.Status.Target.ResolvedNamespace = "catalog-namespace"
	currentWorkspace.Status.Queue.LocalQueue = "jobqueue"
	command := newResumeTestCommand(
		func(*cobra.Command) runConnectionEnsurer { return ensurer },
		resumeCommandHooks{
			fetchStatus: func(_ context.Context, kubeContext, namespace, name string) (status.Snapshot, error) {
				fetchCalls++
				if kubeContext != "catalog-context" || namespace != "catalog-namespace" || name != "resume-job" {
					t.Fatalf("fetch route context=%q namespace=%q name=%q", kubeContext, namespace, name)
				}
				snapshot := failedJobSnapshot(name)
				snapshot.Events = []status.Event{{Reason: "Evicted"}}
				return snapshot, nil
			},
			fetchWorkspace: func(_ *cobra.Command, kubeContext, namespace, name string) (tauworkspace.Workspace, error) {
				workspaceCalls++
				if kubeContext != "catalog-context" || namespace != tauworkspace.PlatformNamespace || name != "sample" {
					t.Fatalf("workspace route context=%q namespace=%q name=%q", kubeContext, namespace, name)
				}
				return currentWorkspace, nil
			},
			validateTarget: func(_ *cobra.Command, target resolvedRunTarget, _ string, _ runExperimentMetadata) error {
				validateCalls++
				if target.job == nil {
					t.Fatal("resume preflight did not resolve a typed Job target")
				}
				options := target.job.Options
				if options.dryRun != "client" ||
					options.workspace != "sample" ||
					options.workspaceResultScope != "/data/workspaces/sample" ||
					options.queue != "jobqueue" {
					t.Fatalf("preflight Job options = %#v", options)
				}
				return nil
			},
			deleteOld: func(_ context.Context, kubeContext, namespace, name string, _ io.Writer) error {
				deleteCalls++
				if kubeContext != "catalog-context" || namespace != "catalog-namespace" || name != "resume-job" {
					t.Fatalf("delete route context=%q namespace=%q name=%q", kubeContext, namespace, name)
				}
				return nil
			},
			executeTarget: func(*cobra.Command, resolvedRunTarget, string, runExperimentMetadata) error {
				executeCalls++
				return nil
			},
		},
	)
	command.SetArgs([]string{"resume", "resume-job", "--project", "alpha", "--config", config})
	if err := command.Execute(); err != nil {
		t.Fatalf("matching workspace resume: %v", err)
	}
	if ensurer.calls != 1 || fetchCalls != 1 || workspaceCalls != 1 || validateCalls != 1 || deleteCalls != 1 || executeCalls != 1 {
		t.Fatalf(
			"matching workspace made ensurer=%d fetch=%d workspace=%d validate=%d delete=%d execute=%d calls",
			ensurer.calls,
			fetchCalls,
			workspaceCalls,
			validateCalls,
			deleteCalls,
			executeCalls,
		)
	}
}

func TestRunResumePreservesMetricsSessionFromFailedJob(t *testing.T) {
	zeroGPUs := 0
	config := filepath.Join(t.TempDir(), "tau.yaml")
	writeRunRoutingFile(t, config, "name: resume-job\n")
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	var gotSession string
	var gotOutput string
	err := runResumeCommand(
		command,
		"resume-job",
		resumeRouting{
			ConfigPath:  config,
			KubeContext: "test-context",
			Namespace:   "ray",
			TargetOptions: runDispatchOptions{
				engine:                "job",
				script:                "train.py",
				metricsOffloadEnabled: true,
				dataPVC:               "metrics-pvc",
				workers:               1,
				jobGPUs:               &zeroGPUs,
				gpusPerWorker:         1,
			},
		},
		"/data/resume-job/checkpoints",
		"client",
		true,
		resumeCommandHooks{
			fetchStatus: func(context.Context, string, string, string) (status.Snapshot, error) {
				snapshot := failedJobSnapshot("resume-job")
				snapshot.Events = []status.Event{{Reason: "Evicted"}}
				snapshot.Annotations = map[string]string{
					workloadmeta.AnnotationMetricsSession: "session-from-failed-job",
					experiment.AnnotationResultPath:       "/data/runs/resume-job-original",
					experiment.AnnotationResultPVC:        "metrics-pvc",
				}
				return snapshot, nil
			},
			executeTarget: func(_ *cobra.Command, target resolvedRunTarget, _ string, _ runExperimentMetadata) error {
				if target.job == nil {
					t.Fatal("resume did not resolve a direct Job")
				}
				gotSession = target.job.Options.metricsSessionID
				gotOutput = target.job.Options.output
				return nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if gotSession != "session-from-failed-job" {
		t.Fatalf("resumed metrics session = %q", gotSession)
	}
	if gotOutput != "/data/runs/resume-job-original" {
		t.Fatalf("resumed metrics output = %q", gotOutput)
	}
}

func TestValidateMetricsResumeStateLocationRejectsOutputDrift(t *testing.T) {
	snapshot := status.Snapshot{Annotations: map[string]string{
		workloadmeta.AnnotationMetricsSession: "session-from-failed-job",
		experiment.AnnotationResultPath:       "/data/runs/original",
		experiment.AnnotationResultPVC:        "original-pvc",
	}}
	options := runDispatchOptions{
		metricsOffloadEnabled: true,
		output:                "/data/runs/changed",
		dataPVC:               "original-pvc",
	}
	if err := validateMetricsResumeStateLocation(snapshot, options, "resume-job"); err == nil ||
		!strings.Contains(err.Error(), "cannot change storage.output") {
		t.Fatalf("output drift error = %v", err)
	}

	options.output = "/data/runs/original"
	options.dataPVC = "changed-pvc"
	if err := validateMetricsResumeStateLocation(snapshot, options, "resume-job"); err == nil ||
		!strings.Contains(err.Error(), "cannot change the output PVC") {
		t.Fatalf("PVC drift error = %v", err)
	}

	options.dataPVC = "original-pvc"
	if err := validateMetricsResumeStateLocation(snapshot, options, "resume-job"); err != nil {
		t.Fatalf("unchanged telemetry storage rejected: %v", err)
	}
}

func TestResumeDoesNotDeleteWhenReplacementValidationFails(t *testing.T) {
	root := t.TempDir()
	initRunRoutingRepo(t, root)
	withRunRoutingCWD(t, root)
	configureRunRoutingProfile(t)
	script := filepath.Join(root, "resume.sh")
	writeRunRoutingFile(t, script, "#!/bin/sh\necho resume\n")
	config := filepath.Join(root, "tau.yaml")
	writeRunRoutingFile(t, config, `name: resume-job
engine: job
entrypoint: resume.sh
compute:
  gpus: 0
runtime:
  image: busybox:1.36
policy:
  profile: test-routing
`)
	deleteCalls := 0
	executeCalls := 0
	command := newResumeTestCommand(
		func(*cobra.Command) runConnectionEnsurer { return &fakeRunConnectionEnsurer{} },
		resumeCommandHooks{
			fetchStatus: func(context.Context, string, string, string) (status.Snapshot, error) {
				snapshot := failedJobSnapshot("resume-job")
				snapshot.Events = []status.Event{{Reason: "Evicted"}}
				return snapshot, nil
			},
			validateTarget: func(*cobra.Command, resolvedRunTarget, string, runExperimentMetadata) error {
				return errors.New("replacement render failed")
			},
			deleteOld: func(context.Context, string, string, string, io.Writer) error {
				deleteCalls++
				return nil
			},
			executeTarget: func(*cobra.Command, resolvedRunTarget, string, runExperimentMetadata) error {
				executeCalls++
				return nil
			},
		},
	)
	command.SetArgs([]string{"resume", "resume-job", "--config", config, "--context", "test", "--namespace", "ray"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), "validating replacement workload") {
		t.Fatalf("expected replacement validation error, got %v", err)
	}
	if deleteCalls != 0 || executeCalls != 0 {
		t.Fatalf("validation failure made delete=%d execute=%d calls", deleteCalls, executeCalls)
	}
}

func TestResumeDoesNotDeleteWhenWorkspaceNamespaceChanged(t *testing.T) {
	config := filepath.Join(t.TempDir(), "tau.yaml")
	writeRunRoutingFile(t, config, "name: resume-job\n")
	command := &cobra.Command{}
	command.SetContext(context.Background())
	command.SetOut(io.Discard)
	command.SetErr(io.Discard)
	currentWorkspace := readyWorkspace()
	currentWorkspace.Spec.Target.Namespace = "sample-new"
	currentWorkspace.Status.Target.ResolvedNamespace = "sample-new"
	deleteCalls := 0
	executeCalls := 0
	err := runResumeCommand(
		command,
		"resume-job",
		resumeRouting{
			ConfigPath:  config,
			KubeContext: "test-context",
			Namespace:   "sample-old",
			TargetOptions: runDispatchOptions{
				workspace: "sample",
			},
		},
		"",
		"",
		false,
		resumeCommandHooks{
			fetchStatus: func(context.Context, string, string, string) (status.Snapshot, error) {
				snapshot := failedJobSnapshot("resume-job")
				snapshot.Events = []status.Event{{Reason: "Evicted"}}
				return snapshot, nil
			},
			fetchWorkspace: func(*cobra.Command, string, string, string) (tauworkspace.Workspace, error) {
				return currentWorkspace, nil
			},
			deleteOld: func(context.Context, string, string, string, io.Writer) error {
				deleteCalls++
				return nil
			},
			executeTarget: func(*cobra.Command, resolvedRunTarget, string, runExperimentMetadata) error {
				executeCalls++
				return nil
			},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "refusing to delete it") {
		t.Fatalf("expected namespace drift refusal, got %v", err)
	}
	if deleteCalls != 0 || executeCalls != 0 {
		t.Fatalf("namespace drift made delete=%d execute=%d calls", deleteCalls, executeCalls)
	}
}

func TestResumeEmptyExplicitContextDoesNotBypassNoGitGuard(t *testing.T) {
	for _, kubeContext := range []string{"", " \t "} {
		t.Run(fmt.Sprintf("context-%q", kubeContext), func(t *testing.T) {
			root := t.TempDir()
			config := filepath.Join(root, "tau.yaml")
			writeRunRoutingFile(t, config, "name: resume-job\n")
			command := &cobra.Command{}
			command.SetContext(context.Background())
			ensurer := &fakeRunConnectionEnsurer{}
			_, _, err := resolveResumeRouting(
				command,
				root,
				"",
				config,
				kubeContext,
				"ray",
				true,
				false,
				ensurer,
			)
			if err == nil || !strings.Contains(err.Error(), "outside a Git repository") {
				t.Fatalf("expected no-Git routing error, got %v", err)
			}
			if ensurer.calls != 0 {
				t.Fatalf("empty explicit context activated connection %d times", ensurer.calls)
			}
		})
	}
}

func TestResumeNoCatalogWorkspaceStillActivatesDescriptor(t *testing.T) {
	root := t.TempDir()
	initRunRoutingRepo(t, root)
	config := filepath.Join(root, "tau.yaml")
	writeRunRoutingFile(t, config, `name: resume-job
policy:
  workspace: legacy-workspace
`)
	command := &cobra.Command{}
	command.SetContext(context.Background())
	ensurer := &fakeRunConnectionEnsurer{connection: workspaceconnection.ActiveConnection{
		Workspace:   "descriptor-workspace",
		ContextName: "descriptor-context",
		Namespace:   "descriptor-namespace",
	}}
	routing, restore, err := resolveResumeRouting(
		command,
		root,
		"",
		config,
		defaultKubeContext(),
		"ray",
		false,
		false,
		ensurer,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer restore()
	if ensurer.calls != 1 {
		t.Fatalf("descriptor activation calls = %d", ensurer.calls)
	}
	if routing.KubeContext != "descriptor-context" || routing.Namespace != "descriptor-namespace" {
		t.Fatalf("routing = %#v", routing)
	}
	if routing.TargetOptions.workspace != "legacy-workspace" {
		t.Fatalf("config workspace was not preserved: %#v", routing.TargetOptions)
	}
}

func TestResumeExternalSymlinkToNoCatalogRepoCannotUseAmbientCluster(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	repo := t.TempDir()
	initRunRoutingRepo(t, repo)
	actual := filepath.Join(repo, "tau.yaml")
	writeRunRoutingFile(t, actual, "name: resume-job\n")
	externalDir := t.TempDir()
	link := filepath.Join(externalDir, "tau.yaml")
	if err := os.Symlink(actual, link); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.SetContext(context.Background())
	ensurer := &fakeRunConnectionEnsurer{}
	_, _, err := resolveResumeRouting(
		command,
		externalDir,
		"",
		link,
		defaultKubeContext(),
		"ray",
		false,
		false,
		ensurer,
	)
	if err == nil || !strings.Contains(err.Error(), "outside a Git repository") {
		t.Fatalf("expected external symlink no-Git guard, got %v", err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("external no-catalog symlink activated connection %d times", ensurer.calls)
	}
}

func TestResumeInternalDirectorySymlinkToNoGitCannotUseAmbientCluster(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink setup requires elevated privileges on Windows")
	}
	repo := t.TempDir()
	initRunRoutingRepo(t, repo)
	outside := t.TempDir()
	writeRunRoutingFile(t, filepath.Join(outside, "tau.yaml"), "name: resume-job\n")
	linkDir := filepath.Join(repo, "linked")
	if err := os.Symlink(outside, linkDir); err != nil {
		t.Fatal(err)
	}
	command := &cobra.Command{}
	command.SetContext(context.Background())
	ensurer := &fakeRunConnectionEnsurer{}
	_, _, err := resolveResumeRouting(
		command,
		repo,
		"",
		filepath.Join(linkDir, "tau.yaml"),
		defaultKubeContext(),
		"ray",
		false,
		false,
		ensurer,
	)
	if err == nil || !strings.Contains(err.Error(), "outside a Git repository") {
		t.Fatalf("expected mixed-boundary no-Git guard, got %v", err)
	}
	if ensurer.calls != 0 {
		t.Fatalf("mixed-boundary resume activated connection %d times", ensurer.calls)
	}
}

func TestResumeStillRequiresExplicitConfig(t *testing.T) {
	root := t.TempDir()
	initRunRoutingRepo(t, root)
	writeRunRoutingFile(t, filepath.Join(root, "tau.yaml"), "name: inferred-default\n")
	withRunRoutingCWD(t, root)
	ensurer := &fakeRunConnectionEnsurer{}
	fetchCalls := 0
	deleteCalls := 0
	command := newResumeTestCommand(
		func(*cobra.Command) runConnectionEnsurer { return ensurer },
		resumeCommandHooks{
			fetchStatus: func(context.Context, string, string, string) (status.Snapshot, error) {
				fetchCalls++
				return status.Snapshot{}, nil
			},
			deleteOld: func(context.Context, string, string, string, io.Writer) error {
				deleteCalls++
				return nil
			},
		},
	)
	command.SetArgs([]string{"resume", "resume-job"})
	err := command.Execute()
	if err == nil || !strings.Contains(err.Error(), `required flag(s) "config" not set`) {
		t.Fatalf("expected required --config error, got %v", err)
	}
	if ensurer.calls != 0 || fetchCalls != 0 || deleteCalls != 0 {
		t.Fatalf("missing config made ensurer=%d fetch=%d delete=%d calls", ensurer.calls, fetchCalls, deleteCalls)
	}
}

func newResumeTestCommand(
	connectionFactory runConnectionEnsurerFactory,
	hooks resumeCommandHooks,
) *cobra.Command {
	parent := &cobra.Command{Use: "run"}
	parent.PersistentFlags().String("project", "", "")
	parent.AddCommand(newRunResumeCmdWithDependencies(connectionFactory, hooks))
	parent.SetContext(context.Background())
	parent.SetOut(io.Discard)
	parent.SetErr(io.Discard)
	return parent
}

func TestResumePreflightCompleted(t *testing.T) {
	snap := status.Snapshot{
		Name:      "done",
		Namespace: "ray",
		JobFound:  true,
		JobConditions: []status.Condition{
			{Type: "Complete", Status: "True"},
		},
	}
	_, err := resumePreflight(snap, "done", "", "", false)
	if err == nil {
		t.Fatal("expected error for completed workload")
	}
	if got := err.Error(); !strings.Contains(got, "completed successfully") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestResumePreflightRunning(t *testing.T) {
	snap := status.Snapshot{
		Name:      "running",
		Namespace: "ray",
		JobFound:  true,
		JobActive: 1,
	}
	_, err := resumePreflight(snap, "running", "", "", false)
	if err == nil {
		t.Fatal("expected error for running workload")
	}
	if got := err.Error(); !strings.Contains(got, "still running") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestResumePreflightOOMBlocked(t *testing.T) {
	snap := failedJobSnapshot("oom-job")
	snap.Pods[0].OOMKilled = true
	_, err := resumePreflight(snap, "oom-job", "", "", false)
	if err == nil {
		t.Fatal("expected error for OOM without --force")
	}
	if got := err.Error(); !strings.Contains(got, "OOM") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestResumePreflightOOMForced(t *testing.T) {
	snap := failedJobSnapshot("oom-job")
	snap.Pods[0].OOMKilled = true
	plan, err := resumePreflight(snap, "oom-job", "", "", true)
	if err != nil {
		t.Fatalf("--force should allow OOM resume: %v", err)
	}
	if plan.Reason != resume.ReasonOOMKilled {
		t.Fatalf("expected OOMKilled reason, got %v", plan.Reason)
	}
}

func TestResumePreflightPreempted(t *testing.T) {
	snap := failedJobSnapshot("preempted-job")
	snap.Pods[0].Conditions = []status.Condition{
		{Type: "DisruptionTarget", Status: "True"},
	}
	plan, err := resumePreflight(snap, "preempted-job", "", "", false)
	if err != nil {
		t.Fatalf("preempted should be resumable: %v", err)
	}
	if plan.Reason != resume.ReasonPreempted {
		t.Fatalf("expected Preempted reason, got %v", plan.Reason)
	}
}

func TestResumePreflightEvicted(t *testing.T) {
	snap := failedJobSnapshot("evicted-job")
	snap.Events = []status.Event{
		{Reason: "Evicted"},
	}
	plan, err := resumePreflight(snap, "evicted-job", "", "", false)
	if err != nil {
		t.Fatalf("evicted should be resumable: %v", err)
	}
	if plan.Reason != resume.ReasonEvicted {
		t.Fatalf("expected Evicted reason, got %v", plan.Reason)
	}
}

func TestResumePreflightDefaultCheckpointPath(t *testing.T) {
	snap := failedJobSnapshot("lora-7b")
	snap.Events = []status.Event{{Reason: "Evicted"}}
	plan, err := resumePreflight(snap, "lora-7b", "", "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := storage.DurableFinetuneDir("lora-7b")
	if plan.CheckpointDir != want {
		t.Fatalf("checkpoint dir = %q, want %q", plan.CheckpointDir, want)
	}
}

func TestResumePreflightCustomCheckpointPath(t *testing.T) {
	snap := failedJobSnapshot("lora-7b")
	snap.Events = []status.Event{{Reason: "Evicted"}}
	plan, err := resumePreflight(snap, "lora-7b", "", "/data/custom/ckpt", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.CheckpointDir != "/data/custom/ckpt" {
		t.Fatalf("checkpoint dir = %q, want /data/custom/ckpt", plan.CheckpointDir)
	}
}

func TestResumePreflightConfigHashMatch(t *testing.T) {
	hash := hashString("config-content")
	snap := failedJobSnapshot("hash-job")
	snap.Events = []status.Event{{Reason: "Evicted"}}
	snap.Annotations = map[string]string{
		experiment.AnnotationConfigHash: hash,
	}
	plan, err := resumePreflight(snap, "hash-job", hash, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.ConfigWarning != "" {
		t.Fatalf("expected no warning for matching hash, got %q", plan.ConfigWarning)
	}
}

func TestResumePreflightConfigHashMismatch(t *testing.T) {
	oldHash := hashString("old-content")
	newHash := hashString("new-content")
	snap := failedJobSnapshot("hash-job")
	snap.Events = []status.Event{{Reason: "Evicted"}}
	snap.Annotations = map[string]string{
		experiment.AnnotationConfigHash: oldHash,
	}
	plan, err := resumePreflight(snap, "hash-job", newHash, "", false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plan.ConfigWarning == "" {
		t.Fatal("expected config hash mismatch warning")
	}
	if !strings.Contains(plan.ConfigWarning, "config hash changed") {
		t.Fatalf("unexpected warning: %s", plan.ConfigWarning)
	}
}

func TestResumePreflightRayJob(t *testing.T) {
	snap := status.Snapshot{
		Name:      "ray-train",
		Namespace: "ray",
		RayJob: status.RayJob{
			Found:               true,
			JobDeploymentStatus: "Failed",
		},
		Pods: []status.Pod{
			{
				Phase: "Failed",
				Conditions: []status.Condition{
					{Type: "DisruptionTarget", Status: "True"},
				},
			},
		},
	}
	plan, err := resumePreflight(snap, "ray-train", "", "", false)
	if err != nil {
		t.Fatalf("RayJob preempted should be resumable: %v", err)
	}
	if plan.Reason != resume.ReasonPreempted {
		t.Fatalf("expected Preempted, got %v", plan.Reason)
	}
}

func TestResumePreflightUnknownFailure(t *testing.T) {
	snap := failedJobSnapshot("unknown-fail")
	_, err := resumePreflight(snap, "unknown-fail", "", "", false)
	if err == nil {
		t.Fatal("expected unknown failure to be rejected by default")
	}
	if got := err.Error(); !strings.Contains(got, "non-retryable reason Unknown") {
		t.Fatalf("unexpected error: %s", got)
	}
}

func TestDeleteWorkloadSkipsMissingRayJobCRDAndDeletesJob(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{
			fakeRawKey("-n", "ray", "delete", "job", "train", "--ignore-not-found"): "job.batch \"train\" deleted\n",
		},
		errors: map[string]error{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train", "--ignore-not-found"): errors.New("the server doesn't have a resource type \"rayjob\""),
		},
	}
	var out strings.Builder
	if err := deleteWorkload(context.Background(), runner, "train", "ray", &out); err != nil {
		t.Fatalf("deleteWorkload should ignore missing RayJob CRD and delete Job: %v", err)
	}
	if len(runner.calls) != 3 {
		t.Fatalf("expected RayJob, Job, and headless Service deletes, got calls=%v", runner.calls)
	}
	if !strings.Contains(out.String(), "job.batch") {
		t.Fatalf("expected job delete output, got %q", out.String())
	}
}

func TestDeleteWorkload_IgnoresExactObjectNotFound(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{},
		errors: map[string]error{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train", "--ignore-not-found"): errors.New("Error from server (NotFound): rayjobs.ray.io \"train\" not found"),
			fakeRawKey("-n", "ray", "delete", "job", "train", "--ignore-not-found"):           errors.New("Error from server (NotFound): jobs.batch \"train\" not found"),
		},
	}
	if err := deleteWorkload(context.Background(), runner, "train", "ray", io.Discard); err != nil {
		t.Fatalf("deleteWorkload should ignore exact object not found: %v", err)
	}
}

func TestDeleteWorkload_DoesNotSwallowNamespaceNotFound(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{},
		errors: map[string]error{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train", "--ignore-not-found"): errors.New("Error from server (NotFound): namespaces \"ray\" not found"),
			fakeRawKey("-n", "ray", "delete", "job", "train", "--ignore-not-found"):           errors.New("Error from server (NotFound): namespaces \"ray\" not found"),
		},
	}
	err := deleteWorkload(context.Background(), runner, "train", "ray", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "namespaces \"ray\" not found") {
		t.Fatalf("expected namespace not found to propagate, got %v", err)
	}
}

func TestDeleteWorkload_DoesNotSwallowWrongObjectNotFound(t *testing.T) {
	runner := &fakeRawRunner{
		outputs: map[string]string{},
		errors: map[string]error{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "train", "--ignore-not-found"): errors.New("Error from server (NotFound): rayjobs.ray.io \"other\" not found"),
			fakeRawKey("-n", "ray", "delete", "job", "train", "--ignore-not-found"):           errors.New("Error from server (NotFound): jobs.batch \"other\" not found"),
		},
	}
	err := deleteWorkload(context.Background(), runner, "train", "ray", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "\"other\" not found") {
		t.Fatalf("expected wrong-object not found to propagate, got %v", err)
	}
}

func TestResumeCleanupWaitsForMultiKueueManagerFinalizer(t *testing.T) {
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "resume-job", "--ignore-not-found"): {{out: "rayjob.ray.io \"resume-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "resume-job", "--ignore-not-found"):           {{out: "job.batch \"resume-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"): {
				{out: `{"metadata":{"name":"wl-a"}}`},
				{err: errors.New("Error from server (NotFound): workloads.kueue.x-k8s.io \"wl-a\" not found")},
			},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=resume-job", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	fetchCalls := 0
	waitCalls := 0
	err := deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "resume-job", "ray", io.Discard, managerCleanupOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, managerCleanupHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			fetchCalls++
			if fetchCalls == 1 {
				return multiKueueResumeCleanupSnapshot("resume-job", "wl-a"), nil
			}
			return status.Snapshot{Name: "resume-job", Namespace: "ray"}, nil
		},
		wait: func(context.Context, time.Duration) error { waitCalls++; return nil },
		now:  time.Now,
	})
	if err != nil {
		t.Fatalf("expected resume cleanup to succeed, got %v", err)
	}
	if waitCalls != 2 {
		t.Fatalf("expected one exact-name wait plus one selector absence proof wait, got %d", waitCalls)
	}
	for _, call := range runner.joinedCalls() {
		if strings.Contains(call, "worker-a") || strings.Contains(call, "pods") || strings.Contains(call, "events") {
			t.Fatalf("resume cleanup should stay manager-side, got call %q", call)
		}
	}
}

func TestResumeCleanupTimeoutUsesCachedSnapshot(t *testing.T) {
	base := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	nowValues := []time.Time{
		base,
		base,
		base,
		base.Add(31 * time.Second),
		base.Add(31 * time.Second),
	}
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "resume-job", "--ignore-not-found"): {{out: "rayjob.ray.io \"resume-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "resume-job", "--ignore-not-found"):           {{out: "job.batch \"resume-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "wl-a", "-o", "json"):       {{out: `{"metadata":{"name":"wl-a"}}`}},
		},
	}
	err := deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "resume-job", "ray", io.Discard, managerCleanupOptions{
		Timeout:  30 * time.Second,
		Interval: time.Second,
	}, managerCleanupHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			snap := multiKueueResumeCleanupSnapshot("resume-job", "wl-a")
			snap.Workloads[0].Admitted = true
			snap.Workloads[0].Phase = "Admitted"
			return snap, nil
		},
		wait: func(context.Context, time.Duration) error { return nil },
		now: func() time.Time {
			if len(nowValues) == 0 {
				return base.Add(time.Hour)
			}
			cur := nowValues[0]
			nowValues = nowValues[1:]
			return cur
		},
	})
	if err == nil || !strings.Contains(err.Error(), "timed out after 30s") {
		t.Fatalf("expected cleanup timeout, got %v", err)
	}
	if !strings.Contains(err.Error(), "selected-worker=worker-a") {
		t.Fatalf("expected cached snapshot detail, got %v", err)
	}
}

func TestResumeCleanup_InconclusivePrefetchStillUsesRediscovery(t *testing.T) {
	base := time.Date(2026, 7, 10, 15, 0, 0, 0, time.UTC)
	current := base
	runner := &scriptedRawRunner{
		steps: map[string][]scriptedRawResponse{
			fakeRawKey("-n", "ray", "delete", "rayjob.ray.io", "resume-job", "--ignore-not-found"): {{out: "rayjob.ray.io \"resume-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "delete", "job", "resume-job", "--ignore-not-found"):           {{out: "job.batch \"resume-job\" deleted\n"}},
			fakeRawKey("-n", "ray", "get", "workloads.kueue.x-k8s.io", "-l", workloadmeta.LabelJob+"=resume-job", "-o", "json"): {
				{out: `{"items":[]}`},
				{out: `{"items":[]}`},
			},
		},
	}
	waitCalls := 0
	err := deleteWorkloadAndWaitForManagerCleanup(context.Background(), runner, "resume-job", "ray", io.Discard, managerCleanupOptions{
		Timeout:  2 * time.Second,
		Interval: time.Second,
	}, managerCleanupHooks{
		fetchSnapshot: func(context.Context) (status.Snapshot, error) {
			return status.Snapshot{Name: "resume-job", Namespace: "ray"}, context.DeadlineExceeded
		},
		wait: func(_ context.Context, d time.Duration) error {
			waitCalls++
			current = current.Add(d)
			return nil
		},
		now: func() time.Time { return current },
	})
	if err == nil {
		t.Fatal("expected cleanup timeout once inconclusive resume rechecks exhaust the deadline")
	}
	for _, want := range []string{
		"timed out after 2s waiting to prove manager workload cleanup",
		"selectors=" + workloadmeta.LabelJob + "=resume-job",
		"snapshot-error=context deadline exceeded",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected error to contain %q, got %q", want, err.Error())
		}
	}
	if waitCalls != 2 {
		t.Fatalf("expected inconclusive resume rechecks to keep waiting until the deadline, got %d waits", waitCalls)
	}
}

func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
