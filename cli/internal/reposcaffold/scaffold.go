// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package reposcaffold renders Tau-ready Python research repository scaffolds.
package reposcaffold

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/template"
	"unicode"

	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/core/version"
)

//go:embed templates/**
var templateFS embed.FS

const (
	TemplatePython         = "python"
	TemplateExternalGitHub = "external-github"
	TemplateDPR            = "dpr"

	defaultPythonVersion = "3.12"
	defaultDPRRepo       = "https://github.com/facebookresearch/DPR.git"
	defaultDPRRef        = "main"
	defaultTauRev        = "main"
)

// Options controls one scaffold render.
type Options struct {
	Name                string
	Template            string
	OutputDir           string
	Image               string
	PythonVersion       string
	Package             string
	TauRev              string
	Workspace           string
	AzureSubscriptionID string
	AzureTenantID       string
	AKSResourceGroup    string
	AKSCluster          string
	ACRName             string
	UpstreamRepo        string
	UpstreamRef         string
	PackageImport       string
	SmokeCommand        string
	TrainCommand        string
	Force               bool
}

// Result summarizes the generated files.
type Result struct {
	OutputDir string
	Files     []string
}

type fileSpec struct {
	TemplatePath string
	OutputPath   string
	Mode         os.FileMode
}

type templateData struct {
	Name                   string
	Title                  string
	RunName                string
	Template               string
	Package                string
	PackageImport          string
	PythonVersion          string
	TauRev                 string
	Image                  string
	Workspace              string
	AzureSubscriptionID    string
	AzureTenantID          string
	AKSResourceGroup       string
	AKSCluster             string
	AKSKubeContext         string
	ACRName                string
	ACRLoginServer         string
	UpstreamRepo           string
	UpstreamRef            string
	UpstreamRoot           string
	SmokeCommand           string
	TrainCommand           string
	SmokeCommandSQ         string
	TrainCommandSQ         string
	SmokeCommandPy         string
	TrainCommandPy         string
	SmokeCommandYAML       string
	TrainCommandYAML       string
	HasUpstream            bool
	HasPackageImport       bool
	HasWorkspaceConnection bool
	AKSResourceID          string
	MinTauVersion          string
	UsesDPR                bool
}

// Render writes the selected scaffold to disk.
func Render(opts Options) (Result, error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return Result{}, err
	}
	files := templateFiles(normalized)
	if err := preflightGeneratedFiles(normalized.OutputDir, files, normalized.Force); err != nil {
		return Result{}, err
	}
	data := buildTemplateData(normalized)
	written := make([]string, 0, len(files))
	for _, spec := range files {
		raw, err := renderTemplate(spec.TemplatePath, data)
		if err != nil {
			return Result{}, err
		}
		dst := filepath.Join(normalized.OutputDir, filepath.FromSlash(spec.OutputPath))
		if err := writeGeneratedFile(dst, raw, spec.Mode, normalized.Force); err != nil {
			return Result{}, err
		}
		written = append(written, spec.OutputPath)
	}
	sort.Strings(written)
	return Result{OutputDir: normalized.OutputDir, Files: written}, nil
}

// Preview renders files in memory for tests and dry validation.
func Preview(opts Options) ([]RenderedFile, error) {
	normalized, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	data := buildTemplateData(normalized)
	files := templateFiles(normalized)
	out := make([]RenderedFile, 0, len(files))
	for _, spec := range files {
		raw, err := renderTemplate(spec.TemplatePath, data)
		if err != nil {
			return nil, err
		}
		out = append(out, RenderedFile{Path: spec.OutputPath, Mode: spec.Mode, Content: string(raw)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// RenderedFile is one in-memory scaffold output.
type RenderedFile struct {
	Path    string
	Mode    os.FileMode
	Content string
}

func normalizeOptions(opts Options) (Options, error) {
	opts.Name = strings.TrimSpace(opts.Name)
	if opts.Name == "" {
		return Options{}, errors.New("name is required")
	}
	opts.Template = strings.TrimSpace(opts.Template)
	if opts.Template == "" {
		opts.Template = TemplatePython
	}
	switch opts.Template {
	case TemplatePython, TemplateExternalGitHub, TemplateDPR:
	default:
		return Options{}, fmt.Errorf("--template must be one of: %s, %s, %s", TemplatePython, TemplateExternalGitHub, TemplateDPR)
	}
	opts.OutputDir = strings.TrimSpace(opts.OutputDir)
	if opts.OutputDir == "" {
		opts.OutputDir = "./" + fileutil.SafePathComponent(opts.Name)
	}
	opts.Image = strings.TrimSpace(opts.Image)
	if opts.Image == "" {
		return Options{}, errors.New("--image is required")
	}
	opts.PythonVersion = strings.TrimSpace(opts.PythonVersion)
	if opts.PythonVersion == "" {
		opts.PythonVersion = defaultPythonVersion
	}
	pkg, err := normalizePackageName(firstNonEmpty(opts.Package, opts.Name))
	if err != nil {
		return Options{}, fmt.Errorf("invalid Python package name from NAME/--package: %w", err)
	}
	opts.Package = pkg
	opts.TauRev = strings.TrimSpace(opts.TauRev)
	if opts.TauRev == "" {
		opts.TauRev = defaultTauRev
	}
	opts.Workspace = strings.TrimSpace(opts.Workspace)
	if opts.Workspace == "" {
		opts.Workspace = "<your-workspace>"
	}
	opts.AzureSubscriptionID = strings.TrimSpace(opts.AzureSubscriptionID)
	opts.AzureTenantID = strings.TrimSpace(opts.AzureTenantID)
	opts.AKSResourceGroup = strings.TrimSpace(opts.AKSResourceGroup)
	opts.AKSCluster = strings.TrimSpace(opts.AKSCluster)
	opts.ACRName = strings.TrimSpace(opts.ACRName)

	switch opts.Template {
	case TemplateDPR:
		opts.UpstreamRepo = firstNonEmpty(opts.UpstreamRepo, defaultDPRRepo)
		opts.UpstreamRef = firstNonEmpty(opts.UpstreamRef, defaultDPRRef)
		opts.PackageImport = firstNonEmpty(opts.PackageImport, "dpr")
		opts.SmokeCommand = firstNonEmpty(opts.SmokeCommand, "python train_dense_encoder.py --help")
		opts.TrainCommand = firstNonEmpty(opts.TrainCommand, "python train_dense_encoder.py --help")
	case TemplateExternalGitHub:
		if strings.TrimSpace(opts.UpstreamRepo) == "" {
			return Options{}, errors.New("--upstream is required for --template external-github")
		}
		if strings.TrimSpace(opts.UpstreamRef) == "" {
			return Options{}, errors.New("--ref is required for --template external-github")
		}
		opts.SmokeCommand = firstNonEmpty(opts.SmokeCommand, "python -m compileall .")
		opts.TrainCommand = firstNonEmpty(opts.TrainCommand, opts.SmokeCommand)
	default:
		opts.SmokeCommand = firstNonEmpty(opts.SmokeCommand, "uv run python -m "+opts.Package+".smoke")
		opts.TrainCommand = firstNonEmpty(opts.TrainCommand, "uv run python -m "+opts.Package+".train")
	}
	opts.UpstreamRepo = strings.TrimSpace(opts.UpstreamRepo)
	opts.UpstreamRef = strings.TrimSpace(opts.UpstreamRef)
	opts.PackageImport = strings.TrimSpace(opts.PackageImport)
	opts.SmokeCommand = strings.TrimSpace(opts.SmokeCommand)
	opts.TrainCommand = strings.TrimSpace(opts.TrainCommand)
	return opts, nil
}

func buildTemplateData(opts Options) templateData {
	packageImport := opts.PackageImport
	hasUpstream := opts.Template == TemplateExternalGitHub || opts.Template == TemplateDPR
	acrLogin := ""
	if opts.ACRName != "" {
		acrLogin = opts.ACRName + ".azurecr.io"
	}
	hasWorkspaceConnection := opts.Workspace != "<your-workspace>" &&
		opts.AzureSubscriptionID != "" &&
		opts.AzureTenantID != "" &&
		opts.AKSResourceGroup != "" &&
		opts.AKSCluster != ""
	aksResourceID := ""
	if hasWorkspaceConnection {
		aksResourceID = fmt.Sprintf(
			"/subscriptions/%s/resourceGroups/%s/providers/Microsoft.ContainerService/managedClusters/%s",
			opts.AzureSubscriptionID,
			opts.AKSResourceGroup,
			opts.AKSCluster,
		)
	}
	return templateData{
		Name:                   opts.Name,
		Title:                  titleFromName(opts.Name),
		RunName:                normalizeKubernetesName(opts.Name),
		Template:               opts.Template,
		Package:                opts.Package,
		PackageImport:          packageImport,
		PythonVersion:          opts.PythonVersion,
		TauRev:                 opts.TauRev,
		Image:                  opts.Image,
		Workspace:              opts.Workspace,
		AzureSubscriptionID:    opts.AzureSubscriptionID,
		AzureTenantID:          opts.AzureTenantID,
		AKSResourceGroup:       opts.AKSResourceGroup,
		AKSCluster:             opts.AKSCluster,
		AKSKubeContext:         opts.AKSCluster,
		ACRName:                opts.ACRName,
		ACRLoginServer:         acrLogin,
		UpstreamRepo:           opts.UpstreamRepo,
		UpstreamRef:            opts.UpstreamRef,
		UpstreamRoot:           "/opt/upstream",
		SmokeCommand:           opts.SmokeCommand,
		TrainCommand:           opts.TrainCommand,
		SmokeCommandSQ:         shellSingleQuote(opts.SmokeCommand),
		TrainCommandSQ:         shellSingleQuote(opts.TrainCommand),
		SmokeCommandPy:         strconv.Quote(opts.SmokeCommand),
		TrainCommandPy:         strconv.Quote(opts.TrainCommand),
		SmokeCommandYAML:       yamlSingleQuote(opts.SmokeCommand),
		TrainCommandYAML:       yamlSingleQuote(opts.TrainCommand),
		HasUpstream:            hasUpstream,
		HasPackageImport:       packageImport != "",
		HasWorkspaceConnection: hasWorkspaceConnection,
		AKSResourceID:          aksResourceID,
		MinTauVersion:          minTauVersion(version.Version),
		UsesDPR:                opts.Template == TemplateDPR,
	}
}

func templateFiles(opts Options) []fileSpec {
	pkgPath := "src/" + opts.Package
	files := []fileSpec{
		{"templates/common/AGENTS.md.tmpl", "AGENTS.md", 0o644},
		{"templates/common/README.md.tmpl", "README.md", 0o644},
		{"templates/common/env.example.tmpl", ".env.example", 0o644},
		{"templates/common/gitignore.tmpl", ".gitignore", 0o644},
		{"templates/common/pyproject.toml.tmpl", "pyproject.toml", 0o644},
		{"templates/common/python-version.tmpl", ".python-version", 0o644},
		{"templates/common/setup.sh.tmpl", "scripts/setup.sh", 0o755},
		{"templates/common/setup-azure.sh.tmpl", "scripts/setup-azure.sh", 0o755},
		{"templates/common/doctor.sh.tmpl", "scripts/doctor.sh", 0o755},
		{"templates/common/configure.sh.tmpl", "scripts/configure.sh", 0o755},
		{"templates/common/smoke.sh.tmpl", "scripts/smoke.sh", 0o755},
		{"templates/common/train.sh.tmpl", "scripts/train.sh", 0o755},
		{"templates/common/smoke.yaml.tmpl", "tau/smoke.yaml", 0o644},
		{"templates/common/train.yaml.tmpl", "tau/train.yaml", 0o644},
		{"templates/common/train-gpu.yaml.tmpl", "tau/train-gpu.yaml", 0o644},
		{"templates/common/init.py.tmpl", pkgPath + "/__init__.py", 0o644},
		{"templates/common/train.py.tmpl", pkgPath + "/train.py", 0o644},
	}
	if opts.Workspace != "<your-workspace>" &&
		opts.AzureSubscriptionID != "" &&
		opts.AzureTenantID != "" &&
		opts.AKSResourceGroup != "" &&
		opts.AKSCluster != "" {
		files = append(files, fileSpec{
			"templates/common/workspace.connection.yaml.tmpl",
			"tau/workspace.connection.yaml",
			0o644,
		})
	}
	if opts.Template == TemplatePython {
		files = append(files,
			fileSpec{"templates/python/smoke.py.tmpl", pkgPath + "/smoke.py", 0o644},
			fileSpec{"templates/python/Dockerfile.tmpl", "images/train.Dockerfile", 0o644},
		)
		return files
	}
	files = append(files,
		fileSpec{"templates/external-github/smoke.py.tmpl", pkgPath + "/smoke.py", 0o644},
		fileSpec{"templates/external-github/Dockerfile.tmpl", "images/train.Dockerfile", 0o644},
	)
	return files
}

func renderTemplate(path string, data templateData) ([]byte, error) {
	raw, err := fs.ReadFile(templateFS, path)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New(filepath.Base(path)).Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render %s: %w", path, err)
	}
	return buf.Bytes(), nil
}

func writeGeneratedFile(path string, raw []byte, perm os.FileMode, force bool) error {
	if !force {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite generator-managed files", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return fileutil.WriteFileAtomic(path, raw, perm)
}

func preflightGeneratedFiles(outputDir string, files []fileSpec, force bool) error {
	if force {
		return nil
	}
	for _, spec := range files {
		dst := filepath.Join(outputDir, filepath.FromSlash(spec.OutputPath))
		if _, err := os.Stat(dst); err == nil {
			return fmt.Errorf("%s already exists; pass --force to overwrite generator-managed files", dst)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func normalizePackageName(value string) (string, error) {
	original := value
	value = strings.TrimSpace(strings.ToLower(value))
	if value == "" {
		return "", errors.New("value is empty")
	}
	var b strings.Builder
	lastUnderscore := false
	for _, r := range value {
		ok := r == '_' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case ok:
		case r == '-' || r == '.' || unicode.IsSpace(r):
			ok = true
			r = '_'
		default:
			return "", fmt.Errorf("%q contains unsupported character %q; use ASCII letters, digits, underscores, hyphens, dots, or spaces", original, r)
		}
		if r == '_' {
			if lastUnderscore {
				continue
			}
			lastUnderscore = true
		} else {
			lastUnderscore = false
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "", fmt.Errorf("%q does not contain any letters or digits", original)
	}
	if out[0] >= '0' && out[0] <= '9' {
		out = "tau_" + out
	}
	if !isPythonPackageName(out) {
		return "", fmt.Errorf("%q normalizes to invalid Python package %q", original, out)
	}
	return out, nil
}

func isPythonPackageName(value string) bool {
	if value == "" {
		return false
	}
	for i, r := range value {
		if i == 0 {
			if !((r >= 'a' && r <= 'z') || r == '_') {
				return false
			}
			continue
		}
		if !((r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_') {
			return false
		}
	}
	return true
}

func normalizeKubernetesName(value string) string {
	value = strings.TrimSpace(strings.ToLower(value))
	var b strings.Builder
	lastDash := false
	for _, r := range value {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		switch {
		case ok:
		case r == '-' || r == '_' || r == '.' || unicode.IsSpace(r):
			ok = true
			r = '-'
		default:
			ok = true
			r = '-'
		}
		if r == '-' {
			if lastDash {
				continue
			}
			lastDash = true
		} else {
			lastDash = false
		}
		b.WriteRune(r)
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "tau-" + fileutil.ShortStringHash(value)
	}
	if out[0] < 'a' || out[0] > 'z' {
		out = "tau-" + out
	}
	const maxRunNamePrefixLen = 50
	if len(out) > maxRunNamePrefixLen {
		out = strings.TrimRight(out[:maxRunNamePrefixLen], "-")
	}
	if out == "" {
		return "tau-workload"
	}
	return out
}

func titleFromName(value string) string {
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for i, part := range parts {
		if part == "" {
			continue
		}
		parts[i] = strings.ToUpper(part[:1]) + part[1:]
	}
	if len(parts) == 0 {
		return "Tau Project"
	}
	return strings.Join(parts, " ")
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func shellSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func yamlSingleQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// minTauVersion is the requirements.minTauVersion written into a generated
// repository's workspace connection descriptor.
//
// It is derived from the generating binary rather than hardcoded. A literal
// pinned ahead of the shipped release makes every generated repo unrunnable —
// `tau run TARGET` rejects it with "install Tau X or newer" naming a version
// that was never published, and blames the researcher for a scaffold defect.
//
// The floor is the generator's own major.minor with patch 0, so patch releases
// of the same line stay compatible while a repo scaffolded by a newer minor
// still refuses an older CLI. Unstamped `go build` binaries report "dev", which
// is not a version; those fall back to 0.0.0, which every real release
// satisfies.
func minTauVersion(current string) string {
	value := strings.TrimPrefix(strings.TrimSpace(current), "v")
	if base, _, found := strings.Cut(value, "-"); found {
		value = base
	}
	parts := strings.Split(value, ".")
	if len(parts) < 2 {
		return "0.0.0"
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil {
		return "0.0.0"
	}
	return fmt.Sprintf("%d.%d.0", major, minor)
}
