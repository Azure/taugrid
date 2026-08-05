package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/Azure/taugrid/core/runconfig"
	"github.com/Azure/taugrid/portal/internal/expimport"
)

func defaultExperimentMetadataFromRunConfig() (runExperimentMetadata, error) {
	configPath, err := runconfig.DiscoverDefault("")
	if err != nil {
		return runExperimentMetadata{}, err
	}
	if configPath == "" {
		return runExperimentMetadata{}, nil
	}
	return loadExperimentMetadataFromRunConfig(configPath)
}

func loadExperimentMetadataFromRunConfig(path string) (runExperimentMetadata, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return runExperimentMetadata{}, err
	}
	var cfg struct {
		Name       string               `yaml:"name"`
		Run        runconfig.Run        `yaml:"run"`
		Experiment runconfig.Experiment `yaml:"experiment"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return runExperimentMetadata{}, fmt.Errorf("parse %s: %w", path, err)
	}
	meta := runconfig.ExperimentRunMetadata(cfg.Experiment)
	meta.RunID = strings.TrimSpace(firstNonEmpty(cfg.Run.Name, cfg.Name))
	return meta, nil
}

func applyExperimentDefaults(cmd *cobra.Command, runID, project, experimentID, runGroupID *string) error {
	meta, err := defaultExperimentMetadataFromRunConfig()
	if err != nil {
		return err
	}
	if meta.Empty() {
		return nil
	}
	if runID != nil && strings.TrimSpace(*runID) == "" && !cmd.Flags().Changed("run") {
		*runID = meta.RunID
	}
	if project != nil && strings.TrimSpace(*project) == "" && !cmd.Flags().Changed("project") {
		*project = meta.Project
	}
	if experimentID != nil && strings.TrimSpace(*experimentID) == "" && !cmd.Flags().Changed("experiment") {
		*experimentID = meta.ExperimentID
	}
	if runGroupID != nil && strings.TrimSpace(*runGroupID) == "" && !cmd.Flags().Changed("group") {
		*runGroupID = meta.RunGroupID
	}
	return nil
}

func applyExperimentDefaultsToJSONLImport(cmd *cobra.Command, opts *expimport.JSONLImportOptions) error {
	return applyExperimentDefaults(cmd, &opts.RunID, &opts.Project, &opts.ExperimentID, &opts.RunGroupID)
}
