// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
)

func runCluster(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newClusterCmd()
	var out, stderr bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return strings.TrimSpace(out.String() + stderr.String()), err
}

func runClusterInstallExternal(t *testing.T, args ...string) (string, error) {
	t.Helper()
	return runCluster(t, append([]string{"install", "--kueue-mode", kueueModeExternal}, args...)...)
}

type releaseFixture struct {
	Release  string
	Values   string
	Metadata string
}

func installFakeHelmWithRelease(t *testing.T, fixture releaseFixture, fake helmCommandRunner) {
	t.Helper()
	original := runHelmCommand
	runHelmCommand = func(ctx context.Context, in io.Reader, out, errOut io.Writer, args []string) error {
		switch {
		case len(args) > 0 && args[0] == "list":
			_, _ = io.WriteString(out, `[{"name":"`+fixture.Release+`"}]`)
			return nil
		case len(args) > 1 && args[0] == "get" && args[1] == "values":
			if fixture.Values == "" {
				return errUnreadableRelease
			}
			_, _ = io.WriteString(out, fixture.Values)
			return nil
		case len(args) > 1 && args[0] == "get" && args[1] == "metadata":
			if fixture.Metadata == "" {
				return errUnreadableRelease
			}
			_, _ = io.WriteString(out, fixture.Metadata)
			return nil
		}
		return fake(ctx, in, out, errOut, args)
	}
	t.Cleanup(func() { runHelmCommand = original })
}
