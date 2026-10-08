// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/cli/internal/projectcatalog"
	"github.com/Azure/taugrid/cli/internal/workspace"
	"github.com/Azure/taugrid/cli/internal/workspaceconnection"
	"github.com/Azure/taugrid/core/kube"
)

type workspaceConnectionTarget struct {
	Project   string
	Scope     workspaceconnection.AssignmentScope
	CheckedIn *workspaceconnection.Discovery
}

type workspaceAssignmentCandidate struct {
	Descriptor   workspaceconnection.Descriptor
	WorkspaceUID string
	Source       string
	VerifiedAt   time.Time
}

type workspaceConnectionInspection struct {
	Project          string                                      `json:"project,omitempty"`
	Source           string                                      `json:"source"`
	Location         string                                      `json:"location"`
	Workspace        string                                      `json:"workspace"`
	WorkspaceUID     string                                      `json:"workspaceUID,omitempty"`
	Context          string                                      `json:"context"`
	SystemNamespace  string                                      `json:"systemNamespace"`
	AccessMethod     workspaceconnection.AccessMethod            `json:"accessMethod"`
	Authorization    workspaceconnection.AuthorizationDescriptor `json:"authorization"`
	LocalShadowed    bool                                        `json:"localAssignmentShadowed"`
	ShadowedLocation string                                      `json:"shadowedLocation,omitempty"`
}

var listWorkspaceAssignmentCandidates = workspaceconnection.ListAssignableConnections

func resolveWorkspaceConnectionTarget(start, projectName string) (workspaceConnectionTarget, error) {
	repository, err := projectcatalog.Discover(start)
	if err != nil {
		return workspaceConnectionTarget{}, err
	}
	if repository.Catalog == nil {
		if strings.TrimSpace(projectName) != "" {
			return workspaceConnectionTarget{}, fmt.Errorf(
				"--project requires %s at the Git worktree root",
				projectcatalog.Filename,
			)
		}
		target := workspaceConnectionTarget{
			Scope: workspaceconnection.AssignmentScope{
				RepositoryRoot:     repository.Boundary.LexicalRoot,
				RealRepositoryRoot: repository.Boundary.Root,
				ProjectRoot:        repository.Boundary.LexicalRoot,
				RealProjectRoot:    repository.Boundary.Root,
			},
		}
		if discovery, discoverErr := workspaceconnection.Discover(start); discoverErr == nil {
			target.CheckedIn = &discovery
		} else if !errors.Is(discoverErr, workspaceconnection.ErrDescriptorNotFound) {
			return workspaceConnectionTarget{}, discoverErr
		}
		return target, nil
	}
	project, err := repository.Catalog.SelectLifecycleProject(projectName, start)
	if err != nil {
		if strings.TrimSpace(projectName) == "" {
			return workspaceConnectionTarget{}, fmt.Errorf("%w; pass a path inside the intended project", err)
		}
		return workspaceConnectionTarget{}, err
	}
	target := workspaceConnectionTarget{
		Project: project.Name,
		Scope: workspaceconnection.AssignmentScope{
			RepositoryRoot:     repository.Catalog.LexicalRoot,
			RealRepositoryRoot: repository.Catalog.Root,
			Project:            project.Name,
			ProjectRoot:        project.LexicalRoot,
			RealProjectRoot:    project.Root,
		},
	}
	if discovery, found, err := repository.Catalog.ResolveProjectConnection(project); err != nil {
		return workspaceConnectionTarget{}, err
	} else if found {
		target.CheckedIn = &discovery
	}
	return target, nil
}

func workspaceConnectionConfigDir() (string, error) {
	if configured := strings.TrimSpace(os.Getenv("TAU_CONFIG_DIR")); configured != "" {
		return filepath.Clean(configured), nil
	}
	return workspaceconnection.DefaultConfigDir()
}

func effectiveWorkspaceConnection(start, projectName string) (workspaceConnectionTarget, workspaceconnection.Discovery, string, error) {
	target, err := resolveWorkspaceConnectionTarget(start, projectName)
	if err != nil {
		return workspaceConnectionTarget{}, workspaceconnection.Discovery{}, "", err
	}
	if target.CheckedIn != nil {
		return target, *target.CheckedIn, "checked-in", nil
	}
	configDir, err := workspaceConnectionConfigDir()
	if err != nil {
		return workspaceConnectionTarget{}, workspaceconnection.Discovery{}, "", err
	}
	discovery, _, _, err := workspaceconnection.AssignmentDiscovery(configDir, target.Scope)
	if err != nil {
		if errors.Is(err, workspaceconnection.ErrAssignmentNotFound) {
			projectFlag := ""
			if target.Project != "" {
				projectFlag = " --project " + target.Project
			}
			return workspaceConnectionTarget{}, workspaceconnection.Discovery{}, "", fmt.Errorf(
				"%w: no checked-in descriptor or local assignment governs this project; run `tau workspace connection assign%s`",
				workspaceconnection.ErrDescriptorNotFound,
				projectFlag,
			)
		}
		return workspaceConnectionTarget{}, workspaceconnection.Discovery{}, "", err
	}
	return target, discovery, "local assignment", nil
}

func assignedCatalogProjectConnection(
	catalog *projectcatalog.Catalog,
	project *projectcatalog.Project,
) (*workspaceconnection.Discovery, error) {
	if discovery, found, err := catalog.ResolveProjectConnection(project); err != nil {
		return nil, err
	} else if found {
		return &discovery, nil
	}
	configDir, err := workspaceConnectionConfigDir()
	if err != nil {
		return nil, err
	}
	scope := workspaceconnection.AssignmentScope{
		RepositoryRoot:     catalog.LexicalRoot,
		RealRepositoryRoot: catalog.Root,
		Project:            project.Name,
		ProjectRoot:        project.LexicalRoot,
		RealProjectRoot:    project.Root,
	}
	discovery, _, _, err := workspaceconnection.AssignmentDiscovery(configDir, scope)
	if err != nil {
		if errors.Is(err, workspaceconnection.ErrAssignmentNotFound) {
			return nil, fmt.Errorf(
				"%w: project %q has no checked-in connection or local assignment; run `tau workspace connection assign --project %s`",
				workspaceconnection.ErrDescriptorNotFound,
				project.Name,
				project.Name,
			)
		}
		return nil, err
	}
	return &discovery, nil
}

func newWorkspaceConnectionAssignCmd() *cobra.Command {
	var path, projectName, kubeContext, systemNamespace string
	var current, replace bool
	cmd := &cobra.Command{
		Use:   "assign [WORKSPACE]",
		Short: "Assign a known workspace to this local project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			start := path
			if strings.TrimSpace(start) == "" {
				var err error
				start, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			target, err := resolveWorkspaceConnectionTarget(start, projectName)
			if err != nil {
				return err
			}
			if target.CheckedIn != nil {
				return fmt.Errorf(
					"project is governed by checked-in workspace connection %s; local assignment cannot override it",
					displayConnectionPath(*target.CheckedIn),
				)
			}
			configDir, err := workspaceConnectionConfigDir()
			if err != nil {
				return err
			}
			workspaceName := ""
			if len(args) == 1 {
				workspaceName = strings.TrimSpace(args[0])
			}
			candidate, err := selectWorkspaceAssignmentCandidate(
				cmd,
				configDir,
				workspaceName,
				strings.TrimSpace(kubeContext),
				strings.TrimSpace(systemNamespace),
				current,
				cmd.Flags().Changed("context"),
			)
			if err != nil {
				return err
			}
			assignment, assignmentPath, err := workspaceconnection.SaveAssignment(
				configDir,
				target.Scope,
				candidate.Descriptor,
				candidate.WorkspaceUID,
				candidate.Source,
				replace,
				time.Now(),
			)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Assigned.")
			if target.Project != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Project:       %s\n", target.Project)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Workspace:     %s\n", assignment.Descriptor.Workspace)
			fmt.Fprintf(cmd.OutOrStdout(), "Context:       %s\n", assignment.Descriptor.Cluster.ContextName)
			fmt.Fprintf(cmd.OutOrStdout(), "Source:        %s\n", assignment.Source)
			fmt.Fprintf(cmd.OutOrStdout(), "Assignment:    %s\n", assignmentPath)
			fmt.Fprintln(cmd.OutOrStdout(), "Ready:         tau workspace connection")
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "repository or project path to assign (default: current directory)")
	cmd.Flags().StringVar(&projectName, "project", "", "Tau project name in tau.projects.yaml")
	cmd.Flags().StringVar(&kubeContext, "context", defaultKubeContext(), "select candidates from this Kubernetes context")
	cmd.Flags().StringVar(&systemNamespace, "system-namespace", defaultSystemNamespace(), systemNamespaceHelp())
	cmd.Flags().BoolVar(&current, "current", false, "reuse the last repository workspace connection Tau activated")
	cmd.Flags().BoolVar(&replace, "replace", false, "replace an existing local assignment")
	return cmd
}

func selectWorkspaceAssignmentCandidate(
	cmd *cobra.Command,
	configDir, workspaceName, kubeContext, systemNamespace string,
	current bool,
	contextExplicit bool,
) (workspaceAssignmentCandidate, error) {
	cached, err := listWorkspaceAssignmentCandidates(configDir)
	if err != nil {
		return workspaceAssignmentCandidate{}, err
	}
	candidates := make([]workspaceAssignmentCandidate, 0, len(cached))
	for _, connection := range cached {
		candidates = append(candidates, workspaceAssignmentCandidate{
			Descriptor:   connection.Descriptor,
			WorkspaceUID: connection.WorkspaceUID,
			Source:       "cached:" + connection.DescriptorPath,
			VerifiedAt:   connection.VerifiedAt,
		})
	}
	if current {
		if workspaceName != "" || contextExplicit {
			return workspaceAssignmentCandidate{}, fmt.Errorf("--current cannot be combined with WORKSPACE or --context")
		}
		return currentWorkspaceAssignmentCandidate(configDir, candidates)
	}
	matches := filterWorkspaceAssignmentCandidates(candidates, workspaceName, kubeContext)
	if len(matches) == 0 {
		live, liveErr := discoverLiveWorkspaceAssignmentCandidates(
			cmd.Context(),
			workspaceName,
			kubeContext,
			systemNamespace,
		)
		if liveErr != nil {
			if len(candidates) == 0 {
				return workspaceAssignmentCandidate{}, liveErr
			}
		} else {
			candidates = append(candidates, live...)
			matches = filterWorkspaceAssignmentCandidates(candidates, workspaceName, kubeContext)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) == 0 {
		if workspaceName == "" {
			return workspaceAssignmentCandidate{}, fmt.Errorf(
				"no assignable Tau workspace connection was found; connect another Tau repository or pass --context for a visible workspace",
			)
		}
		return workspaceAssignmentCandidate{}, fmt.Errorf(
			"no assignable Tau workspace %q was found%s",
			workspaceName,
			contextFlag(kubeContext),
		)
	}
	if workspaceName != "" || !stdinIsTerminal(cmd.InOrStdin()) {
		return workspaceAssignmentCandidate{}, fmt.Errorf(
			"workspace selection is ambiguous: %s; pass --context to select one target",
			describeWorkspaceAssignmentCandidates(matches),
		)
	}
	return promptWorkspaceAssignmentCandidate(cmd, matches)
}

func filterWorkspaceAssignmentCandidates(
	candidates []workspaceAssignmentCandidate,
	workspaceName, kubeContext string,
) []workspaceAssignmentCandidate {
	var matches []workspaceAssignmentCandidate
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if workspaceName != "" && candidate.Descriptor.Workspace != workspaceName {
			continue
		}
		if kubeContext != "" && candidate.Descriptor.Cluster.ContextName != kubeContext {
			continue
		}
		digest, err := workspaceconnection.Digest(candidate.Descriptor)
		if err != nil {
			continue
		}
		key := candidate.Descriptor.Workspace + "\x00" +
			candidate.WorkspaceUID + "\x00" +
			candidate.Descriptor.Cluster.ContextName + "\x00" +
			digest
		if seen[key] {
			continue
		}
		seen[key] = true
		matches = append(matches, candidate)
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Descriptor.Workspace != matches[j].Descriptor.Workspace {
			return matches[i].Descriptor.Workspace < matches[j].Descriptor.Workspace
		}
		return matches[i].Descriptor.Cluster.ContextName < matches[j].Descriptor.Cluster.ContextName
	})
	return matches
}

func currentWorkspaceAssignmentCandidate(configDir string, candidates []workspaceAssignmentCandidate) (workspaceAssignmentCandidate, error) {
	raw, err := os.ReadFile(filepath.Join(configDir, activeWorkspaceCacheFilename))
	if err != nil {
		return workspaceAssignmentCandidate{}, fmt.Errorf("read current Tau workspace connection: %w", err)
	}
	var active activeWorkspaceCache
	if err := json.Unmarshal(raw, &active); err != nil {
		return workspaceAssignmentCandidate{}, fmt.Errorf("parse current Tau workspace connection: %w", err)
	}
	if active.Schema != activeWorkspaceCacheSchema {
		return workspaceAssignmentCandidate{}, fmt.Errorf("current Tau workspace connection uses unsupported schema %q", active.Schema)
	}
	var selected *workspaceAssignmentCandidate
	selectedDigest := ""
	for _, candidate := range candidates {
		if candidate.Descriptor.Workspace == active.Workspace &&
			candidate.Descriptor.Cluster.ContextName == active.ContextName &&
			(active.WorkspaceUID == "" || candidate.WorkspaceUID == active.WorkspaceUID) {
			digest, err := workspaceconnection.Digest(candidate.Descriptor)
			if err != nil {
				continue
			}
			if selected == nil {
				copy := candidate
				selected = &copy
				selectedDigest = digest
				continue
			}
			if digest != selectedDigest {
				return workspaceAssignmentCandidate{}, fmt.Errorf(
					"current Tau workspace connection maps to multiple descriptor contracts; pass WORKSPACE and --context explicitly",
				)
			}
		}
	}
	if selected == nil {
		return workspaceAssignmentCandidate{}, fmt.Errorf(
			"current Tau workspace connection cannot be mapped to one assignable descriptor; reconnect its source repository first",
		)
	}
	return *selected, nil
}

func discoverLiveWorkspaceAssignmentCandidates(
	ctx context.Context,
	workspaceName,
	kubeContext, systemNamespace string,
) ([]workspaceAssignmentCandidate, error) {
	runner := kube.New(kubeContext)
	if kubeContext == "" {
		currentContext, err := runner.Raw(ctx, []string{"config", "current-context"}, nil)
		if err != nil {
			return nil, fmt.Errorf("resolve current Kubernetes context: %w", err)
		}
		kubeContext = strings.TrimSpace(currentContext)
		if kubeContext == "" {
			return nil, fmt.Errorf("current Kubernetes context is empty")
		}
		runner = kube.New(kubeContext)
	}
	var items []workspace.Workspace
	if workspaceName != "" {
		raw, err := runner.Raw(ctx, []string{
			"-n", systemNamespace,
			"get", "workspace.tau.azure.com", workspaceName,
			"-o", "json",
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("discover Tau workspace %q in context %q: %w", workspaceName, kubeContext, err)
		}
		item, err := workspace.Parse([]byte(raw))
		if err != nil {
			return nil, err
		}
		items = []workspace.Workspace{item}
	} else {
		raw, err := runner.Raw(ctx, []string{
			"-n", systemNamespace,
			"get", "workspaces.tau.azure.com",
			"-o", "json",
		}, nil)
		if err != nil {
			return nil, fmt.Errorf("discover Tau workspaces in context %q: %w", kubeContext, err)
		}
		list, err := workspace.ParseList([]byte(raw))
		if err != nil {
			return nil, err
		}
		items = list.Items
	}
	candidates := make([]workspaceAssignmentCandidate, 0, len(items))
	for _, item := range items {
		mode := workspace.AuthorizationModeWorkspaceRBAC
		if item.Spec.Authorization != nil && strings.TrimSpace(item.Spec.Authorization.Mode) != "" {
			mode = strings.TrimSpace(item.Spec.Authorization.Mode)
		}
		requiredRole := ""
		if mode == workspace.AuthorizationModeWorkspaceRBAC {
			requiredRole = strings.TrimSpace(item.Spec.Role)
			if requiredRole == "" {
				requiredRole = workspace.DefaultResearcherRole
			}
		}
		descriptor := workspaceconnection.Descriptor{
			Schema:    workspaceconnection.DescriptorSchema,
			Workspace: item.Metadata.Name,
			Cluster: workspaceconnection.ClusterDescriptor{
				ContextName:     kubeContext,
				SystemNamespace: systemNamespace,
			},
			Access: workspaceconnection.AccessDescriptor{
				Method: workspaceconnection.AccessMethodKubeconfig,
			},
			Authorization: workspaceconnection.AuthorizationDescriptor{
				Mode:         mode,
				RequiredRole: requiredRole,
			},
			Requirements: workspaceconnection.RequirementsDescriptor{MinTauVersion: "0.3.0"},
			Network:      workspaceconnection.NetworkDescriptor{PrivateCluster: false},
		}
		if err := descriptor.Validate(); err != nil {
			continue
		}
		verification, err := (workspaceconnection.KubectlVerifier{}).Verify(ctx, descriptor, "")
		if err != nil {
			continue
		}
		candidates = append(candidates, workspaceAssignmentCandidate{
			Descriptor:   descriptor,
			WorkspaceUID: verification.WorkspaceUID,
			Source:       "context:" + kubeContext,
		})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("context %q has no visible Ready Tau workspace usable by this identity", kubeContext)
	}
	return candidates, nil
}

func describeWorkspaceAssignmentCandidates(candidates []workspaceAssignmentCandidate) string {
	values := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		values = append(values, fmt.Sprintf(
			"%s@%s",
			candidate.Descriptor.Workspace,
			candidate.Descriptor.Cluster.ContextName,
		))
	}
	return strings.Join(values, ", ")
}

func promptWorkspaceAssignmentCandidate(cmd *cobra.Command, candidates []workspaceAssignmentCandidate) (workspaceAssignmentCandidate, error) {
	fmt.Fprintln(cmd.OutOrStdout(), "Select a Tau workspace:")
	for index, candidate := range candidates {
		fmt.Fprintf(
			cmd.OutOrStdout(),
			"  %d. %s (%s)\n",
			index+1,
			candidate.Descriptor.Workspace,
			candidate.Descriptor.Cluster.ContextName,
		)
	}
	fmt.Fprint(cmd.OutOrStdout(), "Selection: ")
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil {
		return workspaceAssignmentCandidate{}, fmt.Errorf("read workspace selection: %w", err)
	}
	selected, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || selected < 1 || selected > len(candidates) {
		return workspaceAssignmentCandidate{}, fmt.Errorf("workspace selection must be a number from 1 to %d", len(candidates))
	}
	return candidates[selected-1], nil
}

func newWorkspaceConnectionInspectCmd() *cobra.Command {
	var path, projectName, output string
	cmd := &cobra.Command{
		Use:   "inspect",
		Short: "Show the effective workspace connection for this project",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := path
			if strings.TrimSpace(start) == "" {
				var err error
				start, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			target, discovery, source, err := effectiveWorkspaceConnection(start, projectName)
			if err != nil {
				return err
			}
			inspection := workspaceConnectionInspection{
				Project:         target.Project,
				Source:          source,
				Location:        displayConnectionPath(discovery),
				Workspace:       discovery.Descriptor.Workspace,
				Context:         discovery.Descriptor.Cluster.ContextName,
				SystemNamespace: discovery.Descriptor.ResolvedSystemNamespace(),
				AccessMethod:    discovery.Descriptor.Access.Method,
				Authorization:   discovery.Descriptor.Authorization,
			}
			configDir, err := workspaceConnectionConfigDir()
			if err != nil {
				return err
			}
			if assignment, assignmentPath, assignmentErr := workspaceconnection.LoadAssignment(configDir, target.Scope); assignmentErr == nil {
				inspection.WorkspaceUID = assignment.WorkspaceUID
				if target.CheckedIn != nil {
					inspection.LocalShadowed = true
					inspection.ShadowedLocation = assignmentPath
				}
			} else if !errors.Is(assignmentErr, workspaceconnection.ErrAssignmentNotFound) {
				return assignmentErr
			}
			switch output {
			case "table":
				if inspection.Project != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Project:       %s\n", inspection.Project)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Source:        %s\n", inspection.Source)
				fmt.Fprintf(cmd.OutOrStdout(), "Location:      %s\n", inspection.Location)
				fmt.Fprintf(cmd.OutOrStdout(), "Workspace:     %s\n", inspection.Workspace)
				if inspection.WorkspaceUID != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "Workspace UID: %s\n", inspection.WorkspaceUID)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Context:       %s\n", inspection.Context)
				fmt.Fprintf(cmd.OutOrStdout(), "Access:        %s\n", inspection.AccessMethod)
				fmt.Fprintf(cmd.OutOrStdout(), "Authorization: %s\n", inspection.Authorization.Mode)
				if inspection.LocalShadowed {
					fmt.Fprintf(cmd.OutOrStdout(), "Shadowed:      %s\n", inspection.ShadowedLocation)
				}
			case "json":
				encoder := json.NewEncoder(cmd.OutOrStdout())
				encoder.SetIndent("", "  ")
				return encoder.Encode(inspection)
			default:
				return fmt.Errorf("-o/--output must be one of: table, json")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "repository or project path to inspect (default: current directory)")
	cmd.Flags().StringVar(&projectName, "project", "", "Tau project name in tau.projects.yaml")
	cmd.Flags().StringVarP(&output, "output", "o", "table", "output format: table|json")
	return cmd
}

func newWorkspaceConnectionClearCmd() *cobra.Command {
	var path, projectName string
	var yes bool
	cmd := &cobra.Command{
		Use:   "clear",
		Short: "Remove this project's local workspace assignment",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			start := path
			if strings.TrimSpace(start) == "" {
				var err error
				start, err = os.Getwd()
				if err != nil {
					return err
				}
			}
			target, err := resolveWorkspaceConnectionTarget(start, projectName)
			if err != nil {
				return err
			}
			configDir, err := workspaceConnectionConfigDir()
			if err != nil {
				return err
			}
			assignment, assignmentPath, err := workspaceconnection.LoadAssignment(configDir, target.Scope)
			if err != nil {
				return err
			}
			if !yes {
				if !stdinIsTerminal(cmd.InOrStdin()) {
					return fmt.Errorf("clearing a local workspace assignment non-interactively requires --yes")
				}
				fmt.Fprintf(
					cmd.OutOrStdout(),
					"Clear local assignment to workspace %q? [y/N] ",
					assignment.Descriptor.Workspace,
				)
				line, readErr := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
				if readErr != nil {
					return fmt.Errorf("read clear confirmation: %w", readErr)
				}
				if answer := strings.ToLower(strings.TrimSpace(line)); answer != "y" && answer != "yes" {
					return fmt.Errorf("workspace assignment was not cleared")
				}
			}
			if _, err := workspaceconnection.ClearAssignment(configDir, target.Scope); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Cleared local workspace assignment %s.\n", assignmentPath)
			if target.CheckedIn != nil {
				fmt.Fprintf(
					cmd.OutOrStdout(),
					"Checked-in connection remains active: %s\n",
					displayConnectionPath(*target.CheckedIn),
				)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "path", "", "repository or project path to clear (default: current directory)")
	cmd.Flags().StringVar(&projectName, "project", "", "Tau project name in tau.projects.yaml")
	cmd.Flags().BoolVar(&yes, "yes", false, "confirm removal without prompting")
	return cmd
}
