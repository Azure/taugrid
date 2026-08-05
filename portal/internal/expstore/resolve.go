package expstore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	ExpStoreEnv     = "TAU_EXP_STORE"
	ExpStoreRootEnv = "TAU_EXP_STORE_ROOT"
	ExpContextEnv   = "TAU_CONTEXT"
	ExpTeamEnv      = "TAU_TEAM"
	ExpProjectEnv   = "TAU_PROJECT"
)

type ResolveOptions struct {
	Explicit string
	Context  string
	Team     string
	Project  string
	Getenv   func(string) string
}

func ResolveRoot(opts ResolveOptions) (string, error) {
	if root := strings.TrimSpace(opts.Explicit); root != "" {
		return cleanRoot(root)
	}
	getenv := opts.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	if root := strings.TrimSpace(getenv(ExpStoreEnv)); root != "" {
		return cleanRoot(root)
	}
	storeRoot := strings.TrimSpace(getenv(ExpStoreRootEnv))
	if storeRoot == "" {
		return "", fmt.Errorf("--store is required (or set %s, or set %s with %s, %s, and %s)", ExpStoreEnv, ExpStoreRootEnv, ExpContextEnv, ExpTeamEnv, ExpProjectEnv)
	}
	context := firstNonEmpty(opts.Context, getenv(ExpContextEnv))
	team := firstNonEmpty(opts.Team, getenv(ExpTeamEnv))
	project := firstNonEmpty(opts.Project, getenv(ExpProjectEnv))
	if missing := missingStoreComponents(context, team, project); len(missing) > 0 {
		return "", fmt.Errorf("%s requires %s (missing %s)", ExpStoreRootEnv, strings.Join([]string{ExpContextEnv, ExpTeamEnv, ExpProjectEnv}, ", "), strings.Join(missing, ", "))
	}
	for _, component := range []struct {
		name  string
		value string
	}{
		{ExpContextEnv, context},
		{ExpTeamEnv, team},
		{ExpProjectEnv, project},
	} {
		if err := validateStorePathComponent(component.name, component.value); err != nil {
			return "", err
		}
	}
	return cleanRoot(filepath.Join(storeRoot, context, team, project))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func missingStoreComponents(context, team, project string) []string {
	var missing []string
	if strings.TrimSpace(context) == "" {
		missing = append(missing, ExpContextEnv)
	}
	if strings.TrimSpace(team) == "" {
		missing = append(missing, ExpTeamEnv)
	}
	if strings.TrimSpace(project) == "" {
		missing = append(missing, ExpProjectEnv)
	}
	return missing
}

func validateStorePathComponent(name, value string) error {
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s %q is invalid (must not have leading or trailing whitespace)", name, value)
	}
	if strings.Contains(value, "/") || strings.Contains(value, "\\") {
		return fmt.Errorf("%s %q is invalid (must be a single path component)", name, value)
	}
	if value == "." || value == ".." {
		return fmt.Errorf("%s %q is invalid (must be a named path component)", name, value)
	}
	return nil
}
