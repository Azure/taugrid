package profile

import (
	"github.com/Azure/taugrid/core/workloadmeta"
	"strings"
	"testing"
)

func TestStorageContractFromProfile(t *testing.T) {
	p := Profile{
		Name: "swordfish-bench-a100",
		Spec: map[string]any{
			"resources": map[string]any{
				"storage": map[string]any{
					"durable": map[string]any{
						"type":      "blobfuse",
						"mountPath": "/data",
						"cache":     "block",
					},
					"hot": map[string]any{
						"type":      "emptyDir",
						"mountPath": "/mnt",
						"fallback":  "durable",
					},
					"modelCache": map[string]any{
						"type":      "local-nvme",
						"mountPath": "/models",
						"scope":     "node",
					},
					"checkpointing": map[string]any{
						"format": "sharded",
					},
				},
			},
		},
	}

	got, ok, err := StorageContractFromProfile(p)
	if err != nil {
		t.Fatalf("StorageContractFromProfile: %v", err)
	}
	if !ok {
		t.Fatal("expected storage contract")
	}
	durable, ok := got.Role(StorageRoleDurableData)
	if !ok || durable.Type != "blobfuse" || durable.Cache != "block" || durable.MountPath != "/data" {
		t.Fatalf("durable role wrong: %+v", durable)
	}
	hot, ok := got.Role(StorageRoleHotScratch)
	if !ok || hot.Type != "empty-dir" || hot.Fallback != StorageRoleDurableData {
		t.Fatalf("hot role wrong: %+v", hot)
	}
	if got.Checkpointing.Format != "sharded" {
		t.Fatalf("checkpointing format=%q", got.Checkpointing.Format)
	}
	labels := got.Labels()
	if labels[workloadmeta.LabelStorageDurableType] != "blobfuse" ||
		labels[workloadmeta.LabelStorageHotType] != "empty-dir" ||
		labels[workloadmeta.LabelStorageModelCacheType] != "local-nvme" ||
		labels[workloadmeta.LabelStorageCheckpointFormat] != "sharded" {
		t.Fatalf("labels wrong: %+v", labels)
	}
	if summary := got.Summary(); !strings.Contains(summary, "durable(type=blobfuse,mount=/data,cache=block)") ||
		!strings.Contains(summary, "checkpointing(format=sharded)") {
		t.Fatalf("summary missing contract details: %s", summary)
	}
}

func TestStorageContractRejectsRelativeMount(t *testing.T) {
	p := Profile{
		Name: "bad-storage",
		Spec: map[string]any{
			"resources": map[string]any{
				"storage": map[string]any{
					"hot": map[string]any{
						"type":      "emptyDir",
						"mountPath": "mnt",
					},
				},
			},
		},
	}
	_, _, err := StorageContractFromProfile(p)
	if err == nil || !strings.Contains(err.Error(), "mountPath must be absolute") {
		t.Fatalf("expected relative mount error, got %v", err)
	}
}
