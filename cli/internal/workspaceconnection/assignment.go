// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package workspaceconnection

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
)

const AssignmentSchema = "tau.workspace.assignment.v1"

var ErrAssignmentNotFound = errors.New("Tau workspace assignment not found")

type AssignmentScope struct {
	RepositoryRoot     string
	RealRepositoryRoot string
	Project            string
	ProjectRoot        string
	RealProjectRoot    string
}

type Assignment struct {
	Schema             string     `json:"schema"`
	RepositoryRoot     string     `json:"repositoryRoot"`
	RealRepositoryRoot string     `json:"realRepositoryRoot"`
	Project            string     `json:"project,omitempty"`
	ProjectRoot        string     `json:"projectRoot"`
	RealProjectRoot    string     `json:"realProjectRoot"`
	Descriptor         Descriptor `json:"descriptor"`
	WorkspaceUID       string     `json:"workspaceUID,omitempty"`
	Source             string     `json:"source"`
	AssignedAt         time.Time  `json:"assignedAt"`
}

func (s AssignmentScope) Validate() error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{"repository root", s.RepositoryRoot},
		{"real repository root", s.RealRepositoryRoot},
		{"project root", s.ProjectRoot},
		{"real project root", s.RealProjectRoot},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("assignment %s is required", field.name)
		}
		if !filepath.IsAbs(field.value) {
			return fmt.Errorf("assignment %s must be absolute", field.name)
		}
	}
	repositoryRoot := filepath.Clean(s.RepositoryRoot)
	realRepositoryRoot := filepath.Clean(s.RealRepositoryRoot)
	projectRoot := filepath.Clean(s.ProjectRoot)
	realProjectRoot := filepath.Clean(s.RealProjectRoot)
	if !pathWithin(repositoryRoot, projectRoot) {
		return fmt.Errorf("assignment project root %s is outside repository root %s", projectRoot, repositoryRoot)
	}
	if !pathWithin(realRepositoryRoot, realProjectRoot) {
		return fmt.Errorf("assignment real project root %s is outside real repository root %s", realProjectRoot, realRepositoryRoot)
	}
	if containsControlCharacter(s.Project) {
		return fmt.Errorf("assignment project must not contain control characters")
	}
	return nil
}

func pathWithin(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func AssignmentPath(configDir string, scope AssignmentScope) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(configDir) == "" {
		var err error
		configDir, err = DefaultConfigDir()
		if err != nil {
			return "", err
		}
	}
	identity := filepath.Clean(scope.RealRepositoryRoot) + "\x00" +
		strings.TrimSpace(scope.Project) + "\x00" +
		filepath.Clean(scope.RealProjectRoot)
	sum := sha256.Sum256([]byte(identity))
	name := safeFilename(scope.Project)
	if strings.TrimSpace(scope.Project) == "" {
		name = "repository"
	}
	return filepath.Join(
		filepath.Clean(configDir),
		"assignments",
		name+"-"+hex.EncodeToString(sum[:8])+".json",
	), nil
}

func LoadAssignment(configDir string, scope AssignmentScope) (Assignment, string, error) {
	path, err := AssignmentPath(configDir, scope)
	if err != nil {
		return Assignment{}, "", err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Assignment{}, path, ErrAssignmentNotFound
	}
	if err != nil {
		return Assignment{}, path, fmt.Errorf("read Tau workspace assignment %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var assignment Assignment
	if err := decoder.Decode(&assignment); err != nil {
		return Assignment{}, path, fmt.Errorf("parse Tau workspace assignment %s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != nil {
		if !errors.Is(err, io.EOF) {
			return Assignment{}, path, fmt.Errorf("parse Tau workspace assignment %s: %w", path, err)
		}
	} else {
		return Assignment{}, path, fmt.Errorf("parse Tau workspace assignment %s: multiple JSON values are not allowed", path)
	}
	if err := assignment.validate(scope); err != nil {
		return Assignment{}, path, fmt.Errorf("validate Tau workspace assignment %s: %w", path, err)
	}
	return assignment, path, nil
}

func (a Assignment) validate(scope AssignmentScope) error {
	if a.Schema != AssignmentSchema {
		return fmt.Errorf("schema %q is unsupported; expected %q", a.Schema, AssignmentSchema)
	}
	if err := scope.Validate(); err != nil {
		return err
	}
	if filepath.Clean(a.RepositoryRoot) != filepath.Clean(scope.RepositoryRoot) ||
		filepath.Clean(a.RealRepositoryRoot) != filepath.Clean(scope.RealRepositoryRoot) ||
		strings.TrimSpace(a.Project) != strings.TrimSpace(scope.Project) ||
		filepath.Clean(a.ProjectRoot) != filepath.Clean(scope.ProjectRoot) ||
		filepath.Clean(a.RealProjectRoot) != filepath.Clean(scope.RealProjectRoot) {
		return fmt.Errorf("assignment repository or project identity does not match the current worktree")
	}
	if err := a.Descriptor.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(a.Source) == "" {
		return fmt.Errorf("assignment source is required")
	}
	if a.AssignedAt.IsZero() {
		return fmt.Errorf("assignment timestamp is required")
	}
	return nil
}

func SaveAssignment(
	configDir string,
	scope AssignmentScope,
	descriptor Descriptor,
	workspaceUID string,
	source string,
	replace bool,
	now time.Time,
) (Assignment, string, error) {
	if err := scope.Validate(); err != nil {
		return Assignment{}, "", err
	}
	if err := descriptor.Validate(); err != nil {
		return Assignment{}, "", err
	}
	if strings.TrimSpace(source) == "" {
		return Assignment{}, "", fmt.Errorf("assignment source is required")
	}
	path, err := AssignmentPath(configDir, scope)
	if err != nil {
		return Assignment{}, "", err
	}
	if existing, _, loadErr := LoadAssignment(configDir, scope); loadErr == nil {
		existingDigest, digestErr := Digest(existing.Descriptor)
		if digestErr != nil {
			return Assignment{}, "", digestErr
		}
		nextDigest, digestErr := Digest(descriptor)
		if digestErr != nil {
			return Assignment{}, "", digestErr
		}
		if existingDigest == nextDigest && existing.WorkspaceUID == strings.TrimSpace(workspaceUID) {
			return existing, path, nil
		}
		if !replace {
			return Assignment{}, path, fmt.Errorf(
				"project already has a local workspace assignment to %q; rerun with --replace",
				existing.Descriptor.Workspace,
			)
		}
	} else if !errors.Is(loadErr, ErrAssignmentNotFound) {
		return Assignment{}, path, loadErr
	}
	assignment := Assignment{
		Schema:             AssignmentSchema,
		RepositoryRoot:     filepath.Clean(scope.RepositoryRoot),
		RealRepositoryRoot: filepath.Clean(scope.RealRepositoryRoot),
		Project:            strings.TrimSpace(scope.Project),
		ProjectRoot:        filepath.Clean(scope.ProjectRoot),
		RealProjectRoot:    filepath.Clean(scope.RealProjectRoot),
		Descriptor:         descriptor,
		WorkspaceUID:       strings.TrimSpace(workspaceUID),
		Source:             strings.TrimSpace(source),
		AssignedAt:         now.UTC(),
	}
	raw, err := json.MarshalIndent(assignment, "", "  ")
	if err != nil {
		return Assignment{}, path, fmt.Errorf("encode Tau workspace assignment: %w", err)
	}
	raw = append(raw, '\n')
	if err := fileutil.WriteFileAtomic(path, raw, 0o600); err != nil {
		return Assignment{}, path, fmt.Errorf("write Tau workspace assignment %s: %w", path, err)
	}
	return assignment, path, nil
}

func ClearAssignment(configDir string, scope AssignmentScope) (string, error) {
	path, err := AssignmentPath(configDir, scope)
	if err != nil {
		return "", err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return path, ErrAssignmentNotFound
	} else if err != nil {
		return path, fmt.Errorf("remove Tau workspace assignment %s: %w", path, err)
	}
	return path, nil
}

func AssignmentDiscovery(configDir string, scope AssignmentScope) (Discovery, Assignment, string, error) {
	assignment, path, err := LoadAssignment(configDir, scope)
	if err != nil {
		return Discovery{}, Assignment{}, path, err
	}
	realPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return Discovery{}, Assignment{}, path, fmt.Errorf("resolve Tau workspace assignment %s: %w", path, err)
	}
	digest, err := Digest(assignment.Descriptor)
	if err != nil {
		return Discovery{}, Assignment{}, path, err
	}
	return Discovery{
		Path:               path,
		RealPath:           realPath,
		RepositoryRoot:     filepath.Clean(scope.RepositoryRoot),
		RealRepositoryRoot: filepath.Clean(scope.RealRepositoryRoot),
		Descriptor:         assignment.Descriptor,
		Digest:             digest,
	}, assignment, path, nil
}
