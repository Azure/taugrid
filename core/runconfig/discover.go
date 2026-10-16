// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package runconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/taugrid/core/experiment"
)

var DefaultFiles = []string{"tau.yaml", "tau.yml", ".tau.yaml"}

func DiscoverDefault(dir string) (string, error) {
	for _, candidate := range DefaultFiles {
		path := filepath.Join(dir, candidate)
		if _, err := os.Stat(path); err == nil {
			return path, nil
		} else if !os.IsNotExist(err) {
			return "", fmt.Errorf("check %s: %w", path, err)
		}
	}
	return "", nil
}

func ExperimentRunMetadata(e Experiment) experiment.RunMetadata {
	group := strings.TrimSpace(e.Group)
	return experiment.RunMetadata{
		Project:      strings.TrimSpace(e.Project),
		ExperimentID: e.resolvedID(),
		RunGroupID:   group,
	}
}
