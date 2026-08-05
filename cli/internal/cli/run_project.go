package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/Azure/taugrid/cli/internal/projectcatalog"
	repositoryfs "github.com/Azure/taugrid/cli/internal/repository"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
)

type runConnectionSource struct {
	StartDir  string
	Discovery *workspaceconnection.Discovery
	Git       bool
	Catalog   bool
	Project   string
}

type runRequestResolution struct {
	Input      runInputDiscovery
	Repository projectcatalog.Repository
	Project    *projectcatalog.Project
	Connection runConnectionSource
}

func resolveRunRequest(
	cwd, projectName, explicitConfig, target string,
	allowProjectlessSmoke bool,
) (runRequestResolution, error) {
	if err := validateRunTargetName(target); err != nil {
		return runRequestResolution{}, err
	}
	explicit, err := projectcatalog.ResolveExplicitConfig(cwd, explicitConfig)
	if err != nil {
		return runRequestResolution{}, err
	}
	anchor := cwd
	if explicit.Resolved != "" {
		anchor = explicit.Resolved
	}
	resolvedRepository, err := projectcatalog.Discover(anchor)
	if err != nil {
		return runRequestResolution{}, err
	}
	repository := resolvedRepository
	if explicit.Lexical != "" {
		lexicalRepository, lexicalErr := projectcatalog.DiscoverLexicalConfig(explicit.Lexical)
		if lexicalErr != nil {
			return runRequestResolution{}, lexicalErr
		}
		gitRootsDiffer := false
		if resolvedRepository.Boundary.Git && lexicalRepository.Boundary.Git {
			sameRoot, err := repositoryfs.SamePath(
				resolvedRepository.Boundary.Root,
				lexicalRepository.Boundary.Root,
			)
			if err != nil {
				return runRequestResolution{}, fmt.Errorf("compare config Git worktrees: %w", err)
			}
			gitRootsDiffer = !sameRoot
		}
		switch {
		case gitRootsDiffer:
			return runRequestResolution{}, fmt.Errorf(
				"--config %s crosses Git worktrees between %s and %s",
				explicit.Original,
				lexicalRepository.Boundary.LexicalRoot,
				resolvedRepository.Boundary.LexicalRoot,
			)
		case resolvedRepository.Catalog != nil && lexicalRepository.Catalog != nil:
			repository = lexicalRepository
		case resolvedRepository.Catalog == nil && lexicalRepository.Catalog != nil:
			repository = lexicalRepository
		case resolvedRepository.Catalog == nil && lexicalRepository.Catalog == nil &&
			resolvedRepository.Boundary.Git != lexicalRepository.Boundary.Git:
			if resolvedRepository.Boundary.Git {
				repository = lexicalRepository
			} else {
				repository = resolvedRepository
			}
		case resolvedRepository.Catalog == nil && lexicalRepository.Catalog == nil:
			repository = lexicalRepository
		}
	}
	resolution := runRequestResolution{
		Repository: repository,
		Connection: runConnectionSource{
			StartDir: anchor,
			Git:      repository.Boundary.Git,
		},
	}

	if repository.Catalog == nil {
		if strings.TrimSpace(projectName) != "" {
			return runRequestResolution{}, fmt.Errorf(
				"--project requires %s at the Git worktree root",
				projectcatalog.Filename,
			)
		}
		input, err := discoverRunInput(cwd, explicit.Original, target)
		if err != nil {
			return runRequestResolution{}, err
		}
		resolution.Input = input
		if repository.Boundary.Git {
			if explicit.Resolved != "" {
				resolution.Connection.StartDir = explicit.Lexical
			} else if input.ConfigPath != "" {
				resolution.Connection.StartDir = input.ConfigPath
			}
		} else {
			resolution.Connection.StartDir = repository.Boundary.LexicalStartDir
			if resolution.Connection.StartDir == "" {
				resolution.Connection.StartDir = cwd
			}
		}
		return resolution, nil
	}

	resolution.Connection.Catalog = true
	if allowProjectlessSmoke &&
		strings.TrimSpace(projectName) == "" &&
		explicit.Lexical == "" &&
		strings.TrimSpace(target) == "smoke" {
		resolution.Input = runInputDiscovery{BuiltinSmoke: true}
		return resolution, nil
	}

	project, err := repository.Catalog.SelectProject(projectcatalog.SelectionOptions{
		ProjectName: projectName,
		ConfigPath:  explicit.Lexical,
		CWD:         cwd,
		Target:      target,
	})
	if err != nil {
		return runRequestResolution{}, err
	}
	input, err := project.ResolveInput(explicit.Original, target)
	if err != nil {
		return runRequestResolution{}, err
	}
	resolution.Project = project
	resolution.Connection.Project = project.Name
	resolution.Input = runInputDiscovery{
		ConfigPath:     input.ConfigPath,
		ExplicitConfig: input.ExplicitConfig,
		BuiltinSmoke:   input.BuiltinSmoke,
	}
	resolution.Connection.Discovery = &project.Connection
	if explicit.Resolved != "" {
		resolution.Connection.StartDir = explicit.Resolved
	} else if input.ConfigPath != "" {
		resolution.Connection.StartDir = input.ConfigPath
	} else {
		resolution.Connection.StartDir = project.LexicalRoot
	}
	return resolution, nil
}

func validateRunTargetName(target string) error {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil
	}
	if filepath.Base(target) != target || target == "." || target == ".." {
		return fmt.Errorf("run target %q must be a name, not a path; use --config for explicit paths", target)
	}
	return nil
}
