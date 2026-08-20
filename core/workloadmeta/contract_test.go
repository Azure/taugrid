// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workloadmeta_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/workloadmeta"
)

// This is the durability half of the label-contract work. Migrating every
// literal to a constant once is a cleanup; this test is what stops the literals
// coming back.
//
// Why a test and not a linter: it runs under the same `go test ./... -count=1`
// everyone already runs and CI already gates on, needs no extra tool in the
// image, and can be written against go/parser so it sees exactly the string
// literals the compiler sees. A grep-based lint rule would trip over every doc
// comment that mentions a key -- and this package's doc comments necessarily
// quote several.
//
// Why go/parser and not regex: only *ast.BasicLit nodes are considered, so
// comments and identifiers are structurally excluded rather than filtered out by
// heuristics.

const (
	// canonicalFile is the one file allowed to spell a key.
	canonicalFile = "metadata.go"
	// canonicalPkgDir is the directory that owns the contract, relative to
	// guardRoot.
	canonicalPkgDir = "core/workloadmeta"
)

// tauProductDirs are the trees this guard walks, relative to guardRoot. The
// contract package lives in core/, but its keys are also used by cli/,
// examples/, and portal/. Scoping the walk to core/ alone would silently stop
// guarding those consumers. Sibling top-level modules (controllers/,
// sdk/python/) own their own contract tests and are deliberately excluded.
var tauProductDirs = []string{"cli", "core", "examples", "portal"}

// allowedNonKeyLiterals are strings that begin with the Tau domain but are not
// label or annotation keys. Each needs a reason.
var allowedNonKeyLiterals = map[string]string{
	// apiVersion for Tau custom resources, not a key.
	workloadmeta.APIGroup + "/v1alpha1": "TauWorkspace/TauQuotaRequest apiVersion",
	// A deliberately unknown key: cleanup must handle every label under the Tau
	// domain, including keys added after this build shipped. The test
	// fixture asserts that forward compatibility, so it cannot use a constant.
	workloadmeta.Domain + "some-future-key": "fixture for forward-compatible offboard stripping",
}

func TestNoInlineTauKeyLiterals(t *testing.T) {
	root := guardRoot(t)
	known := knownKeys(t, root)

	type finding struct {
		file string
		line int
		lit  string
		hint string
	}
	var findings []finding

	walkGoFiles(t, root, func(path string, rel string) {
		if filepath.Dir(rel) == canonicalPkgDir && filepath.Base(rel) == canonicalFile {
			return
		}
		fset := token.NewFileSet()
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			val, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			for _, tok := range tauTokens(val) {
				if _, ok := allowedNonKeyLiterals[tok]; ok {
					continue
				}
				pos := fset.Position(lit.Pos())
				findings = append(findings, finding{
					file: rel,
					line: pos.Line,
					lit:  tok,
					hint: suggest(tok, known),
				})
			}
			return true
		})
	})

	if len(findings) == 0 {
		return
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].file != findings[j].file {
			return findings[i].file < findings[j].file
		}
		return findings[i].line < findings[j].line
	})
	var b strings.Builder
	b.WriteString("inline \"" + workloadmeta.Domain + "\" keys found outside " +
		canonicalPkgDir + "/" + canonicalFile + ".\n" +
		"Every Tau label and annotation key must be declared exactly once, in workloadmeta.\n" +
		"Renaming a key that is spelled in several places is not a compile error, which is\n" +
		"how stale keys have silently shipped before.\n\n")
	for _, f := range findings {
		b.WriteString("  " + f.file + ":" + strconv.Itoa(f.line) + ": " + strconv.Quote(f.lit))
		if f.hint != "" {
			b.WriteString("\n      use " + f.hint)
		}
		b.WriteString("\n")
	}
	t.Fatal(b.String())
}

// TestTauDocsAndYAMLUseDeclaredKeys covers the surface where Go constants
// cannot be imported: Markdown and YAML that name Tau keys directly. Every Tau
// key spelled in an executable example or manifest must exist in the canonical
// inventory, otherwise the documented selector matches nothing.
func TestTauDocsAndYAMLUseDeclaredKeys(t *testing.T) {
	root := guardRoot(t)
	known := knownKeys(t, root)
	var findings []string

	walkTauTrees(t, root, func(path, rel string) {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".md", ".yaml", ".yml":
		default:
			return
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for lineNo, line := range strings.Split(string(src), "\n") {
			for _, tok := range tauTokens(line) {
				if tok == workloadmeta.APIGroup+"/v1alpha1" {
					continue
				}
				if _, ok := known[tok]; !ok {
					findings = append(findings, rel+":"+strconv.Itoa(lineNo+1)+
						": undeclared Tau key "+strconv.Quote(tok))
				}
			}
		}
	})
	sort.Strings(findings)
	if len(findings) > 0 {
		t.Fatalf("Tau docs/YAML drifted from workloadmeta:\n  %s\n\n"+
			"Use a key declared in "+canonicalPkgDir+".",
			strings.Join(findings, "\n  "))
	}
}

// TestEveryDeclaredKeyIsUsed keeps the contract honest in the other direction:
// a constant nobody references is a key nobody applies, which is exactly the
// orphaned-declaration state this work exists to remove.
func TestEveryDeclaredKeyIsUsed(t *testing.T) {
	root := guardRoot(t)
	names := declaredNames(t, root)

	used := map[string]bool{}
	walkGoFiles(t, root, func(path string, rel string) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		if err != nil {
			return
		}
		// Inside the canonical package the constants are referenced bare; outside
		// it they are qualified. Both count: a prefix helper such as
		// StellarAnnotationPrefix is legitimately consumed only by the package's
		// own PodCorrelationAnnotations, and demanding an external caller would
		// push a helper out of the package that owns it.
		local := filepath.Dir(rel) == canonicalPkgDir
		declared := map[*ast.Ident]bool{}
		if local {
			for _, decl := range f.Decls {
				gen, ok := decl.(*ast.GenDecl)
				if !ok || gen.Tok != token.CONST {
					continue
				}
				for _, spec := range gen.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for _, name := range vs.Names {
						declared[name] = true
					}
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if local {
				if ident, ok := n.(*ast.Ident); ok && !declared[ident] {
					used[ident.Name] = true
				}
				return true
			}
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok || ident.Name != "workloadmeta" {
				return true
			}
			used[sel.Sel.Name] = true
			return true
		})
	})
	// A few contract keys are consumed only by checked-in manifests/examples.
	// Count those references so the orphan check does not force runtime Go code
	// to import a key solely to keep its declaration alive.
	known := knownKeys(t, root)
	walkTauTrees(t, root, func(path, rel string) {
		switch strings.ToLower(filepath.Ext(path)) {
		case ".md", ".yaml", ".yml":
		default:
			return
		}
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		for _, tok := range tauTokens(string(src)) {
			if name, ok := known[tok]; ok {
				used[name] = true
			}
		}
	})

	var unused []string
	for _, name := range names {
		if !used[name] {
			unused = append(unused, name)
		}
	}
	sort.Strings(unused)
	if len(unused) > 0 {
		t.Fatalf("workloadmeta declares keys nothing references: %s\n"+
			"Either wire the constant up at its call site or delete it -- an unreferenced\n"+
			"key constant is indistinguishable from a key Tau no longer applies.",
			strings.Join(unused, ", "))
	}
}

// TestNoDuplicateKeyStrings asserts one declaration per key string, which is the
// property that makes a rename a compile error rather than a partial edit.
//
// Two exceptions are declared explicitly rather than allowed by pattern:
// gpu-class and managed-by each have a workload-facing and a node-facing name
// because they are genuinely read from both sides of the contract.
func TestNoDuplicateKeyStrings(t *testing.T) {
	allowedDuplicates := map[string][]string{
		workloadmeta.Domain + "gpu-class": {"LabelGPUClass", "NodeLabelGPUClass"},
		workloadmeta.Domain + "gpu-count": {"LabelGPUCount", "AnnotationGPUCount"},
	}

	root := guardRoot(t)
	byKey := map[string][]string{}
	for name, key := range declaredKeys(t, root) {
		byKey[key] = append(byKey[key], name)
	}
	for key, names := range byKey {
		if len(names) < 2 {
			continue
		}
		sort.Strings(names)
		want, ok := allowedDuplicates[key]
		if !ok {
			t.Errorf("key %q is declared under %d names (%s); collapse to one",
				key, len(names), strings.Join(names, ", "))
			continue
		}
		sort.Strings(want)
		if strings.Join(names, ",") != strings.Join(want, ",") {
			t.Errorf("key %q declared as [%s], expected exactly [%s]",
				key, strings.Join(names, ", "), strings.Join(want, ", "))
		}
	}
}

// suggest maps a literal back to the constant that should replace it. It matches
// keys exactly and never by prefix: "tau.azure.com/profile" is the resolved
// compute profile, while "tau.azure.com/profile-mode" and friends are Nsight
// profiler settings. Prefix matching would confidently point a reader at the
// wrong contract.
func suggest(tok string, known map[string]string) string {
	if name, ok := known[tok]; ok {
		return "workloadmeta." + name
	}
	if tok == workloadmeta.Domain {
		return "workloadmeta.Domain"
	}
	// Longest match, never first match. "tau.azure.com/run" is a prefix of
	// "tau.azure.com/run-id" and "tau.azure.com/profile" is a prefix of the
	// seven "tau.azure.com/profile-*" profiler annotations; a first-match loop
	// would point the reader at the wrong contract with full confidence.
	best, bestName := "", ""
	for key, name := range known {
		if strings.HasPrefix(tok, key) && len(key) > len(best) {
			best, bestName = key, name
		}
	}
	if bestName != "" {
		return "workloadmeta." + bestName + " + " + strconv.Quote(strings.TrimPrefix(tok, best))
	}
	return "no matching constant -- if this is a real key, declare it in workloadmeta; " +
		"if it is not, add it to allowedNonKeyLiterals with a reason"
}

// tauTokenRe finds Tau-domain keys wherever they appear, including embedded in a
// larger YAML fixture, JSON blob, kubectl selector, or help string. Scanning
// tokens rather than whole literals is what lets a multi-document YAML fixture
// carry an apiVersion without being flagged, while a stale key inside the same
// blob still is.
var tauTokenRe = regexp.MustCompile(regexp.QuoteMeta(workloadmeta.Domain) + `[A-Za-z0-9._-]*`)

// tauIdentByte reports whether c can appear inside a DNS-style identifier, and
// so whether a Tau-domain match that starts right after it is really the tail of
// a longer name rather than a key in its own right.
func tauIdentByte(c byte) bool {
	return c == '.' || c == '-' || c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func tauTokens(s string) []string {
	spans := tauTokenRe.FindAllStringIndex(s, -1)
	if spans == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, span := range spans {
		// Require a left boundary. kubectl's <resource>.<group>/<name> form puts
		// the domain in the middle of a token, so "workspace.tau.azure.com/foo"
		// would otherwise be read as the label key "tau.azure.com/foo" and
		// flagged as undeclared. That is correct kubectl, and docs must be able
		// to show it.
		if span[0] > 0 && tauIdentByte(s[span[0]-1]) {
			continue
		}
		m := s[span[0]:span[1]]
		if seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

var keyDeclRe = regexp.MustCompile(`(?m)^\t([A-Za-z][A-Za-z0-9]*)\s+= "(tau\.azure\.com/[^"]*)"$`)

func declaredKeys(t *testing.T, root string) map[string]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(root, canonicalPkgDir, canonicalFile))
	if err != nil {
		t.Fatalf("read canonical file: %v", err)
	}
	out := map[string]string{}
	for _, m := range keyDeclRe.FindAllStringSubmatch(string(src), -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatal("parsed zero key declarations from the canonical file; the declaration " +
			"format changed and this guard is no longer guarding anything")
	}
	return out
}

func declaredNames(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	for name := range declaredKeys(t, root) {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// knownKeys inverts declaredKeys. Where a key has several names (gpu-class), the
// first alphabetically wins for suggestion purposes only.
func knownKeys(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for name, key := range declaredKeys(t, root) {
		if existing, ok := out[key]; !ok || name < existing {
			out[key] = name
		}
	}
	return out
}

func walkGoFiles(t *testing.T, root string, fn func(path, rel string)) {
	t.Helper()
	walkTauTrees(t, root, func(path, rel string) {
		if !strings.HasSuffix(path, ".go") {
			return
		}
		fn(path, rel)
	})
}

// walkTauTrees visits every file under the Tau product trees, reporting paths
// relative to root so callers keep comparing rel against canonicalPkgDir.
func walkTauTrees(t *testing.T, root string, fn func(path, rel string)) {
	t.Helper()
	for _, dir := range tauProductDirs {
		base := filepath.Join(root, dir)
		if _, err := os.Stat(base); os.IsNotExist(err) {
			continue
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case ".git", "vendor", "node_modules", "testdata":
					return filepath.SkipDir
				}
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			fn(path, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
}

// guardRoot is the repo root, the common parent of the Tau product modules.
func guardRoot(t *testing.T) string {
	t.Helper()
	return filepath.Dir(moduleRoot(t))
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate module root")
		}
		dir = parent
	}
}

// TestTauTokensRequiresLeftBoundary pins the scanner's boundary rule. Docs
// legitimately show kubectl's <resource>.<group>/<name> form, where the Tau
// domain sits in the middle of a token and the trailing segment is an object
// name, not a label key. Without a left boundary the scanner reads those as
// undeclared keys and the guard fails on correct documentation.
func TestTauTokensRequiresLeftBoundary(t *testing.T) {
	key := workloadmeta.LabelRunID
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{{
		name: "bare key",
		in:   key + ": abc",
		want: []string{key},
	}, {
		name: "quoted key",
		in:   `"` + key + `": "abc"`,
		want: []string{key},
	}, {
		name: "label selector",
		in:   "kubectl get pods -l " + key + "=abc",
		want: []string{key},
	}, {
		name: "kubectl resource reference is a name, not a key",
		in:   "kubectl delete workspace." + workloadmeta.Domain + "taugrid-default -n tau-system",
		want: nil,
	}, {
		name: "resource reference does not mask a real key on the same line",
		in:   "kubectl label workspace." + workloadmeta.Domain + "x " + key + "=abc",
		want: []string{key},
	}} {
		t.Run(tc.name, func(t *testing.T) {
			got := tauTokens(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("tauTokens(%q) = %v, want %v", tc.in, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("tauTokens(%q) = %v, want %v", tc.in, got, tc.want)
				}
			}
		})
	}
}
