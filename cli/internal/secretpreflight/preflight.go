// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package secretpreflight

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/core/envspec"
	"k8s.io/apimachinery/pkg/util/validation"
)

const secretKeysTemplate = `go-template={{range $key, $_ := .data}}{{printf "%s\n" $key}}{{end}}`

type Runner interface {
	Raw(context.Context, []string, []byte) (string, error)
}

// AvailableSecret describes a Secret that will be created in the same apply
// operation as the workload. Keys are sufficient for preflight; values stay
// with the owning renderer.
type AvailableSecret struct {
	Name string
	Keys []string
}

type requiredSecret struct {
	keys map[string][]string
}

// ValidateRequiredEnv verifies required Secret key references without returning
// Secret values to Tau. The caller's Kubernetes identity remains authoritative:
// restricted identities fail closed instead of bypassing the check.
func ValidateRequiredEnv(ctx context.Context, runner Runner, namespace string, vars []envspec.Var, available ...AvailableSecret) error {
	required := collectRequired(vars)
	if len(required) == 0 {
		return nil
	}
	pending := make(map[string]map[string]struct{}, len(available))
	for _, secret := range available {
		pending[secret.Name] = stringSet(secret.Keys)
	}

	names := sortedKeys(required)
	for _, name := range names {
		if problems := validation.IsDNS1123Subdomain(name); len(problems) > 0 {
			return fmt.Errorf("required Secret name %q is invalid: %s", name, strings.Join(problems, "; "))
		}
		if keys, ok := pending[name]; ok {
			if missing := missingKeys(required[name], keys); len(missing) > 0 {
				return fmt.Errorf("required Secret %s/%s is missing keys: %s", namespace, name, strings.Join(missing, ", "))
			}
			continue
		}
		if runner == nil {
			return fmt.Errorf("required Secret preflight needs a Kubernetes client")
		}
		allowed, err := runner.Raw(ctx, []string{
			"auth", "can-i", "get", "-n", namespace, "--", "secret/" + name,
		}, nil)
		if err != nil || strings.TrimSpace(allowed) != "yes" {
			if strings.TrimSpace(allowed) == "no" {
				return fmt.Errorf("cannot verify required Secret %s/%s before submission: current identity cannot get this Secret; Tau will not submit without a permission-safe preflight", namespace, name)
			}
			if err == nil {
				return fmt.Errorf("check permission to verify required Secret %s/%s: kubectl auth can-i returned %q", namespace, name, strings.TrimSpace(allowed))
			}
			return fmt.Errorf("check permission to verify required Secret %s/%s: %w", namespace, name, err)
		}

		out, err := runner.Raw(ctx, []string{
			"get", "secret", "-n", namespace, "-o", secretKeysTemplate, "--", name,
		}, nil)
		if err != nil {
			message := strings.ToLower(err.Error())
			switch {
			case strings.Contains(message, "notfound"), strings.Contains(message, "not found"):
				return fmt.Errorf("required Secret %s/%s does not exist; referenced keys: %s", namespace, name, strings.Join(sortedKeys(required[name].keys), ", "))
			case strings.Contains(message, "forbidden"):
				return fmt.Errorf("cannot verify required Secret %s/%s before submission: current identity cannot get this Secret; Tau will not submit without a permission-safe preflight", namespace, name)
			default:
				return fmt.Errorf("verify required Secret %s/%s: %w", namespace, name, err)
			}
		}

		available := lineSet(out)
		if missing := missingKeys(required[name], available); len(missing) > 0 {
			return fmt.Errorf("required Secret %s/%s is missing keys: %s", namespace, name, strings.Join(missing, ", "))
		}
	}
	return nil
}

func collectRequired(vars []envspec.Var) map[string]requiredSecret {
	out := make(map[string]requiredSecret)
	for _, variable := range vars {
		if variable.ValueFrom == nil || variable.ValueFrom.SecretKeyRef == nil {
			continue
		}
		ref := variable.ValueFrom.SecretKeyRef
		item := out[ref.Name]
		if item.keys == nil {
			item.keys = make(map[string][]string)
		}
		item.keys[ref.Key] = append(item.keys[ref.Key], variable.Name)
		out[ref.Name] = item
	}
	return out
}

func missingKeys(required requiredSecret, available map[string]struct{}) []string {
	var missing []string
	for _, key := range sortedKeys(required.keys) {
		if _, ok := available[key]; !ok {
			missing = append(missing, key)
		}
	}
	return missing
}

func lineSet(value string) map[string]struct{} {
	return stringSet(strings.Split(value, "\n"))
}

func stringSet(values []string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			out[value] = struct{}{}
		}
	}
	return out
}

func sortedKeys[V any](values map[string]V) []string {
	out := make([]string, 0, len(values))
	for key := range values {
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}
