// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package projectcatalog owns strict Tau monorepo project discovery and
// filesystem-derived run targets.
package projectcatalog

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/cli/internal/repository"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

const (
	Filename = "tau.projects.yaml"
	Schema   = "tau.projects.v1"
)

var (
	ErrCatalogNotFound = errors.New("Tau project catalog not found")
	projectNamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
)

type ProjectSpec struct {
	Path       string `yaml:"path"`
	Connection string `yaml:"connection"`
}

type Spec struct {
	Schema   string                 `yaml:"schema"`
	Projects map[string]ProjectSpec `yaml:"projects"`
}

type Repository struct {
	Boundary repository.Boundary
	Catalog  *Catalog
}

type Catalog struct {
	LexicalRoot string
	Root        string
	Path        string
	Projects    map[string]*Project
	gitlinks    repository.GitlinkIndex
}

type Project struct {
	Name               string
	Path               string
	LexicalRoot        string
	Root               string
	ConnectionPath     string
	Connection         workspaceconnection.Discovery
	Targets            map[string]string
	DefaultConfigPath  string
	defaultConfigCount int
}

// Parse decodes the strict, versioned catalog schema.
func Parse(raw []byte) (Spec, error) {
	if err := validateYAMLStructure(raw); err != nil {
		return Spec{}, fmt.Errorf("parse %s: %w", Filename, err)
	}
	var spec Spec
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&spec); err != nil {
		if err == io.EOF {
			return Spec{}, fmt.Errorf("parse %s: catalog is empty", Filename)
		}
		return Spec{}, fmt.Errorf("parse %s: %w", Filename, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Spec{}, fmt.Errorf("parse %s: multiple YAML documents are not allowed", Filename)
		}
		return Spec{}, fmt.Errorf("parse %s: %w", Filename, err)
	}
	if spec.Schema != Schema {
		return Spec{}, fmt.Errorf("project catalog schema %q is unsupported; expected %q", spec.Schema, Schema)
	}
	if len(spec.Projects) == 0 {
		return Spec{}, fmt.Errorf("project catalog projects must contain at least one project")
	}
	for name, project := range spec.Projects {
		if !projectNamePattern.MatchString(name) {
			return Spec{}, fmt.Errorf("project name %q is invalid; use lowercase letters, digits, and internal hyphens", name)
		}
		if err := validateRelativePath(project.Path); err != nil {
			return Spec{}, fmt.Errorf("project %q path: %w", name, err)
		}
		if err := validateRelativePath(project.Connection); err != nil {
			return Spec{}, fmt.Errorf("project %q connection: %w", name, err)
		}
	}
	return spec, nil
}

func validateYAMLStructure(raw []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		if err == io.EOF {
			return fmt.Errorf("catalog is empty")
		}
		return err
	}
	if len(document.Content) != 1 {
		return fmt.Errorf("catalog must contain one YAML document")
	}
	if err := validateYAMLNode(document.Content[0]); err != nil {
		return err
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple YAML documents are not allowed")
		}
		return err
	}
	return nil
}

func validateYAMLNode(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode {
		return fmt.Errorf("YAML aliases are not allowed")
	}
	if node.Kind == yaml.ScalarNode && node.Tag != "!!str" {
		return fmt.Errorf("YAML scalar %q must be a string", node.Value)
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := validateYAMLNode(child); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			value := node.Content[index+1]
			if key.Kind == yaml.AliasNode {
				return fmt.Errorf("YAML aliases are not allowed as mapping keys")
			}
			if key.Kind != yaml.ScalarNode {
				return fmt.Errorf("YAML mapping keys must be scalar values")
			}
			if key.Value == "<<" || key.Tag == "!!merge" {
				return fmt.Errorf("YAML merge keys are not allowed")
			}
			if key.Tag != "!!str" {
				return fmt.Errorf("YAML mapping key %q must be a string", key.Value)
			}
			semanticKey := key.Value
			if _, exists := seen[semanticKey]; exists {
				return fmt.Errorf("duplicate YAML mapping key %q", key.Value)
			}
			seen[semanticKey] = struct{}{}
			if err := validateYAMLNode(value); err != nil {
				return err
			}
		}
	}
	return nil
}

// Discover resolves the exact Git worktree for start and loads only the catalog
// at that root. Non-Git directories never search parent directories.
func Discover(start string) (Repository, error) {
	boundary, err := repository.Resolve(start)
	if err != nil {
		return Repository{}, err
	}
	result := Repository{Boundary: boundary}
	if !boundary.Git {
		return result, nil
	}

	catalogPath := filepath.Join(boundary.LexicalRoot, Filename)
	if _, err := os.Lstat(catalogPath); err != nil {
		if os.IsNotExist(err) {
			return result, nil
		}
		return Repository{}, fmt.Errorf("inspect project catalog %s: %w", catalogPath, err)
	}
	catalog, err := loadBoundary(boundary)
	if err != nil {
		return Repository{}, err
	}
	result.Catalog = catalog
	return result, nil
}

// DiscoverLexicalConfig finds the Git boundary containing a config's lexical
// path without following symlinked directory components past that boundary.
func DiscoverLexicalConfig(configPath string) (Repository, error) {
	absolute, err := filepath.Abs(configPath)
	if err != nil {
		return Repository{}, fmt.Errorf("resolve lexical config path: %w", err)
	}
	current := filepath.Dir(filepath.Clean(absolute))
	noGitBoundary := current
	for {
		if parent, found, err := internalGitSymlinkParent(current); err != nil {
			return Repository{}, err
		} else if found {
			current = parent
			noGitBoundary = parent
			continue
		}
		catalogPath := filepath.Join(current, Filename)
		if _, err := os.Lstat(catalogPath); err == nil {
			boundary, err := repository.Resolve(current)
			if err != nil {
				return Repository{}, err
			}
			exactRoot := false
			if boundary.Git {
				exactRoot, err = repository.SamePath(boundary.StartDir, boundary.Root)
				if err != nil {
					return Repository{}, fmt.Errorf("compare lexical catalog directory to Git root: %w", err)
				}
			}
			if boundary.Git && exactRoot {
				catalog, err := loadBoundary(boundary)
				if err != nil {
					return Repository{}, err
				}
				return Repository{Boundary: boundary, Catalog: catalog}, nil
			}
		} else if !os.IsNotExist(err) {
			return Repository{}, fmt.Errorf("inspect lexical project catalog %s: %w", catalogPath, err)
		}
		gitMarker := filepath.Join(current, ".git")
		if _, err := os.Lstat(gitMarker); err == nil {
			boundary, err := repository.Resolve(current)
			if err != nil {
				return Repository{}, err
			}
			return Repository{Boundary: boundary}, nil
		} else if !os.IsNotExist(err) {
			return Repository{}, fmt.Errorf("inspect lexical Git boundary %s: %w", gitMarker, err)
		}
		parent := filepath.Dir(current)
		if parent == current {
			boundary, err := repository.Resolve(noGitBoundary)
			if err != nil {
				return Repository{}, err
			}
			return Repository{Boundary: boundary}, nil
		}
		current = parent
	}
}

func internalGitSymlinkParent(start string) (string, bool, error) {
	current := filepath.Clean(start)
	for {
		info, err := os.Lstat(current)
		if err != nil {
			if !os.IsNotExist(err) {
				return "", false, fmt.Errorf("inspect lexical path component %s: %w", current, err)
			}
		} else if info.Mode()&os.ModeSymlink != 0 {
			parent := filepath.Dir(current)
			if filepath.Dir(parent) != parent {
				return parent, true, nil
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false, nil
		}
		current = parent
	}
}

// Load validates the catalog at one exact Git worktree root.
func Load(root string) (*Catalog, error) {
	boundary, err := repository.Resolve(root)
	if err != nil {
		return nil, err
	}
	if !boundary.Git {
		return nil, fmt.Errorf("project catalog root %s must be an exact Git worktree root", root)
	}
	exactRoot, err := repository.SamePath(boundary.Root, boundary.StartDir)
	if err != nil {
		return nil, fmt.Errorf("compare project catalog root %s to Git worktree: %w", root, err)
	}
	if !exactRoot {
		return nil, fmt.Errorf("project catalog root %s must be an exact Git worktree root", root)
	}
	return loadBoundary(boundary)
}

func loadBoundary(boundary repository.Boundary) (*Catalog, error) {
	catalogPath := filepath.Join(boundary.LexicalRoot, Filename)
	catalogAbsolute, catalogReal, catalogInfo, err := repository.ExistingPath(catalogPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: expected %s", ErrCatalogNotFound, catalogPath)
		}
		return nil, fmt.Errorf("resolve project catalog %s: %w", catalogPath, err)
	}
	if !catalogInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("project catalog %s is not a regular file", catalogAbsolute)
	}
	physicalCatalogContained, err := repository.PathContains(boundary.Root, catalogReal)
	if err != nil {
		return nil, fmt.Errorf("compare project catalog to Git worktree: %w", err)
	}
	if !repository.Contains(boundary.LexicalRoot, catalogAbsolute) || !physicalCatalogContained {
		return nil, fmt.Errorf("project catalog %s resolves outside Git worktree %s", catalogAbsolute, boundary.LexicalRoot)
	}
	catalogBoundary, err := repository.Resolve(catalogReal)
	if err != nil {
		return nil, fmt.Errorf("resolve project catalog Git boundary: %w", err)
	}
	sameCatalogRoot := false
	if catalogBoundary.Git {
		sameCatalogRoot, err = repository.SamePath(catalogBoundary.Root, boundary.Root)
		if err != nil {
			return nil, fmt.Errorf("compare project catalog Git worktree: %w", err)
		}
	}
	if !catalogBoundary.Git || !sameCatalogRoot {
		return nil, fmt.Errorf(
			"project catalog %s belongs to Git worktree %s, not %s",
			catalogAbsolute,
			catalogBoundary.Root,
			boundary.Root,
		)
	}
	gitlinks, err := repository.LoadGitlinks(boundary.LexicalRoot)
	if err != nil {
		return nil, err
	}
	relativeCatalog, err := filepath.Rel(boundary.LexicalRoot, catalogAbsolute)
	if err != nil {
		return nil, fmt.Errorf("resolve project catalog path relative to worktree: %w", err)
	}
	if gitlink, found, err := gitlinks.AtOrAbove(relativeCatalog); err != nil {
		return nil, fmt.Errorf("inspect project catalog Git submodule ownership: %w", err)
	} else if found {
		return nil, fmt.Errorf("project catalog %s is at or beneath Git submodule %q", catalogAbsolute, gitlink)
	}
	raw, err := os.ReadFile(catalogAbsolute)
	if err != nil {
		return nil, fmt.Errorf("read project catalog %s: %w", catalogAbsolute, err)
	}
	spec, err := Parse(raw)
	if err != nil {
		return nil, err
	}
	catalog := &Catalog{
		LexicalRoot: boundary.LexicalRoot,
		Root:        boundary.Root,
		Path:        catalogAbsolute,
		Projects:    make(map[string]*Project, len(spec.Projects)),
		gitlinks:    gitlinks,
	}
	names := sortedSpecNames(spec.Projects)
	for _, name := range names {
		project, err := loadProject(boundary.LexicalRoot, boundary.Root, gitlinks, name, spec.Projects[name])
		if err != nil {
			return nil, err
		}
		catalog.Projects[name] = project
	}
	if err := catalog.validateDisjointRoots(); err != nil {
		return nil, err
	}
	return catalog, nil
}

func loadProject(
	worktreeLexicalRoot, worktreeRoot string,
	gitlinks repository.GitlinkIndex,
	name string,
	spec ProjectSpec,
) (*Project, error) {
	if gitlink, found, err := gitlinks.AtOrAbove(spec.Path); err != nil {
		return nil, fmt.Errorf("project %q path: %w", name, err)
	} else if found {
		return nil, fmt.Errorf("project %q path %q is at or beneath Git submodule %q", name, spec.Path, gitlink)
	}
	lexicalRoot, realRoot, info, err := resolveCatalogPath(worktreeLexicalRoot, worktreeRoot, spec.Path)
	if err != nil {
		return nil, fmt.Errorf("project %q path: %w", name, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("project %q path %s is not a directory", name, lexicalRoot)
	}
	projectBoundary, err := repository.Resolve(realRoot)
	if err != nil {
		return nil, fmt.Errorf("project %q path: %w", name, err)
	}
	sameProjectRoot := false
	if projectBoundary.Git {
		sameProjectRoot, err = repository.SamePath(projectBoundary.Root, worktreeRoot)
		if err != nil {
			return nil, fmt.Errorf("project %q path: compare Git worktree: %w", name, err)
		}
	}
	if !projectBoundary.Git || !sameProjectRoot {
		return nil, fmt.Errorf(
			"project %q path %s belongs to Git worktree %s, not catalog worktree %s",
			name,
			lexicalRoot,
			projectBoundary.Root,
			worktreeRoot,
		)
	}

	if gitlink, found, gitlinkErr := gitlinks.AtOrAbove(spec.Connection); gitlinkErr != nil {
		return nil, fmt.Errorf("project %q connection: %w", name, gitlinkErr)
	} else if found {
		return nil, fmt.Errorf("project %q connection %q is at or beneath Git submodule %q", name, spec.Connection, gitlink)
	}
	connectionLexical, connectionReal, connectionInfo, err := resolveCatalogPath(worktreeLexicalRoot, worktreeRoot, spec.Connection)
	if err != nil {
		return nil, fmt.Errorf("project %q connection: %w", name, err)
	}
	if !connectionInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("project %q connection %s is not a regular file", name, connectionLexical)
	}
	connectionBoundary, err := repository.Resolve(connectionReal)
	if err != nil {
		return nil, fmt.Errorf("project %q connection: %w", name, err)
	}
	sameConnectionRoot := false
	if connectionBoundary.Git {
		sameConnectionRoot, err = repository.SamePath(connectionBoundary.Root, worktreeRoot)
		if err != nil {
			return nil, fmt.Errorf("project %q connection: compare Git worktree: %w", name, err)
		}
	}
	if !connectionBoundary.Git || !sameConnectionRoot {
		return nil, fmt.Errorf(
			"project %q connection %s belongs to Git worktree %s, not catalog worktree %s",
			name,
			connectionLexical,
			connectionBoundary.Root,
			worktreeRoot,
		)
	}
	connection, err := workspaceconnection.LoadFile(connectionLexical, worktreeLexicalRoot)
	if err != nil {
		return nil, fmt.Errorf("project %q connection: %w", name, err)
	}

	project := &Project{
		Name:           name,
		Path:           spec.Path,
		LexicalRoot:    lexicalRoot,
		Root:           realRoot,
		ConnectionPath: connectionReal,
		Connection:     connection,
		Targets:        map[string]string{},
	}
	if err := project.loadTargets(worktreeLexicalRoot, worktreeRoot, gitlinks); err != nil {
		return nil, err
	}
	if err := project.loadDefaultConfig(worktreeLexicalRoot, worktreeRoot, gitlinks); err != nil {
		return nil, err
	}
	return project, nil
}

func (p *Project) loadTargets(
	worktreeLexicalRoot, worktreeRoot string,
	gitlinks repository.GitlinkIndex,
) error {
	tauDir := filepath.Join(p.LexicalRoot, "tau")
	entries, err := os.ReadDir(tauDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("project %q read target directory %s: %w", p.Name, tauDir, err)
	}
	extensions := map[string]string{}
	for _, entry := range entries {
		extension := filepath.Ext(entry.Name())
		if extension != ".yaml" && extension != ".yml" {
			continue
		}
		target := strings.TrimSuffix(entry.Name(), extension)
		if target == "" || entry.Name() == filepath.Base(workspaceconnection.DescriptorRelativePath) {
			continue
		}
		candidate := filepath.Join(tauDir, entry.Name())
		relativeCandidate, err := filepath.Rel(worktreeLexicalRoot, candidate)
		if err != nil {
			return fmt.Errorf("project %q target %q: %w", p.Name, target, err)
		}
		if gitlink, found, err := gitlinks.AtOrAbove(relativeCandidate); err != nil {
			return fmt.Errorf("project %q target %q: %w", p.Name, target, err)
		} else if found {
			return fmt.Errorf("project %q target %q is at or beneath Git submodule %q", p.Name, target, gitlink)
		}
		lexical, realPath, info, err := repository.ExistingPath(candidate)
		if err != nil {
			return fmt.Errorf("project %q target %q: %w", p.Name, target, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		withinWorktree, err := repository.PathContains(worktreeRoot, realPath)
		if err != nil {
			return fmt.Errorf("project %q target %q: compare worktree containment: %w", p.Name, target, err)
		}
		withinProject, err := repository.PathContains(p.Root, realPath)
		if err != nil {
			return fmt.Errorf("project %q target %q: compare project containment: %w", p.Name, target, err)
		}
		if !repository.Contains(worktreeLexicalRoot, lexical) ||
			!withinWorktree ||
			!repository.Contains(p.LexicalRoot, lexical) ||
			!withinProject {
			return fmt.Errorf("project %q target %q escapes project root", p.Name, target)
		}
		targetBoundary, err := repository.Resolve(realPath)
		if err != nil {
			return fmt.Errorf("project %q target %q: %w", p.Name, target, err)
		}
		sameTargetRoot := false
		if targetBoundary.Git {
			sameTargetRoot, err = repository.SamePath(targetBoundary.Root, worktreeRoot)
			if err != nil {
				return fmt.Errorf("project %q target %q: compare Git worktree: %w", p.Name, target, err)
			}
		}
		if !targetBoundary.Git || !sameTargetRoot {
			return fmt.Errorf(
				"project %q target %q belongs to Git worktree %s, not catalog worktree %s",
				p.Name,
				target,
				targetBoundary.Root,
				worktreeRoot,
			)
		}
		if previous, ok := extensions[target]; ok {
			return fmt.Errorf(
				"project %q run target %q has both %s and %s; keep exactly one extension",
				p.Name,
				target,
				previous,
				extension,
			)
		}
		extensions[target] = extension
		p.Targets[target] = lexical
	}
	return nil
}

func (p *Project) loadDefaultConfig(
	worktreeLexicalRoot, worktreeRoot string,
	gitlinks repository.GitlinkIndex,
) error {
	for _, name := range []string{"tau.yaml", "tau.yml", ".tau.yaml"} {
		candidate := filepath.Join(p.LexicalRoot, name)
		relativeCandidate, err := filepath.Rel(worktreeLexicalRoot, candidate)
		if err != nil {
			return fmt.Errorf("project %q default config %s: %w", p.Name, candidate, err)
		}
		if gitlink, found, err := gitlinks.AtOrAbove(relativeCandidate); err != nil {
			return fmt.Errorf("project %q default config %s: %w", p.Name, candidate, err)
		} else if found {
			return fmt.Errorf("project %q default config %s is at or beneath Git submodule %q", p.Name, candidate, gitlink)
		}
		lexical, realPath, info, err := repository.ExistingPath(candidate)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("project %q default config %s: %w", p.Name, candidate, err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		withinWorktree, err := repository.PathContains(worktreeRoot, realPath)
		if err != nil {
			return fmt.Errorf("project %q default config %s: compare worktree containment: %w", p.Name, candidate, err)
		}
		withinProject, err := repository.PathContains(p.Root, realPath)
		if err != nil {
			return fmt.Errorf("project %q default config %s: compare project containment: %w", p.Name, candidate, err)
		}
		if !repository.Contains(worktreeLexicalRoot, lexical) ||
			!withinWorktree ||
			!repository.Contains(p.LexicalRoot, lexical) ||
			!withinProject {
			return fmt.Errorf("project %q default config %s escapes project root", p.Name, candidate)
		}
		configBoundary, err := repository.Resolve(realPath)
		if err != nil {
			return fmt.Errorf("project %q default config %s: %w", p.Name, candidate, err)
		}
		sameConfigRoot := false
		if configBoundary.Git {
			sameConfigRoot, err = repository.SamePath(configBoundary.Root, worktreeRoot)
			if err != nil {
				return fmt.Errorf("project %q default config %s: compare Git worktree: %w", p.Name, candidate, err)
			}
		}
		if !configBoundary.Git || !sameConfigRoot {
			return fmt.Errorf(
				"project %q default config %s belongs to Git worktree %s, not catalog worktree %s",
				p.Name,
				candidate,
				configBoundary.Root,
				worktreeRoot,
			)
		}
		p.defaultConfigCount++
		p.DefaultConfigPath = lexical
	}
	if p.defaultConfigCount > 1 {
		return fmt.Errorf(
			"project %q has multiple default Tau configs; keep exactly one of tau.yaml, tau.yml, or .tau.yaml",
			p.Name,
		)
	}
	return nil
}

func (c *Catalog) validateDisjointRoots() error {
	names := c.Names()
	for i, firstName := range names {
		first := c.Projects[firstName]
		for _, secondName := range names[i+1:] {
			second := c.Projects[secondName]
			lexicalOverlap, err := repository.PathsOverlap(first.LexicalRoot, second.LexicalRoot)
			if err != nil {
				return fmt.Errorf("compare project roots %q and %q: %w", firstName, secondName, err)
			}
			realOverlap, err := repository.PathsOverlap(first.Root, second.Root)
			if err != nil {
				return fmt.Errorf("compare resolved project roots %q and %q: %w", firstName, secondName, err)
			}
			if pathsOverlap(first.LexicalRoot, second.LexicalRoot) ||
				pathsOverlap(first.Root, second.Root) ||
				lexicalOverlap ||
				realOverlap {
				return fmt.Errorf(
					"project roots must be disjoint: %q (%s) overlaps %q (%s)",
					firstName,
					first.Path,
					secondName,
					second.Path,
				)
			}
		}
	}
	return nil
}

func (c *Catalog) Names() []string {
	names := make([]string, 0, len(c.Projects))
	for name := range c.Projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func resolveCatalogPath(lexicalRoot, root, relative string) (string, string, os.FileInfo, error) {
	lexical := filepath.Clean(filepath.Join(lexicalRoot, filepath.FromSlash(relative)))
	if !repository.Contains(lexicalRoot, lexical) {
		return "", "", nil, fmt.Errorf("path %q escapes catalog worktree %s", relative, lexicalRoot)
	}
	absolute, realPath, info, err := repository.ExistingPath(lexical)
	if err != nil {
		return "", "", nil, err
	}
	physicalContained, err := repository.PathContains(root, realPath)
	if err != nil {
		return "", "", nil, err
	}
	if !repository.Contains(lexicalRoot, absolute) || !physicalContained {
		return "", "", nil, fmt.Errorf("path %q resolves outside catalog worktree %s", relative, lexicalRoot)
	}
	return absolute, realPath, info, nil
}

func validateRelativePath(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("is required")
	}
	if value != strings.TrimSpace(value) {
		return fmt.Errorf("%q must not have leading or trailing whitespace", value)
	}
	if filepath.IsAbs(value) || filepath.VolumeName(value) != "" || strings.HasPrefix(value, "/") {
		return fmt.Errorf("%q must be repository-relative", value)
	}
	if len(value) >= 3 &&
		((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) &&
		value[1] == ':' && value[2] == '/' {
		return fmt.Errorf("%q must be repository-relative", value)
	}
	if strings.Contains(value, `\`) {
		return fmt.Errorf("%q must use slash-separated path components", value)
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("%q contains an empty, dot, or parent path component", value)
		}
	}
	return nil
}

func sortedSpecNames(projects map[string]ProjectSpec) []string {
	names := make([]string, 0, len(projects))
	for name := range projects {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func pathsOverlap(first, second string) bool {
	return repository.Contains(first, second) || repository.Contains(second, first)
}
