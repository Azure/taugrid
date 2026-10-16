// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

func fileURIFromPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("make file URI path absolute: %w", err)
	}
	uriPath := filepath.ToSlash(abs)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	return (&url.URL{Scheme: "file", Path: uriPath}).String(), nil
}

func localPathFromFileURI(value string) (string, error) {
	if !strings.HasPrefix(value, "file://") {
		return "", fmt.Errorf("%q is not a file URI", value)
	}

	legacyPath := strings.TrimPrefix(value, "file://")
	if !strings.HasPrefix(legacyPath, "/") {
		return filepath.FromSlash(legacyPath), nil
	}

	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse file URI %q: %w", value, err)
	}
	if parsed.Host != "" && parsed.Host != "localhost" {
		return "", fmt.Errorf("file URI %q must not include a host", value)
	}
	path := filepath.FromSlash(parsed.Path)
	if runtime.GOOS == "windows" && len(path) >= 3 && path[0] == filepath.Separator && path[2] == ':' {
		path = path[1:]
	}
	return path, nil
}
