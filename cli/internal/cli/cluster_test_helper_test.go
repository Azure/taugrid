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

// releaseFixture is the state the read-only Helm verbs report for a release
// that is already installed. Zero values mean "that read fails", which is how
// tests exercise the degradation paths.
type releaseFixture struct {
	Release  string
	Values   string
	Metadata string
}

// installFakeHelmWithRelease answers the read-only Helm verbs from a fixture so
// tests can drive the drain path, and passes every mutating verb to fake. The
// split keeps arg assertions on `upgrade`/`uninstall` free of preflight noise.
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
