// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package repository

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
)

type Boundary struct {
	LexicalRoot     string
	Root            string
	LexicalStartDir string
	StartDir        string
	Git             bool
}

func Resolve(start string) (Boundary, error) {
	if strings.TrimSpace(start) == "" {
		var err error
		start, err = os.Getwd()
		if err != nil {
			return Boundary{}, fmt.Errorf("get current directory: %w", err)
		}
	}

	lexicalPath, realPath, info, err := ExistingPath(start)
	if err != nil {
		return Boundary{}, fmt.Errorf("resolve repository search path: %w", err)
	}
	lexicalStartDir := lexicalPath
	startDir := realPath
	if !info.IsDir() {
		lexicalStartDir = filepath.Dir(lexicalPath)
		startDir = filepath.Dir(realPath)
	}

	cmd := gitCommand(lexicalStartDir, "rev-parse", "--show-toplevel")
	output, err := cmd.CombinedOutput()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && isNotRepositoryError(string(output)) {
			return Boundary{
				LexicalRoot:     lexicalStartDir,
				Root:            startDir,
				LexicalStartDir: lexicalStartDir,
				StartDir:        startDir,
			}, nil
		}

		return Boundary{}, fmt.Errorf("resolve Git worktree from %s: %w: %s", startDir, err, strings.TrimSpace(string(output)))
	}

	root := strings.TrimSpace(string(output))
	if root == "" {
		return Boundary{}, fmt.Errorf("resolve Git worktree from %s: git returned an empty root", startDir)
	}
	_, realRoot, rootInfo, err := ExistingPath(root)
	if err != nil {
		return Boundary{}, fmt.Errorf("resolve Git worktree root %s: %w", root, err)
	}
	if !rootInfo.IsDir() {
		return Boundary{}, fmt.Errorf("resolve Git worktree root %s: not a directory", root)
	}
	contained, err := PathContains(realRoot, startDir)
	if err != nil {
		return Boundary{}, fmt.Errorf("compare Git worktree root %s to %s: %w", realRoot, startDir, err)
	}
	if !contained {
		return Boundary{}, fmt.Errorf("Git worktree root %s does not contain %s", realRoot, startDir)
	}
	lexicalRoot, err := findLexicalRoot(lexicalStartDir, realRoot)
	if err != nil {
		return Boundary{}, err
	}
	return Boundary{
		LexicalRoot:     filepath.Clean(lexicalRoot),
		Root:            realRoot,
		LexicalStartDir: lexicalStartDir,
		StartDir:        startDir,
		Git:             true,
	}, nil
}

type GitlinkIndex struct {
	root  string
	paths []string
}

func LoadGitlinks(root string) (GitlinkIndex, error) {
	cmd := gitCommand(root, "ls-files", "--stage", "-z")
	output, err := cmd.Output()
	if err != nil {
		return GitlinkIndex{}, fmt.Errorf("inspect Git index for submodules: %w", err)
	}
	index := GitlinkIndex{root: root}
	for _, record := range strings.Split(string(output), "\x00") {
		if record == "" {
			continue
		}
		metadata, indexedPath, ok := strings.Cut(record, "\t")
		if !ok {
			return GitlinkIndex{}, fmt.Errorf("inspect Git index for submodules: malformed ls-files record")
		}
		fields := strings.Fields(metadata)
		if len(fields) < 1 || fields[0] != "160000" {
			continue
		}
		index.paths = append(index.paths, path.Clean(indexedPath))
	}
	return index, nil
}

func (index GitlinkIndex) AtOrAbove(relativePath string) (string, bool, error) {
	relativePath = path.Clean(filepath.ToSlash(relativePath))
	for _, indexedPath := range index.paths {
		if relativePath == indexedPath || strings.HasPrefix(relativePath, indexedPath+"/") {
			return indexedPath, true, nil
		}
	}
	_, resolvedCandidate, _, candidateErr := ExistingPath(filepath.Join(index.root, filepath.FromSlash(relativePath)))
	if candidateErr != nil {
		return "", false, nil
	}
	for _, gitlink := range index.paths {
		gitlinkPath := filepath.Join(index.root, filepath.FromSlash(gitlink))
		gitlinkInfo, err := os.Stat(gitlinkPath)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return "", false, fmt.Errorf("inspect Git submodule path %s: %w", gitlinkPath, err)
		}
		contains, err := fileAncestorContains(gitlinkInfo, resolvedCandidate)
		if err != nil {
			return "", false, fmt.Errorf("compare Git submodule path %s: %w", gitlinkPath, err)
		}
		if contains {
			return gitlink, true, nil
		}
	}
	return "", false, nil
}

func GitlinkAtOrAbove(root, relativePath string) (string, bool, error) {
	index, err := LoadGitlinks(root)
	if err != nil {
		return "", false, err
	}
	return index.AtOrAbove(relativePath)
}

func PathsOverlap(first, second string) (bool, error) {
	firstInfo, err := os.Stat(first)
	if err != nil {
		return false, err
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		return false, err
	}
	if os.SameFile(firstInfo, secondInfo) {
		return true, nil
	}
	firstContainsSecond, err := fileAncestorContains(firstInfo, second)
	if err != nil {
		return false, err
	}
	if firstContainsSecond {
		return true, nil
	}
	return fileAncestorContains(secondInfo, first)
}

func fileAncestorContains(ancestor os.FileInfo, candidate string) (bool, error) {
	current := filepath.Clean(candidate)
	for {
		info, err := os.Stat(current)
		if err != nil {
			return false, err
		}
		if os.SameFile(ancestor, info) {
			return true, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false, nil
		}
		current = parent
	}
}

func findLexicalRoot(startDir, realRoot string) (string, error) {
	rootInfo, err := os.Stat(realRoot)
	if err != nil {
		return "", fmt.Errorf("inspect Git worktree root %s: %w", realRoot, err)
	}
	current := filepath.Clean(startDir)
	for {
		_, _, info, err := ExistingPath(current)
		if err != nil {
			return "", fmt.Errorf("resolve lexical Git worktree boundary from %s: %w", startDir, err)
		}
		if info.IsDir() && os.SameFile(rootInfo, info) {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", fmt.Errorf("resolve lexical Git worktree boundary from %s: no ancestor resolves to %s", startDir, realRoot)
}

func SamePath(first, second string) (bool, error) {
	firstInfo, err := os.Stat(first)
	if err != nil {
		return false, err
	}
	secondInfo, err := os.Stat(second)
	if err != nil {
		return false, err
	}
	return os.SameFile(firstInfo, secondInfo), nil
}

func PathContains(root, candidate string) (bool, error) {
	rootInfo, err := os.Stat(root)
	if err != nil {
		return false, err
	}
	return fileAncestorContains(rootInfo, candidate)
}

func ExistingPath(path string) (string, string, os.FileInfo, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", nil, err
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", "", nil, err
	}
	realPath, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", "", nil, err
	}
	realPath, err = filepath.Abs(realPath)
	if err != nil {
		return "", "", nil, err
	}
	return absolute, filepath.Clean(realPath), info, nil
}

func Contains(root, candidate string) bool {
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(candidate))
	if err != nil || filepath.IsAbs(relative) {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}

func isNotRepositoryError(output string) bool {
	output = strings.ToLower(output)
	return strings.Contains(output, "not a git repository") ||
		strings.Contains(output, "not in a git directory")
}

func gitCommand(dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = gitEnvironment()
	return cmd
}

func gitEnvironment() []string {
	environment := os.Environ()
	filtered := make([]string, 0, len(environment)+2)
	for _, entry := range environment {
		if strings.HasPrefix(entry, "LC_ALL=") || strings.HasPrefix(entry, "LANG=") {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, "LC_ALL=C", "LANG=C")
}
