package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateCapacityAcceptsBorrowingQueueWithinNominalCapacity(t *testing.T) {
	capacity := writeTempFile(t, "capacity.yaml", `
apiVersion: ai-runtime.aks/v1alpha1
kind: KueueCapacityInventory
metadata:
  name: test
spec:
  resources:
  - flavor: a100
    resource: nvidia.com/gpu
    capacity: "8"
    reserve: "1"
`)
	queues := writeTempFile(t, "queues.yaml", `
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata:
  name: reserved-a
spec:
  cohort: research
  resourceGroups:
  - coveredResources: ["nvidia.com/gpu"]
    flavors:
    - name: a100
      resources:
      - name: nvidia.com/gpu
        nominalQuota: "4"
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata:
  name: default
spec:
  cohort: research
  resourceGroups:
  - coveredResources: ["nvidia.com/gpu"]
    flavors:
    - name: a100
      resources:
      - name: nvidia.com/gpu
        nominalQuota: "3"
        borrowingLimit: "5"
`)

	limits, err := loadCapacityInventory(capacity)
	if err != nil {
		t.Fatalf("load capacity: %v", err)
	}
	quotas, err := loadClusterQueueQuotas([]string{queues})
	if err != nil {
		t.Fatalf("load quotas: %v", err)
	}
	report, err := validateCapacity(limits, quotas)
	if err != nil {
		t.Fatalf("validate capacity: %v", err)
	}
	if report.ClusterQueues != 2 {
		t.Fatalf("ClusterQueues=%d, want 2", report.ClusterQueues)
	}
}

func TestValidateCapacityRejectsNominalQuotaAboveCapacity(t *testing.T) {
	capacity := writeTempFile(t, "capacity.yaml", `
apiVersion: ai-runtime.aks/v1alpha1
kind: KueueCapacityInventory
metadata:
  name: test
spec:
  resources:
  - flavor: h100
    resource: nvidia.com/gpu
    capacity: "4"
`)
	queues := writeTempFile(t, "queues.yaml", `
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata:
  name: team-a
spec:
  resourceGroups:
  - coveredResources: ["nvidia.com/gpu"]
    flavors:
    - name: h100
      resources:
      - name: nvidia.com/gpu
        nominalQuota: "3"
---
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata:
  name: team-b
spec:
  resourceGroups:
  - coveredResources: ["nvidia.com/gpu"]
    flavors:
    - name: h100
      resources:
      - name: nvidia.com/gpu
        nominalQuota: "2"
`)

	limits, err := loadCapacityInventory(capacity)
	if err != nil {
		t.Fatalf("load capacity: %v", err)
	}
	quotas, err := loadClusterQueueQuotas([]string{queues})
	if err != nil {
		t.Fatalf("load quotas: %v", err)
	}
	_, err = validateCapacity(limits, quotas)
	if err == nil {
		t.Fatal("expected capacity validation to fail")
	}
	if !strings.Contains(err.Error(), "reserved nominalQuota 5 exceeds available capacity 4") {
		t.Fatalf("error %q did not describe over-reserved capacity", err)
	}
}

func TestValidateCapacityRejectsMissingCapacityEntry(t *testing.T) {
	capacity := writeTempFile(t, "capacity.yaml", `
apiVersion: ai-runtime.aks/v1alpha1
kind: KueueCapacityInventory
metadata:
  name: test
spec:
  resources:
  - flavor: default
    resource: cpu
    capacity: "8"
`)
	queues := writeTempFile(t, "queues.yaml", `
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata:
  name: gpu
spec:
  resourceGroups:
  - coveredResources: ["nvidia.com/gpu"]
    flavors:
    - name: default
      resources:
      - name: nvidia.com/gpu
        nominalQuota: "1"
`)

	limits, err := loadCapacityInventory(capacity)
	if err != nil {
		t.Fatalf("load capacity: %v", err)
	}
	quotas, err := loadClusterQueueQuotas([]string{queues})
	if err != nil {
		t.Fatalf("load quotas: %v", err)
	}
	_, err = validateCapacity(limits, quotas)
	if err == nil {
		t.Fatal("expected missing capacity validation to fail")
	}
	if !strings.Contains(err.Error(), "capacity inventory has no matching entry") {
		t.Fatalf("error %q did not describe the missing inventory entry", err)
	}
}

func TestLoadClusterQueueQuotasRejectsInvalidQuantity(t *testing.T) {
	queues := writeTempFile(t, "queues.yaml", `
apiVersion: kueue.x-k8s.io/v1beta1
kind: ClusterQueue
metadata:
  name: bad
spec:
  resourceGroups:
  - coveredResources: ["cpu"]
    flavors:
    - name: default
      resources:
      - name: cpu
        nominalQuota: "not-a-quantity"
`)

	_, err := loadClusterQueueQuotas([]string{queues})
	if err == nil {
		t.Fatal("expected invalid nominalQuota to fail")
	}
	if !strings.Contains(err.Error(), "not a valid Kubernetes quantity") {
		t.Fatalf("error %q did not describe invalid quantity", err)
	}
}

func TestRepositoryKueueFixturesStayWithinDeclaredCapacity(t *testing.T) {
	capacity := filepath.Join("..", "..", "kueue", "fixtures", "kueue-capacity.yaml")
	inputs := []string{
		filepath.Join("..", "..", "kueue", "fixtures", "kueue-resources.yaml"),
		filepath.Join("..", "..", "stack", "fixtures", "stack-kueue-resources.yaml"),
	}

	limits, err := loadCapacityInventory(capacity)
	if err != nil {
		t.Fatalf("load repository capacity: %v", err)
	}
	quotas, err := loadClusterQueueQuotas(inputs)
	if err != nil {
		t.Fatalf("load repository quotas: %v", err)
	}
	if _, err := validateCapacity(limits, quotas); err != nil {
		t.Fatalf("repository fixtures violate declared capacity: %v", err)
	}
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return path
}
