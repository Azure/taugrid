package storage

import "testing"

func TestDurableFinetuneArtifactPaths(t *testing.T) {
	if got, want := DurableFinetuneArtifactsDir("run-1"), "/data/checkpoints/finetunes/run-1/artifacts"; got != want {
		t.Fatalf("DurableFinetuneArtifactsDir()=%q want %q", got, want)
	}
	if got, want := DurableFinetuneArtifactPath("run-1", "rank0/final.safetensors"), "/data/checkpoints/finetunes/run-1/artifacts/rank0/final.safetensors"; got != want {
		t.Fatalf("DurableFinetuneArtifactPath()=%q want %q", got, want)
	}
	if got, want := DurableFinetuneArtifactsFile("run-1"), "/data/checkpoints/finetunes/run-1/artifacts.json"; got != want {
		t.Fatalf("DurableFinetuneArtifactsFile()=%q want %q", got, want)
	}
	if got, want := DurableFinetuneModelFile("run-1"), "/data/checkpoints/finetunes/run-1/model.json"; got != want {
		t.Fatalf("DurableFinetuneModelFile()=%q want %q", got, want)
	}
}

func TestModelRegistryPaths(t *testing.T) {
	if got, want := ModelRegistryModelsDir(), "/data/model-registry/models"; got != want {
		t.Fatalf("ModelRegistryModelsDir()=%q want %q", got, want)
	}
	if got, want := ModelRegistryRunFile("sample", "run-1"), "/data/model-registry/models/sample/runs/run-1.json"; got != want {
		t.Fatalf("ModelRegistryRunFile()=%q want %q", got, want)
	}
	if got, want := ModelRegistryAliasFile("sample", "best-loss"), "/data/model-registry/models/sample/aliases/best-loss.json"; got != want {
		t.Fatalf("ModelRegistryAliasFile()=%q want %q", got, want)
	}
}

func TestDatasetRegistryPaths(t *testing.T) {
	if got, want := DatasetRegistryDatasetsDir(), "/data/dataset-registry/datasets"; got != want {
		t.Fatalf("DatasetRegistryDatasetsDir()=%q want %q", got, want)
	}
	if got, want := DatasetRegistryRecordFile("fineweb-sample-10bt", "v1"), "/data/dataset-registry/datasets/fineweb-sample-10bt/v1/dataset.json"; got != want {
		t.Fatalf("DatasetRegistryRecordFile()=%q want %q", got, want)
	}
	if got, want := DatasetRegistryAliasesDir("fineweb-sample-10bt"), "/data/dataset-registry/datasets/fineweb-sample-10bt/aliases"; got != want {
		t.Fatalf("DatasetRegistryAliasesDir()=%q want %q", got, want)
	}
	if got, want := DatasetRegistryAliasFile("fineweb-sample-10bt", "latest"), "/data/dataset-registry/datasets/fineweb-sample-10bt/aliases/latest.json"; got != want {
		t.Fatalf("DatasetRegistryAliasFile()=%q want %q", got, want)
	}
}
