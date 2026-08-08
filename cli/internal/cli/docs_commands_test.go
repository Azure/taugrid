// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

var (
	markdownFencePattern = regexp.MustCompile("(?ms)^[ \\t]{0,3}```([^\\n]*)\\n(.*?)^[ \\t]{0,3}```[ \\t]*$")
	inlineCodePattern    = regexp.MustCompile("(?s)`([^`]+)`")
)

type documentedTauCommand struct {
	line    int
	command string
}

func TestDocumentedTauCommandsUsePublicTree(t *testing.T) {
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test file path")
	}
	docsRoot := filepath.Clean(filepath.Join(filepath.Dir(filename), "..", "..", "..", "site", "content", "en", "docs"))
	root := NewRoot()

	err := filepath.WalkDir(docsRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, documented := range extractDocumentedTauCommands(string(data)) {
			fields := strings.Fields(documented.command)
			if len(fields) == 0 || fields[0] != "tau" {
				continue
			}
			if len(fields) == 1 || strings.HasPrefix(fields[1], "-") {
				continue
			}
			command, _, findErr := root.Find(fields[1:])
			if findErr != nil || command == nil {
				t.Errorf("%s:%d: documented command %q does not resolve: %v", path, documented.line, documented.command, findErr)
				continue
			}
			top := command
			for top.Parent() != nil && top.Parent() != root {
				top = top.Parent()
			}
			if top.Hidden {
				t.Errorf("%s:%d: documented command %q uses hidden root %q", path, documented.line, documented.command, top.Name())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExtractDocumentedTauCommandsIncludesIndentedFences(t *testing.T) {
	content := "1. Validate the config.\n\n   ```bash\n   tau run status example\n   ```\n"
	commands := extractDocumentedTauCommands(content)
	if len(commands) != 1 {
		t.Fatalf("got %d commands, want 1: %#v", len(commands), commands)
	}
	if commands[0].command != "tau run status example" {
		t.Fatalf("got command %q, want %q", commands[0].command, "tau run status example")
	}
}

func extractDocumentedTauCommands(content string) []documentedTauCommand {
	var commands []documentedTauCommand
	for _, match := range markdownFencePattern.FindAllStringSubmatchIndex(content, -1) {
		language := strings.TrimSpace(content[match[2]:match[3]])
		if language != "bash" && language != "sh" && language != "shell" && language != "console" {
			continue
		}
		commands = append(commands, extractTauCommands(content[match[4]:match[5]], lineAt(content, match[4]))...)
	}

	withoutFences := markdownFencePattern.ReplaceAllString(content, "")
	for _, match := range inlineCodePattern.FindAllStringSubmatchIndex(withoutFences, -1) {
		commands = append(commands, extractTauCommands(withoutFences[match[2]:match[3]], lineAt(withoutFences, match[2]))...)
	}
	return commands
}

func extractTauCommands(text string, startLine int) []documentedTauCommand {
	var commands []documentedTauCommand
	for offset, line := range strings.Split(text, "\n") {
		fields := strings.Fields(strings.TrimSpace(strings.TrimPrefix(line, "$")))
		for index, field := range fields {
			if strings.Trim(field, "`(),.;:") != "tau" {
				continue
			}
			end := len(fields)
			for i := index + 1; i < len(fields); i++ {
				if fields[i] == "&&" || fields[i] == "||" || fields[i] == "|" || fields[i] == ";" {
					end = i
					break
				}
			}
			commandFields := append([]string{"tau"}, fields[index+1:end]...)
			for i := range commandFields {
				commandFields[i] = strings.Trim(commandFields[i], "`(),.;:")
			}
			commands = append(commands, documentedTauCommand{
				line:    startLine + offset,
				command: strings.Join(commandFields, " "),
			})
			break
		}
	}
	return commands
}

func lineAt(content string, offset int) int {
	return strings.Count(content[:offset], "\n") + 1
}
