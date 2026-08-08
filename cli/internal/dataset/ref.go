// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package dataset

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Ref is a parsed dataset reference. A bare name resolves through the default
// alias; name@token is resolved as a version first, then as an alias.
type Ref struct {
	Name    string
	Version string
	Alias   string
}

// DefaultAlias is used when a reference names only a dataset.
const DefaultAlias = "latest"

// ParseRef parses NAME, NAME@VERSION, or NAME@ALIAS.
func ParseRef(ref string) (Ref, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Ref{}, fmt.Errorf("dataset ref is required")
	}
	if strings.Contains(ref, "@") {
		parts := strings.SplitN(ref, "@", 2)
		if parts[0] == "" || parts[1] == "" {
			return Ref{}, fmt.Errorf("dataset ref %q must be NAME@VERSION or NAME@ALIAS", ref)
		}
		if err := ValidateName(parts[0]); err != nil {
			return Ref{}, err
		}
		return Ref{Name: parts[0], Version: parts[1]}, nil
	}
	if err := ValidateName(ref); err != nil {
		return Ref{}, err
	}
	return Ref{Name: ref, Alias: DefaultAlias}, nil
}

func marshalJSON(v any) ([]byte, error) {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}
