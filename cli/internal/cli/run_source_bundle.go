package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/taugrid/cli/internal/sourcebundle"
)

// stageSourceBundle is replaceable in tests so submission wiring can be
// exercised without creating a helper pod.
var stageSourceBundle = sourcebundle.Stage

type preparedSourceBundle struct {
	bundle  sourcebundle.Bundle
	runtime sourcebundle.Runtime
}

func (o runDispatchOptions) hasSourceBundle() bool {
	return strings.TrimSpace(o.sourceBundlePath) != ""
}

func buildRunSourceBundle(o runDispatchOptions) (*preparedSourceBundle, error) {
	if !o.hasSourceBundle() {
		return nil, nil
	}
	if strings.TrimSpace(o.script) == "" {
		return nil, fmt.Errorf("run.source_bundle requires run.entrypoint")
	}

	root, err := resolvedSourcePath(o.sourceBundlePath)
	if err != nil {
		return nil, fmt.Errorf("run.source_bundle.path: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("run.source_bundle.path: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("run.source_bundle.path %q must be a directory", o.sourceBundlePath)
	}

	entrypoint, err := resolvedSourcePath(o.script)
	if err != nil {
		return nil, fmt.Errorf("run.entrypoint: %w", err)
	}
	entryInfo, err := os.Stat(entrypoint)
	if err != nil {
		return nil, fmt.Errorf("run.entrypoint: %w", err)
	}
	if entryInfo.IsDir() {
		return nil, fmt.Errorf("run.entrypoint %q must be a file inside run.source_bundle.path", o.script)
	}
	relativeEntrypoint, err := filepath.Rel(root, entrypoint)
	if err != nil || relativeEntrypoint == ".." || strings.HasPrefix(relativeEntrypoint, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("run.entrypoint %s must live inside run.source_bundle.path %s", o.script, o.sourceBundlePath)
	}
	relativeEntrypoint = filepath.ToSlash(relativeEntrypoint)
	if err := sourcebundle.ValidateEntrypointRelative(relativeEntrypoint); err != nil {
		return nil, fmt.Errorf("run.entrypoint %q: %w", o.script, err)
	}

	bundle, err := sourcebundle.Build(sourcebundle.BuildOptions{
		Dir:            root,
		Excludes:       o.sourceBundleExcludes,
		ExpectedDigest: o.sourceBundleDigest,
	})
	if err != nil {
		return nil, fmt.Errorf("run.source_bundle: %w", err)
	}
	runtime, err := bundle.RuntimeFor(relativeEntrypoint)
	if err != nil {
		return nil, fmt.Errorf("run.source_bundle: %w", err)
	}
	return &preparedSourceBundle{bundle: bundle, runtime: runtime}, nil
}

func resolvedSourcePath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func stageRunSourceBundle(ctx context.Context, dryRun string, runner sourcebundle.Runner, namespace, pvc, runName string, source *preparedSourceBundle) error {
	if source == nil || dryRun != "" {
		return nil
	}
	if strings.TrimSpace(pvc) == "" {
		return fmt.Errorf("source bundle requires a durable PVC")
	}
	if err := stageSourceBundle(ctx, runner, namespace, pvc, runName, source.bundle); err != nil {
		return err
	}
	return nil
}
