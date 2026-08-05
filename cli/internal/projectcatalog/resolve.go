package projectcatalog

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Azure/taugrid/cli/internal/repository"
)

type SelectionOptions struct {
	ProjectName string
	ConfigPath  string
	CWD         string
	Target      string
}

type RunInput struct {
	ConfigPath     string
	ExplicitConfig bool
	BuiltinSmoke   bool
}

type ExplicitConfig struct {
	Original string
	Lexical  string
	Resolved string
}

// SelectProject applies submission project precedence without selecting a
// connection or interpreting a workload name.
func (c *Catalog) SelectProject(options SelectionOptions) (*Project, error) {
	explicitName := strings.TrimSpace(options.ProjectName)
	var explicitProject *Project
	if explicitName != "" {
		var ok bool
		explicitProject, ok = c.Projects[explicitName]
		if !ok {
			return nil, fmt.Errorf("unknown Tau project %q; valid projects: %s", explicitName, strings.Join(c.Names(), ", "))
		}
	}

	if strings.TrimSpace(options.ConfigPath) != "" {
		owner, err := c.ProjectForPath(options.ConfigPath)
		if err != nil {
			return nil, err
		}
		if explicitProject != nil && owner.Name != explicitProject.Name {
			return nil, fmt.Errorf(
				"--project %q does not own --config %s; config belongs to project %q",
				explicitProject.Name,
				options.ConfigPath,
				owner.Name,
			)
		}
		return owner, nil
	}
	if explicitProject != nil {
		return explicitProject, nil
	}

	if strings.TrimSpace(options.CWD) != "" {
		if owner, ok, err := c.containingProject(options.CWD, false); err != nil {
			return nil, err
		} else if ok {
			return owner, nil
		}
	}

	target := strings.TrimSpace(options.Target)
	if target != "" && target != "smoke" {
		var matches []*Project
		for _, name := range c.Names() {
			if _, ok := c.Projects[name].Targets[target]; ok {
				matches = append(matches, c.Projects[name])
			}
		}
		switch len(matches) {
		case 1:
			return matches[0], nil
		case 0:
		default:
			names := make([]string, 0, len(matches))
			for _, project := range matches {
				names = append(names, project.Name)
			}
			return nil, fmt.Errorf(
				"run target %q exists in projects: %s; rerun with --project <name>",
				target,
				strings.Join(names, ", "),
			)
		}
	}

	if len(c.Projects) == 1 {
		return c.Projects[c.Names()[0]], nil
	}
	return nil, fmt.Errorf("Tau project is ambiguous; rerun with --project <name>. Valid projects: %s", strings.Join(c.Names(), ", "))
}

// SelectLifecycleProject applies only explicit, containing-CWD, and one-project
// selection. Workload names are intentionally never treated as target names.
func (c *Catalog) SelectLifecycleProject(projectName, cwd string) (*Project, error) {
	projectName = strings.TrimSpace(projectName)
	if projectName != "" {
		project, ok := c.Projects[projectName]
		if !ok {
			return nil, fmt.Errorf("unknown Tau project %q; valid projects: %s", projectName, strings.Join(c.Names(), ", "))
		}
		return project, nil
	}
	if owner, ok, err := c.containingProject(cwd, false); err != nil {
		return nil, err
	} else if ok {
		return owner, nil
	}
	if len(c.Projects) == 1 {
		return c.Projects[c.Names()[0]], nil
	}
	return nil, fmt.Errorf("lifecycle routing is ambiguous; rerun with --project <name>. Valid projects: %s", strings.Join(c.Names(), ", "))
}

// ProjectForPath returns the unique project that lexically and physically owns
// an existing config path.
func (c *Catalog) ProjectForPath(path string) (*Project, error) {
	project, ok, err := c.containingProject(path, true)
	if err != nil {
		return nil, err
	}
	if ok {
		lexical, _, _, err := repository.ExistingPath(path)
		if err != nil {
			return nil, fmt.Errorf("resolve config path %s: %w", path, err)
		}
		relative, err := filepath.Rel(c.LexicalRoot, lexical)
		if err != nil {
			return nil, fmt.Errorf("resolve config path %s relative to catalog: %w", path, err)
		}
		if gitlink, found, err := c.gitlinks.AtOrAbove(relative); err != nil {
			return nil, fmt.Errorf("--config %s: %w", path, err)
		} else if found {
			return nil, fmt.Errorf("--config %s is at or beneath Git submodule %q", path, gitlink)
		}
		return project, nil
	}
	projectPaths := make([]string, 0, len(c.Projects))
	for _, name := range c.Names() {
		projectPaths = append(projectPaths, fmt.Sprintf("%s=%s", name, c.Projects[name].Path))
	}
	return nil, fmt.Errorf(
		"--config %s is not owned by any Tau project; known project paths: %s",
		path,
		strings.Join(projectPaths, ", "),
	)
}

func (c *Catalog) containingProject(path string, requireRegular bool) (*Project, bool, error) {
	lexical, realPath, info, err := repository.ExistingPath(path)
	if err != nil {
		return nil, false, fmt.Errorf("resolve path %s: %w", path, err)
	}
	if requireRegular && !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("config path %s is not a regular file", lexical)
	}
	if repository.Contains(c.LexicalRoot, lexical) {
		relative, err := filepath.Rel(c.LexicalRoot, lexical)
		if err != nil {
			return nil, false, fmt.Errorf("resolve path %s relative to catalog: %w", path, err)
		}
		if gitlink, found, err := c.gitlinks.AtOrAbove(relative); err != nil {
			return nil, false, fmt.Errorf("resolve path %s Git submodule ownership: %w", path, err)
		} else if found {
			return nil, false, fmt.Errorf("path %s is at or beneath Git submodule %q", path, gitlink)
		}
	}
	var matches []*Project
	for _, name := range c.Names() {
		project := c.Projects[name]
		physicalContained, err := repository.PathContains(project.Root, realPath)
		if err != nil {
			return nil, false, fmt.Errorf("compare path %s to project %q: %w", path, name, err)
		}
		if repository.Contains(project.LexicalRoot, lexical) && physicalContained {
			matches = append(matches, project)
		}
	}
	if len(matches) == 0 {
		return nil, false, nil
	}
	if len(matches) > 1 {
		names := make([]string, 0, len(matches))
		for _, project := range matches {
			names = append(names, project.Name)
		}
		sort.Strings(names)
		return nil, false, fmt.Errorf("path %s belongs to multiple Tau projects: %s", path, strings.Join(names, ", "))
	}
	return matches[0], true, nil
}

// ResolveInput chooses a config only after a project has been selected.
func (p *Project) ResolveInput(explicitConfig, target string) (RunInput, error) {
	if strings.TrimSpace(explicitConfig) != "" {
		return RunInput{ConfigPath: filepath.Clean(explicitConfig), ExplicitConfig: true}, nil
	}
	target = strings.TrimSpace(target)
	if target == "smoke" {
		return RunInput{BuiltinSmoke: true}, nil
	}
	if target != "" {
		configPath, ok := p.Targets[target]
		if !ok {
			return RunInput{}, fmt.Errorf(
				"run target %q not found in project %q; expected %s",
				target,
				p.Name,
				filepath.Join(p.LexicalRoot, "tau", target+".yaml"),
			)
		}
		return RunInput{ConfigPath: configPath}, nil
	}
	if p.DefaultConfigPath != "" {
		return RunInput{ConfigPath: p.DefaultConfigPath}, nil
	}
	return RunInput{}, fmt.Errorf(
		"no Tau run config found in project %q; create tau.yaml or pass --config",
		p.Name,
	)
}

// ResolveExplicitConfig validates and absolutizes --config while preserving the
// caller's original cleaned spelling for config-relative rendering behavior.
func ResolveExplicitConfig(cwd, config string) (ExplicitConfig, error) {
	if strings.TrimSpace(config) == "" {
		return ExplicitConfig{}, nil
	}
	original := filepath.Clean(config)
	checkPath := original
	if !filepath.IsAbs(checkPath) {
		checkPath = filepath.Join(cwd, checkPath)
	}
	lexical, resolved, info, err := repository.ExistingPath(checkPath)
	if err != nil {
		return ExplicitConfig{}, fmt.Errorf("--config %s: %w", config, err)
	}
	if !info.Mode().IsRegular() {
		return ExplicitConfig{}, fmt.Errorf("--config %s is not a regular file", config)
	}
	return ExplicitConfig{Original: original, Lexical: lexical, Resolved: resolved}, nil
}
