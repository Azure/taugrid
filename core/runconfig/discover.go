package runconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Azure/taugrid/core/experiment"
)

// DefaultFiles are the run config names Tau looks for when no --config is
// given, in precedence order.
var DefaultFiles = []string{"tau.yaml", "tau.yml", ".tau.yaml"}

// DiscoverDefault returns the path of the first DefaultFiles entry present in
// dir, or "" when the directory holds no run config. An empty dir means the
// process working directory.
//
// Both products need this: `tau run` resolves the config it is about to
// submit, and taugrid-portal reads the same file to default experiment flags
// so `taugrid-portal experiment track` picks up the project and experiment a
// repository already declares.
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

// ExperimentRunMetadata derives run identity from a config's experiment block.
func ExperimentRunMetadata(e Experiment) experiment.RunMetadata {
	group := strings.TrimSpace(e.Group)
	return experiment.RunMetadata{
		Project:      strings.TrimSpace(e.Project),
		ExperimentID: e.resolvedID(),
		RunGroupID:   group,
	}
}
