// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package storage

import (
	"path"
	"strings"
)

const (
	DurableRoot           = "/data"
	HotRoot               = "/mnt"
	HotDatasetsDir        = "/mnt/datasets"
	HotCheckpointsDir     = "/mnt/checkpoints"
	DurableDatasetsDir    = "/data/datasets"
	DurableCheckpointsDir = "/data/checkpoints"
	DurableEvalsDir       = "/data/evals"
	FinetuneArtifactsDir  = "artifacts"
	FinetuneArtifactsFile = "artifacts.json"
	FinetuneModelFile     = "model.json"
	ModelRegistryDir      = "/data/model-registry"
	ModelRegistryModels   = "models"
	ModelRegistryRuns     = "runs"
	ModelRegistryAliases  = "aliases"

	// DatasetRegistryDir holds the dataset registry control-plane records,
	// mirroring the model registry layout. Records are small JSON files; the
	// dataset bytes themselves live in a dedicated dataset storage account
	// referenced by each record's account/container/prefix.
	DatasetRegistryDir      = "/data/dataset-registry"
	DatasetRegistryDatasets = "datasets"
	DatasetRegistryAliases  = "aliases"
	DatasetRecordFile       = "dataset.json"
)

func EvalFile(name string) string {
	return DurableEvalsDir + "/" + name + ".json"
}

func EvalDir(name string) string {
	return DurableEvalsDir + "/" + name
}

func FinetuneDir(name string) string {
	return HotCheckpointsDir + "/finetunes/" + name
}

func DurableFinetuneDir(name string) string {
	return DurableCheckpointsDir + "/finetunes/" + name
}

func DurableFinetuneArtifactsDir(name string) string {
	return path.Join(DurableFinetuneDir(name), FinetuneArtifactsDir)
}

func DurableFinetuneArtifactPath(name, artifact string) string {
	return path.Join(DurableFinetuneArtifactsDir(name), strings.TrimLeft(artifact, "/"))
}

func DurableFinetuneArtifactsFile(name string) string {
	return path.Join(DurableFinetuneDir(name), FinetuneArtifactsFile)
}

func DurableFinetuneModelFile(name string) string {
	return path.Join(DurableFinetuneDir(name), FinetuneModelFile)
}

func ModelRegistryModelsDir() string {
	return path.Join(ModelRegistryDir, ModelRegistryModels)
}

func ModelRegistryModelDir(model string) string {
	return path.Join(ModelRegistryModelsDir(), strings.TrimLeft(model, "/"))
}

func ModelRegistryModelRunsDir(model string) string {
	return path.Join(ModelRegistryModelDir(model), ModelRegistryRuns)
}

func ModelRegistryRunFile(model, run string) string {
	return path.Join(ModelRegistryModelRunsDir(model), strings.TrimLeft(run, "/")+".json")
}

func ModelRegistryModelAliasesDir(model string) string {
	return path.Join(ModelRegistryModelDir(model), ModelRegistryAliases)
}

func ModelRegistryAliasFile(model, alias string) string {
	return path.Join(ModelRegistryModelAliasesDir(model), strings.TrimLeft(alias, "/")+".json")
}

// DatasetRegistryDatasetsDir is the root under which per-dataset records live.
func DatasetRegistryDatasetsDir() string {
	return path.Join(DatasetRegistryDir, DatasetRegistryDatasets)
}

// DatasetRegistryDatasetDir is the directory for a single dataset name. Its
// children are version directories plus an aliases directory.
func DatasetRegistryDatasetDir(name string) string {
	return path.Join(DatasetRegistryDatasetsDir(), strings.TrimLeft(name, "/"))
}

// DatasetRegistryVersionDir is the directory for one immutable dataset version.
func DatasetRegistryVersionDir(name, version string) string {
	return path.Join(DatasetRegistryDatasetDir(name), strings.TrimLeft(version, "/"))
}

// DatasetRegistryRecordFile is the immutable per-version record (dataset.json).
func DatasetRegistryRecordFile(name, version string) string {
	return path.Join(DatasetRegistryVersionDir(name, version), DatasetRecordFile)
}

// DatasetRegistryAliasesDir holds the movable alias pointers for a dataset.
func DatasetRegistryAliasesDir(name string) string {
	return path.Join(DatasetRegistryDatasetDir(name), DatasetRegistryAliases)
}

// DatasetRegistryAliasFile is the movable alias pointer file for a dataset.
func DatasetRegistryAliasFile(name, alias string) string {
	return path.Join(DatasetRegistryAliasesDir(name), strings.TrimLeft(alias, "/")+".json")
}

// DatasetRegistryIngestStatusFile is the mutable ingest-status companion
// (ingest-status.json) that tracks ingestion progress for one version. It lives
// alongside the immutable dataset.json in the version directory.
func DatasetRegistryIngestStatusFile(name, version string) string {
	return path.Join(DatasetRegistryVersionDir(name, version), "ingest-status.json")
}

func NormalizeCheckpointPath(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return DurableCheckpointsDir + "/" + strings.TrimLeft(path, "/")
}
