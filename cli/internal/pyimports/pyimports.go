// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package pyimports

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type Kind string

const (
	KindModule   Kind = "module"
	KindPackage  Kind = "package"
	KindRelative Kind = "relative"
)

type Finding struct {
	Module string
	Path   string
	Line   int
	Kind   Kind
}

var (
	importRE     = regexp.MustCompile(`^\s*import\s+(.+)$`)
	fromImportRE = regexp.MustCompile(`^\s*from\s+(\.*)([A-Za-z0-9_.]*)\s+import\s`)
	identifierRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func Check(path string, shipped []string, extraRoots ...string) ([]Finding, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("reading entrypoint %s: %w", path, err)
	}
	defer file.Close()

	shippedSet := make(map[string]bool, len(shipped))
	for _, name := range shipped {
		shippedSet[strings.TrimSuffix(filepath.Base(name), ".py")] = true
	}
	roots := []string{filepath.Dir(path)}
	for _, root := range extraRoots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		if abs, err := filepath.Abs(root); err == nil {
			root = abs
		}
		if root != roots[0] {
			roots = append(roots, root)
		}
	}

	var findings []Finding
	seen := map[string]bool{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	lineNo := 0
	inDocstring := ""
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()

		if inDocstring != "" {
			if strings.Contains(line, inDocstring) {
				inDocstring = ""
			}
			continue
		}
		if delim := docstringOpener(line); delim != "" {
			inDocstring = delim
			continue
		}
		if idx := strings.Index(line, "#"); idx == 0 || (idx > 0 && strings.TrimSpace(line[:idx]) == "") {
			continue
		}

		for _, mod := range importedModules(line) {
			if seen[mod] {
				continue
			}
			f, ok := classify(roots, mod, shippedSet)
			if !ok {
				continue
			}
			seen[mod] = true
			f.Line = lineNo
			findings = append(findings, f)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading entrypoint %s: %w", path, err)
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].Line < findings[j].Line })
	return findings, nil
}

func importedModules(line string) []string {
	if m := fromImportRE.FindStringSubmatch(line); m != nil {
		dots, module := m[1], m[2]
		if dots != "" {
			return []string{dots + module}
		}
		if name := firstComponent(module); name != "" {
			return []string{name}
		}
		return nil
	}
	m := importRE.FindStringSubmatch(line)
	if m == nil {
		return nil
	}
	spec := m[1]
	if idx := strings.Index(spec, "#"); idx >= 0 {
		spec = spec[:idx]
	}
	var names []string
	for _, part := range strings.Split(spec, ",") {
		fields := strings.Fields(part)
		if len(fields) == 0 {
			continue
		}
		if name := firstComponent(fields[0]); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func firstComponent(dotted string) string {
	name, _, _ := strings.Cut(strings.TrimSpace(dotted), ".")
	if !identifierRE.MatchString(name) {
		return ""
	}
	return name
}

func docstringOpener(line string) string {
	for _, delim := range []string{`"""`, "'''"} {
		if strings.Count(line, delim) == 1 {
			return delim
		}
	}
	return ""
}

func classify(roots []string, module string, shipped map[string]bool) (Finding, bool) {
	if strings.HasPrefix(module, ".") {
		return Finding{Module: module, Kind: KindRelative}, true
	}
	if module == "" || shipped[module] {
		return Finding{}, false
	}
	for _, dir := range roots {
		if info, err := os.Stat(filepath.Join(dir, module)); err == nil && info.IsDir() {
			return Finding{Module: module, Path: filepath.Join(dir, module), Kind: KindPackage}, true
		}
		modulePath := filepath.Join(dir, module+".py")
		if info, err := os.Stat(modulePath); err == nil && !info.IsDir() {
			return Finding{Module: module, Path: modulePath, Kind: KindModule}, true
		}
	}
	return Finding{}, false
}

func Error(entrypoint string, findings []Finding) error {
	if len(findings) == 0 {
		return nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s imports code that will not be shipped with the run:\n", entrypoint)
	for _, f := range findings {
		switch f.Kind {
		case KindRelative:
			fmt.Fprintf(&b, "  line %d: %q is a relative import, which requires the entrypoint to run as part of a package; Tau runs it as a top-level script\n", f.Line, f.Module)
		case KindPackage:
			fmt.Fprintf(&b, "  line %d: %q resolves to the local package directory %s; embedded payload file names are flat, so directories cannot be shipped\n", f.Line, f.Module, f.Path)
		default:
			fmt.Fprintf(&b, "  line %d: %q resolves to the local file %s, which is not part of this run's payload\n", f.Line, f.Module, f.Path)
		}
	}
	b.WriteString("\nBy default Tau embeds only the entrypoint in the workload spec, so nothing else " +
		"from the project directory is shipped. Set run.working_dir to the project root to ship the " +
		"whole directory: Tau packages it and hands it to Ray as a runtime_env working_dir, which puts " +
		"it on PYTHONPATH for the driver and every worker, so these imports resolve. " +
		"Alternatively, inline the code into the entrypoint or bake it into a custom Ray image. " +
		"Caught before submission: this would otherwise fail with ModuleNotFoundError " +
		"after the Ray cluster had already started.")
	return fmt.Errorf("%s", b.String())
}
