// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package podsecurity applies Tau's typed Restricted Pod Security contract.
package podsecurity

import "fmt"

const defaultRestrictedID int64 = 65532

// ApplyRestricted adds Restricted Pod Security fields to every generated
// container. fsGroup is optional; Ray uses it for its shared emptyDir.
func ApplyRestricted(pod map[string]any, fsGroup *int64) error {
	podContext, err := securityContext(pod, "pod")
	if err != nil {
		return err
	}
	if err := rejectRoot(podContext, "pod"); err != nil {
		return err
	}
	podContext["runAsNonRoot"] = true
	setSeccomp(podContext)
	if fsGroup != nil {
		if _, exists := podContext["fsGroup"]; !exists {
			podContext["fsGroup"] = *fsGroup
		}
	}
	pod["securityContext"] = podContext

	for _, field := range []string{"initContainers", "containers"} {
		containers, ok := pod[field].([]any)
		if !ok && pod[field] != nil {
			return fmt.Errorf("restricted security: pod %s must be a container list", field)
		}
		for i, raw := range containers {
			container, ok := raw.(map[string]any)
			if !ok {
				return fmt.Errorf("restricted security: pod %s[%d] must be a container", field, i)
			}
			name, _ := container["name"].(string)
			if name == "" {
				name = fmt.Sprintf("%s[%d]", field, i)
			}
			if err := applyContainer(container, name); err != nil {
				return err
			}
		}
	}
	return nil
}

func applyContainer(container map[string]any, name string) error {
	context, err := securityContext(container, "container "+name)
	if err != nil {
		return err
	}
	if value, _ := context["privileged"].(bool); value {
		return fmt.Errorf("runtime.security.mode=restricted conflicts with privileged container %q", name)
	}
	if value, _ := context["allowPrivilegeEscalation"].(bool); value {
		return fmt.Errorf("runtime.security.mode=restricted conflicts with allowPrivilegeEscalation on container %q", name)
	}
	if err := rejectRoot(context, "container "+name); err != nil {
		return err
	}
	capabilities, err := nestedMap(context, "capabilities", "container "+name)
	if err != nil {
		return err
	}
	if additions, exists := capabilities["add"]; exists && nonEmptyList(additions) {
		return fmt.Errorf("runtime.security.mode=restricted conflicts with added capabilities on container %q", name)
	}
	capabilities["drop"] = []any{"ALL"}
	context["capabilities"] = capabilities
	context["allowPrivilegeEscalation"] = false
	context["runAsNonRoot"] = true
	if _, exists := context["runAsUser"]; !exists {
		context["runAsUser"] = defaultRestrictedID
	}
	if _, exists := context["runAsGroup"]; !exists {
		context["runAsGroup"] = defaultRestrictedID
	}
	setSeccomp(context)
	container["securityContext"] = context
	return nil
}

func securityContext(parent map[string]any, owner string) (map[string]any, error) {
	return nestedMap(parent, "securityContext", owner)
}

func nestedMap(parent map[string]any, field, owner string) (map[string]any, error) {
	raw, exists := parent[field]
	if !exists || raw == nil {
		return map[string]any{}, nil
	}
	value, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("restricted security: %s %s must be an object", owner, field)
	}
	copy := make(map[string]any, len(value))
	for key, item := range value {
		copy[key] = item
	}
	return copy, nil
}

func rejectRoot(context map[string]any, owner string) error {
	if value, exists := context["runAsNonRoot"]; exists {
		if enabled, ok := value.(bool); ok && !enabled {
			return fmt.Errorf("runtime.security.mode=restricted conflicts with runAsNonRoot=false on %s", owner)
		}
	}
	if value, exists := context["runAsUser"]; exists && numericZero(value) {
		return fmt.Errorf("runtime.security.mode=restricted conflicts with runAsUser=0 on %s", owner)
	}
	if profile, ok := context["seccompProfile"].(map[string]any); ok {
		if profileType, _ := profile["type"].(string); profileType == "Unconfined" {
			return fmt.Errorf("runtime.security.mode=restricted conflicts with unconfined seccomp on %s", owner)
		}
	}
	return nil
}

func setSeccomp(context map[string]any) {
	if _, exists := context["seccompProfile"]; !exists {
		context["seccompProfile"] = map[string]any{"type": "RuntimeDefault"}
	}
}

func numericZero(value any) bool {
	switch value := value.(type) {
	case int:
		return value == 0
	case int32:
		return value == 0
	case int64:
		return value == 0
	case float64:
		return value == 0
	default:
		return false
	}
}

func nonEmptyList(value any) bool {
	switch value := value.(type) {
	case []any:
		return len(value) > 0
	case []string:
		return len(value) > 0
	default:
		return value != nil
	}
}
