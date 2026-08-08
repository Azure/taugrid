// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package expcockpit

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Azure/taugrid/portal/internal/portalbin"
)

// Stellar moved out of the tau CLI, so every command this package hands a user
// to copy has to name taugrid-portal. A stale "tau experiment ..." still
// compiles and still renders — it only fails when someone pastes it — so these
// are the assertions that turn that into a build-time failure.

func TestActionCommandsNameThePortalBinary(t *testing.T) {
	actions := buildActions("/data/exp", "vision-r3", "experiment", nil, "eval/accuracy")

	fields := map[string]string{
		"CopyCLI":      actions.CopyCLI,
		"OpenCLI":      actions.OpenCLI,
		"ExportPacket": actions.ExportPacket,
		"ObserveCLI":   actions.ObserveCLI,
		"NextCommand":  actions.NextCommand,
	}
	for name, got := range fields {
		if got == "" {
			t.Fatalf("%s is empty; the assertion below would pass vacuously", name)
		}
		if !strings.HasPrefix(got, portalbin.ExperimentCmd+" ") {
			t.Fatalf("%s = %q, want a command starting with %q", name, got, portalbin.ExperimentCmd)
		}
		if strings.HasPrefix(got, "tau ") {
			t.Fatalf("%s = %q still names the tau binary, which no longer has the experiment verb", name, got)
		}
	}

	// NextCommand takes a different branch when no metric is selected.
	noMetric := buildActions("/data/exp", "vision-r3", "experiment", nil, "")
	if !strings.HasPrefix(noMetric.NextCommand, portalbin.ExperimentCmd+" ") {
		t.Fatalf("NextCommand without a metric = %q, want prefix %q", noMetric.NextCommand, portalbin.ExperimentCmd)
	}
	if noMetric.NextCommand == actions.NextCommand {
		t.Fatalf("expected the metric and no-metric branches of defaultNextCommand to differ; both = %q", noMetric.NextCommand)
	}
}

func TestRunObserveCommandNamesThePortalBinary(t *testing.T) {
	runs := attachRunDetails("/data/exp", []RunView{{RunID: "seed-1"}}, nil, nil, nil, nil, nil)
	if len(runs) != 1 {
		t.Fatalf("attachRunDetails returned %d runs, want 1", len(runs))
	}
	got := runs[0].ObserveCLI
	if !strings.HasPrefix(got, portalbin.ExperimentCmd+" ") {
		t.Fatalf("run ObserveCLI = %q, want prefix %q", got, portalbin.ExperimentCmd)
	}
	if !strings.Contains(got, "--scope run:'seed-1'") {
		t.Fatalf("run ObserveCLI = %q, want it scoped to the run", got)
	}
}

// TestNoGoSourceGeneratesTauExperimentCommands is the fail-closed half. The
// assertions above cover the fields that exist today; this one covers the ones
// someone adds tomorrow. It reads string literals from the AST rather than
// grepping, so the comments that legitimately describe the pre-split commands
// do not trip it.
func TestNoGoSourceGeneratesTauExperimentCommands(t *testing.T) {
	forbidden := []string{"tau experiment", "tau exp ", "tau portal "}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" || d.Name() == "assets" {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		checked++
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, bad := range forbidden {
				if strings.Contains(value, bad) {
					t.Errorf("%s:%d: string literal contains %q; Stellar and Portal verbs live in %s, so emit %q instead:\n\t%s",
						rel, fset.Position(lit.Pos()).Line, bad, portalbin.Name, portalbin.ExperimentCmd, value)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if checked < 50 {
		t.Fatalf("walked only %d Go files; the guard is not reaching the module", checked)
	}
}
