// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package envspec

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var envNameRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Var is the Tau env contract rendered into Kubernetes containers.
// Value is for non-secret literals. ValueFrom.SecretKeyRef is for values
// that must stay in Kubernetes Secrets and never appear in rendered YAML.
type Var struct {
	Name      string     `yaml:"name"`
	Value     string     `yaml:"value,omitempty"`
	ValueFrom *ValueFrom `yaml:"valueFrom,omitempty"`
}

type ValueFrom struct {
	SecretKeyRef *SecretKeyRef `yaml:"secretKeyRef,omitempty"`
}

type SecretKeyRef struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

func direct(name, value string) Var {
	return Var{Name: name, Value: value}
}

func Secret(name, secretName, key string) Var {
	return Var{
		Name: name,
		ValueFrom: &ValueFrom{
			SecretKeyRef: &SecretKeyRef{Name: secretName, Key: key},
		},
	}
}

func ParseSecretKeyRefSpec(spec string) (SecretKeyRef, error) {
	secretName, key, ok := strings.Cut(spec, ":")
	if !ok || strings.TrimSpace(secretName) == "" || strings.TrimSpace(key) == "" || strings.TrimSpace(spec) != spec || strings.TrimSpace(secretName) != secretName || strings.TrimSpace(key) != key {
		return SecretKeyRef{}, fmt.Errorf("expected SECRET:KEY, got %q", spec)
	}
	return SecretKeyRef{Name: secretName, Key: key}, nil
}

func RedactSecretRefs(vars []Var) []Var {
	out := make([]Var, len(vars))
	copy(out, vars)
	for i := range out {
		if out[i].ValueFrom == nil || out[i].ValueFrom.SecretKeyRef == nil {
			continue
		}
		out[i].ValueFrom = &ValueFrom{
			SecretKeyRef: &SecretKeyRef{Name: "<redacted>", Key: "<redacted>"},
		}
	}
	return out
}

func FromMap(values map[string]string) []Var {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Var, 0, len(keys))
	for _, k := range keys {
		out = append(out, direct(k, values[k]))
	}
	return out
}

// Merge returns env vars sorted by name. Later entries override earlier entries,
// matching CLI override semantics over profile/config defaults.
func Merge(groups ...[]Var) ([]Var, error) {
	byName := map[string]Var{}
	for _, group := range groups {
		for _, v := range group {
			if err := validateOne(v); err != nil {
				return nil, err
			}
			byName[v.Name] = v
		}
	}
	keys := make([]string, 0, len(byName))
	for k := range byName {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]Var, 0, len(keys))
	for _, k := range keys {
		out = append(out, byName[k])
	}
	return out, nil
}

func Validate(vars []Var) error {
	for i, v := range vars {
		if err := validateOne(v); err != nil {
			return fmt.Errorf("runtime.env[%d]: %w", i, err)
		}
	}
	return nil
}

func validateOne(v Var) error {
	if !envNameRE.MatchString(v.Name) {
		return fmt.Errorf("env var name %q is invalid (use C_IDENTIFIER format)", v.Name)
	}
	hasSecret := v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil
	if v.Value != "" && hasSecret {
		return fmt.Errorf("env var %s must set either value or valueFrom.secretKeyRef, not both", v.Name)
	}
	if v.ValueFrom != nil && !hasSecret {
		return fmt.Errorf("env var %s has unsupported valueFrom; only secretKeyRef is supported", v.Name)
	}
	if hasSecret {
		ref := v.ValueFrom.SecretKeyRef
		if ref.Name == "" || ref.Key == "" {
			return fmt.Errorf("env var %s secretKeyRef requires name and key", v.Name)
		}
	}
	return nil
}

func DirectMap(vars []Var) map[string]string {
	out := map[string]string{}
	for _, v := range vars {
		if v.ValueFrom == nil {
			out[v.Name] = v.Value
		}
	}
	return out
}

func K8sList(vars []Var) []any {
	out := make([]any, 0, len(vars))
	for _, v := range vars {
		item := map[string]any{"name": v.Name}
		if v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil {
			item["valueFrom"] = map[string]any{
				"secretKeyRef": map[string]any{
					"name": v.ValueFrom.SecretKeyRef.Name,
					"key":  v.ValueFrom.SecretKeyRef.Key,
				},
			}
		} else {
			item["value"] = v.Value
		}
		out = append(out, item)
	}
	return out
}

func RenderYAML(vars []Var, indent int) string {
	if len(vars) == 0 {
		return ""
	}
	spaces := makeSpaces(indent)
	child := makeSpaces(indent + 2)
	grandchild := makeSpaces(indent + 4)
	greatGrandchild := makeSpaces(indent + 6)
	var out string
	for _, v := range vars {
		out += fmt.Sprintf("%s- name: %s\n", spaces, quote(v.Name))
		if v.ValueFrom != nil && v.ValueFrom.SecretKeyRef != nil {
			out += fmt.Sprintf("%svalueFrom:\n", child)
			out += fmt.Sprintf("%ssecretKeyRef:\n", grandchild)
			out += fmt.Sprintf("%sname: %s\n", greatGrandchild, quote(v.ValueFrom.SecretKeyRef.Name))
			out += fmt.Sprintf("%skey: %s\n", greatGrandchild, quote(v.ValueFrom.SecretKeyRef.Key))
			continue
		}
		out += fmt.Sprintf("%svalue: %s\n", child, quote(v.Value))
	}
	return out
}

func ParseProfileEnv(raw any) ([]Var, error) {
	switch v := raw.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		out := make([]Var, 0, len(v))
		for name, value := range v {
			if value == nil {
				out = append(out, direct(name, ""))
			} else {
				out = append(out, direct(name, fmt.Sprint(value)))
			}
		}
		return Merge(out)
	case []any:
		out := make([]Var, 0, len(v))
		for i, item := range v {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("runtime.env[%d]: expected mapping", i)
			}
			name, _ := m["name"].(string)
			if name == "" {
				return nil, fmt.Errorf("runtime.env[%d]: name is required", i)
			}
			if vf, ok := m["valueFrom"].(map[string]any); ok {
				ref, ok := vf["secretKeyRef"].(map[string]any)
				if !ok {
					return nil, fmt.Errorf("runtime.env[%d]: valueFrom only supports secretKeyRef", i)
				}
				secretName, _ := ref["name"].(string)
				key, _ := ref["key"].(string)
				out = append(out, Secret(name, secretName, key))
				continue
			}
			switch value := m["value"].(type) {
			case string:
				out = append(out, direct(name, value))
			case nil:
				out = append(out, direct(name, ""))
			default:
				out = append(out, direct(name, fmt.Sprint(value)))
			}
		}
		return Merge(out)
	default:
		return nil, fmt.Errorf("runtime.env: expected mapping or list, got %T", raw)
	}
}

func makeSpaces(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = ' '
	}
	return string(b)
}

func quote(value string) string {
	return strconv.Quote(value)
}
