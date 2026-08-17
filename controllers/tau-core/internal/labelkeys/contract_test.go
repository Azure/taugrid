// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package labelkeys

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The Tau CLI and this controller are separate Go modules with no dependency
// in either direction. The CLI is published as an OSS artifact, while the
// controller contract is internal to this module. Their shared surface remains
// intentionally small, so a source-parsing contract test is cheaper and safer
// than a third module or a controller dependency in the CLI.
const cliMetadataPath = "../../../../core/workloadmeta/metadata.go"

var tauTokenRe = regexp.MustCompile(regexp.QuoteMeta(Domain) + `[A-Za-z0-9._-]*`)

// sharedKeys contains only keys that cross the module boundary. Controller-only
// namespace and quota keys do not belong in the CLI's canonical package.
var sharedKeys = map[string]string{
	"LabelWorkspace":             "LabelWorkspace",
	"LabelManagedBy":             "LabelManagedBy",
	"AnnotationResultScope":      "AnnotationResultScope",
	"AnnotationResultPVC":        "AnnotationResultPVC",
	"AnnotationArtifactBundleID": "AnnotationArtifactBundleID",
	"AnnotationArtifactStore":    "AnnotationArtifactStore",
}

func TestSharedLabelKeysAgreeWithTauCLI(t *testing.T) {
	ours := parseStringConsts(t, "labelkeys.go")
	theirs := parseStringConsts(t, cliMetadataPath)

	var problems []string
	for ourName, theirName := range sharedKeys {
		ourVal, ok := ours[ourName]
		if !ok {
			problems = append(problems, "controller no longer declares "+ourName)
			continue
		}
		theirVal, ok := theirs[theirName]
		if !ok {
			problems = append(problems, "Tau CLI no longer declares "+theirName)
			continue
		}
		if ourVal != theirVal {
			problems = append(problems, "key drift: controller "+ourName+"="+strconv.Quote(ourVal)+
				" but CLI "+theirName+"="+strconv.Quote(theirVal))
		}
	}
	sort.Strings(problems)
	if len(problems) > 0 {
		t.Fatalf("controller and Tau CLI disagree on shared label keys:\n  %s\n\n"+
			"These modules cannot import each other, so this test turns a one-sided\n"+
			"rename into a failure instead of a silent runtime mismatch.",
			strings.Join(problems, "\n  "))
	}
}

// TestNoInlineTauKeyLiterals guards every Go package in this module, including
// tests and fixtures. That matters because a test asserting the same stale
// literal would otherwise stay green.
func TestNoInlineTauKeyLiterals(t *testing.T) {
	root := moduleRoot(t)
	var offenders []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == filepath.FromSlash("internal/labelkeys/labelkeys.go") ||
			rel == filepath.FromSlash("internal/labelkeys/contract_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err == nil && strings.Contains(val, Domain) {
				offenders = append(offenders, rel+":"+
					strconv.Itoa(fset.Position(lit.Pos()).Line)+": "+strconv.Quote(val))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan module: %v", err)
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("tau.azure.com keys spelled outside internal/labelkeys:\n  %s\n\n"+
			"Reference labelkeys constants so a rename is a compile error rather\n"+
			"than a partial edit.", strings.Join(offenders, "\n  "))
	}
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get cwd: %v", err)
	}
	return filepath.Clean(filepath.Join(wd, "../.."))
}

func parseStringConsts(t *testing.T, path string) map[string]string {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	out := map[string]string{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != len(vs.Values) {
				continue
			}
			for i, name := range vs.Names {
				lit, ok := vs.Values[i].(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				if val, err := strconv.Unquote(lit.Value); err == nil {
					out[name.Name] = val
				}
			}
		}
	}
	return out
}
