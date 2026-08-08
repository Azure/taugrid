// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package platform

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/depcontract"
)

// fakeRunner is a minimal, no-live-cluster stand-in for internal/kube.Runner
// used only by these tests. Responses are keyed by the joined args so tests
// can assert exactly which read-only kubectl calls preflight makes.
type fakeRunner struct {
	responses map[string]string // joined-args -> stdout
	missing   map[string]bool   // joined-args -> simulate "not found" error
	calls     []string
}

func (f *fakeRunner) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if f.missing[key] {
		return "", fmt.Errorf("Error from server (NotFound): %s not found", key)
	}
	out, ok := f.responses[key]
	if !ok {
		return "", fmt.Errorf("unexpected kubectl args: %s", key)
	}
	return out, nil
}

func pvcJSON(storageClass string) string {
	return pvcJSONWithPhase(storageClass, "Bound")
}

func pvcJSONWithPhase(storageClass, phase string) string {
	return fmt.Sprintf(`{"spec":{"storageClassName":%q},"status":{"phase":%q}}`, storageClass, phase)
}

func pvcJSONMissingPhase(storageClass string) string {
	return fmt.Sprintf(`{"spec":{"storageClassName":%q},"status":{}}`, storageClass)
}

func pvcJSONMissingStorageClass() string {
	return `{"spec":{},"status":{"phase":"Bound"}}`
}

func slicesContains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

func TestCheckMultiKueuePreflight_AllHealthy(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get secret hf-token -o name":                                         "secret/hf-token",
		"-n ray get serviceaccount tau-workload -o name":                             "serviceaccount/tau-workload",
		"-n ray get secret acr-secret -o name":                                       "secret/acr-secret",
		"-n ray get pvc blob-training -o json":                                       pvcJSON("blob-premium-rwx"),
		"get storageclass blob-premium-rwx -o name":                                  "storageclass.storage.k8s.io/blob-premium-rwx",
		"-n ray get secretproviderclasses.secrets-store.csi.x-k8s.io kv-spc -o name": "secretproviderclass.secrets-store.csi.x-k8s.io/kv-spc",
	}}
	workerB := &fakeRunner{responses: map[string]string{
		"-n ray get secret hf-token -o name":                                         "secret/hf-token",
		"-n ray get serviceaccount tau-workload -o name":                             "serviceaccount/tau-workload",
		"-n ray get secret acr-secret -o name":                                       "secret/acr-secret",
		"-n ray get pvc blob-training -o json":                                       pvcJSON("blob-premium-rwx"),
		"get storageclass blob-premium-rwx -o name":                                  "storageclass.storage.k8s.io/blob-premium-rwx",
		"-n ray get secretproviderclasses.secrets-store.csi.x-k8s.io kv-spc -o name": "secretproviderclass.secrets-store.csi.x-k8s.io/kv-spc",
	}}

	deps := []depcontract.Dependency{
		{Kind: depcontract.KindSecret, Namespace: "ray", Name: "hf-token", Roles: []depcontract.Role{depcontract.RoleJob}},
		{Kind: depcontract.KindServiceAccount, Namespace: "ray", Name: "tau-workload", Roles: []depcontract.Role{depcontract.RoleJob}},
		{Kind: depcontract.KindImagePullSecret, Namespace: "ray", Name: "acr-secret", Roles: []depcontract.Role{depcontract.RoleJob}},
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "ray", Name: "blob-training", Roles: []depcontract.Role{depcontract.RoleJob}},
		{Kind: depcontract.KindSecretProviderClass, Namespace: "ray", Name: "kv-spc", Roles: []depcontract.Role{depcontract.RoleJob}},
		{Kind: depcontract.KindImage, Name: "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0"},
	}

	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{NamespaceOverride: "ray"})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected report to be OK, got failures: %+v", report.Failures())
	}
	if err := report.Err(); err != nil {
		t.Fatalf("expected nil Err(), got %v", err)
	}

	var infoCount int
	for _, res := range report.Results {
		if res.Status == StatusInfo {
			infoCount++
			if res.Kind != depcontract.KindImage {
				t.Fatalf("expected only Image results to be Info, got %+v", res)
			}
		}
	}
	if infoCount != 1 {
		t.Fatalf("expected exactly 1 info result (the image), got %d", infoCount)
	}
}

func TestCheckMultiKueuePreflight_MissingSecretNamesWorker(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get secret hf-token -o name": "secret/hf-token",
	}}
	workerB := &fakeRunner{missing: map[string]bool{
		"-n ray get secret hf-token -o name": true,
	}}

	deps := []depcontract.Dependency{
		{Kind: depcontract.KindSecret, Namespace: "ray", Name: "hf-token"},
	}
	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if report.OK() {
		t.Fatalf("expected report to have failures")
	}
	reportErr := report.Err()
	if reportErr == nil {
		t.Fatalf("expected non-nil Err()")
	}
	msg := reportErr.Error()
	if !strings.Contains(msg, "Secret") || !strings.Contains(msg, "hf-token") {
		t.Fatalf("error must name the dependency kind/name, got: %s", msg)
	}
	if !strings.Contains(msg, "worker-b") {
		t.Fatalf("error must name the affected worker context, got: %s", msg)
	}
	if strings.Contains(msg, "worker-a:") {
		t.Fatalf("worker-a passed and must not be listed as a failing context, got: %s", msg)
	}
}

func TestCheckMultiKueuePreflight_MultiWorkerFailuresAllNamed(t *testing.T) {
	workerA := &fakeRunner{missing: map[string]bool{"-n ray get serviceaccount tau-workload -o name": true}}
	workerB := &fakeRunner{missing: map[string]bool{"-n ray get serviceaccount tau-workload -o name": true}}

	deps := []depcontract.Dependency{
		{Kind: depcontract.KindServiceAccount, Namespace: "ray", Name: "tau-workload"},
	}
	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	err := report.Err()
	if err == nil {
		t.Fatalf("expected an error")
	}
	msg := err.Error()
	for _, want := range []string{"ServiceAccount", "tau-workload", "worker-a", "worker-b"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckMultiKueuePreflight_PVCMissingOnOneWorker(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json": pvcJSON("blob-premium-rwx"),
	}}
	workerB := &fakeRunner{missing: map[string]bool{"-n ray get pvc blob-training -o json": true}}

	deps := []depcontract.Dependency{
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "ray", Name: "blob-training"},
	}
	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	err := report.Err()
	if err == nil {
		t.Fatalf("expected an error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "PersistentVolumeClaim") || !strings.Contains(msg, "blob-training") || !strings.Contains(msg, "worker-b") ||
		!strings.Contains(msg, "platform-managed PVC") || !strings.Contains(msg, "pre-provision and bind it") {
		t.Fatalf("expected PVC failure to name kind/name/worker, got: %s", msg)
	}
	// worker-a has the PVC but no StorageClass check response was
	// registered because worker-b never resolved a class name to check
	// parity against — confirm no panics / unexpected calls happened.
}

func TestCheckMultiKueuePreflight_PVCPhaseBoundAcrossWorkersPasses(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json":      pvcJSON("blob-premium-rwx"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
	}}
	workerB := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json":      pvcJSON("blob-premium-rwx"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "ray", Name: "blob-training"},
	}
	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected Bound PVCs on all workers to pass, got failures: %+v", report.Failures())
	}
}

func TestCheckMultiKueuePreflight_PVCPendingFailsButStillChecksStorageClassParity(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json":      pvcJSONWithPhase("blob-premium-rwx", "Pending"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
	}}
	workerB := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json":      pvcJSON("blob-premium-rwx"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "ray", Name: "blob-training"},
	}
	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	err := report.Err()
	if err == nil {
		t.Fatalf("expected Pending PVC to fail preflight")
	}
	msg := err.Error()
	for _, want := range []string{"PersistentVolumeClaim", "blob-training", "worker-a", "phase=Pending", "platform-managed PVC", "platform storage lifecycle"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected pending-phase failure to mention %q, got: %s", want, msg)
		}
	}
	for _, want := range []string{
		"get storageclass blob-premium-rwx -o name",
	} {
		if !slicesContains(workerA.calls, want) || !slicesContains(workerB.calls, want) {
			t.Fatalf("expected StorageClass parity diagnostics to run even when one worker is Pending; worker-a=%v worker-b=%v", workerA.calls, workerB.calls)
		}
	}
}

func TestCheckMultiKueuePreflight_PVCEmptyPhaseFails(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json": pvcJSONWithPhase("blob-premium-rwx", ""),
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "ray", Name: "blob-training"},
	}
	workers := []Worker{{Context: "worker-a", Runner: workerA}}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	err := report.Err()
	if err == nil || !strings.Contains(err.Error(), "phase=<empty>") {
		t.Fatalf("expected empty PVC phase to fail with an explicit phase marker, got: %v", err)
	}
}

func TestCheckMultiKueuePreflight_PVCMissingPhaseFails(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json": pvcJSONMissingPhase("blob-premium-rwx"),
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "ray", Name: "blob-training"},
	}
	workers := []Worker{{Context: "worker-a", Runner: workerA}}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	err := report.Err()
	if err == nil || !strings.Contains(err.Error(), "phase=<missing>") {
		t.Fatalf("expected missing PVC phase to fail with an explicit phase marker, got: %v", err)
	}
}

func TestCheckMultiKueuePreflight_StorageClassMismatchAcrossWorkers(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json":      pvcJSON("blob-premium-rwx"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
		"get storageclass amlfs-rwx -o name":        "storageclass.storage.k8s.io/amlfs-rwx",
	}}
	workerB := &fakeRunner{responses: map[string]string{
		"-n ray get pvc blob-training -o json":      pvcJSON("amlfs-rwx"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
		"get storageclass amlfs-rwx -o name":        "storageclass.storage.k8s.io/amlfs-rwx",
	}}

	deps := []depcontract.Dependency{
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "ray", Name: "blob-training"},
	}
	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	err := report.Err()
	if err == nil {
		t.Fatalf("expected a StorageClass parity error")
	}
	msg := err.Error()
	if !strings.Contains(msg, "StorageClass") || !strings.Contains(msg, "blob-training") {
		t.Fatalf("expected StorageClass mismatch to name the PVC, got: %s", msg)
	}
	if !strings.Contains(msg, "worker-a") || !strings.Contains(msg, "worker-b") {
		t.Fatalf("expected mismatch error to name both worker contexts and their class names, got: %s", msg)
	}
}

func TestCheckMultiKueuePreflight_SamePVCNameDifferentNamespaces_CheckedSeparately(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n team-a get pvc shared-data -o json":     pvcJSON("blob-premium-rwx"),
		"-n team-b get pvc shared-data -o json":     pvcJSON("amlfs-rwx"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
		"get storageclass amlfs-rwx -o name":        "storageclass.storage.k8s.io/amlfs-rwx",
	}}
	workerB := &fakeRunner{responses: map[string]string{
		"-n team-a get pvc shared-data -o json":     pvcJSON("blob-premium-rwx"),
		"-n team-b get pvc shared-data -o json":     pvcJSON("amlfs-rwx"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
		"get storageclass amlfs-rwx -o name":        "storageclass.storage.k8s.io/amlfs-rwx",
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "team-a", Name: "shared-data"},
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "team-b", Name: "shared-data"},
	}
	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected same-named PVCs in different namespaces to be checked independently, got failures: %+v", report.Failures())
	}
	summary := report.Summary()
	for _, want := range []string{"team-a/shared-data", "team-b/shared-data", "blob-premium-rwx", "amlfs-rwx"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("expected summary to mention %q, got:\n%s", want, summary)
		}
	}
	for _, want := range []string{
		"-n team-a get pvc shared-data -o json",
		"-n team-b get pvc shared-data -o json",
		"get storageclass blob-premium-rwx -o name",
		"get storageclass amlfs-rwx -o name",
	} {
		if !slicesContains(workerA.calls, want) {
			t.Fatalf("worker-a calls missing %q: %+v", want, workerA.calls)
		}
		if !slicesContains(workerB.calls, want) {
			t.Fatalf("worker-b calls missing %q: %+v", want, workerB.calls)
		}
	}
}

func TestCheckMultiKueuePreflight_SamePVCNameDifferentNamespaces_ReportQualifiedFailures(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n team-a get pvc shared-data -o json":     pvcJSON("blob-premium-rwx"),
		"-n team-b get pvc shared-data -o json":     pvcJSON("amlfs-rwx"),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
		"get storageclass amlfs-rwx -o name":        "storageclass.storage.k8s.io/amlfs-rwx",
	}}
	workerB := &fakeRunner{responses: map[string]string{
		"-n team-a get pvc shared-data -o json":     pvcJSON("blob-premium-rwx"),
		"-n team-b get pvc shared-data -o json":     pvcJSONMissingStorageClass(),
		"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
		"get storageclass amlfs-rwx -o name":        "storageclass.storage.k8s.io/amlfs-rwx",
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "team-a", Name: "shared-data"},
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "team-b", Name: "shared-data"},
	}
	workers := []Worker{
		{Context: "worker-a", Runner: workerA},
		{Context: "worker-b", Runner: workerB},
	}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	err := report.Err()
	if err == nil {
		t.Fatalf("expected a parity error for team-b/shared-data")
	}
	msg := err.Error()
	if !strings.Contains(msg, "StorageClass team-b/shared-data") {
		t.Fatalf("expected failure to identify the namespaced PVC key, got: %s", msg)
	}
	if !strings.Contains(msg, `manifest: PVC "team-b/shared-data" resolves to different StorageClass names across workers: worker-a="amlfs-rwx", worker-b=""`) {
		t.Fatalf("expected mismatch details to preserve the namespace-qualified PVC name and missing StorageClass, got: %s", msg)
	}
	if strings.Contains(msg, "StorageClass team-a/shared-data failed") {
		t.Fatalf("healthy team-a/shared-data must not be reported as failed: %s", msg)
	}
	summary := report.Summary()
	for _, want := range []string{"team-a/shared-data", "team-b/shared-data"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("expected summary to mention %q, got:\n%s", want, summary)
		}
	}
}

func TestCheckMultiKueuePreflight_UnsupportedDependencyFailsIndependentOfWorkers(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindUnsupported, Namespace: "ray", Name: "legacy-config-map", Detail: "unsupported ConfigMap reference"},
	}
	workers := []Worker{{Context: "worker-a", Runner: workerA}}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	err := report.Err()
	if err == nil {
		t.Fatalf("expected Unsupported dependency to fail preflight")
	}
	if !strings.Contains(err.Error(), "legacy-config-map") {
		t.Fatalf("expected error to name the unsupported dependency, got: %s", err.Error())
	}
	if len(workerA.calls) != 0 {
		t.Fatalf("Unsupported dependency check must not issue any kubectl calls, got: %+v", workerA.calls)
	}
}

func TestCheckMultiKueuePreflight_RedactedSecretRefusesToCheck(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindSecret, Namespace: "ray", Name: depcontract.RedactedPlaceholder},
	}
	workers := []Worker{{Context: "worker-a", Runner: workerA}}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	if report.OK() {
		t.Fatalf("expected redacted secret dependency to fail preflight rather than silently pass")
	}
	if len(workerA.calls) != 0 {
		t.Fatalf("must never issue a kubectl call for a redacted placeholder name, got: %+v", workerA.calls)
	}
	err := report.Err()
	if err == nil || !strings.Contains(err.Error(), "redact") {
		t.Fatalf("expected error to explain the redaction problem, got: %v", err)
	}
}

func TestCheckMultiKueuePreflight_NamespacedDependencyWithoutNamespaceFailsBeforeKubectl(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get secret hf-token -o name": "secret/hf-token",
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindSecret, Name: "hf-token"},
	}
	workers := []Worker{{Context: "worker-a", Runner: workerA}}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if report.OK() {
		t.Fatalf("expected namespace-less namespaced dependency to fail")
	}
	if len(workerA.calls) != 0 {
		t.Fatalf("expected fail-fast before any kubectl call, got %+v", workerA.calls)
	}
	reportErr := report.Err()
	if reportErr == nil {
		t.Fatalf("expected non-nil Err()")
	}
	msg := reportErr.Error()
	for _, want := range []string{"Secret", "hf-token", "metadata.namespace", "--namespace"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("expected error to mention %q, got: %s", want, msg)
		}
	}
}

func TestCheckMultiKueuePreflight_OnlyClusterScopedDependenciesMayOmitNamespace(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindImage, Name: "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0"},
	}
	workers := []Worker{{Context: "worker-a", Runner: workerA}}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected namespace-less cluster-scoped dependency to pass, got failures: %+v", report.Failures())
	}
	if len(workerA.calls) != 0 {
		t.Fatalf("cluster-scoped image inventory must not issue kubectl calls, got %+v", workerA.calls)
	}
}

func TestCheckMultiKueuePreflight_NamespaceOverrideSuppliesMissingNamespace(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{
		"-n ray get secret hf-token -o name": "secret/hf-token",
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindSecret, Name: "hf-token"},
	}
	workers := []Worker{{Context: "worker-a", Runner: workerA}}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{NamespaceOverride: "ray"})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected namespace override to satisfy missing namespace, got failures: %+v", report.Failures())
	}
	if !slicesContains(workerA.calls, "-n ray get secret hf-token -o name") {
		t.Fatalf("expected override namespace kubectl call, got %+v", workerA.calls)
	}
	if got := report.Results[0].Namespace; got != "ray" {
		t.Fatalf("reported namespace = %q, want ray", got)
	}
}

func TestCheckMultiKueuePreflight_RequiresAtLeastOneWorker(t *testing.T) {
	_, err := CheckMultiKueuePreflight(context.Background(), nil, nil, Options{})
	if err == nil {
		t.Fatalf("expected an error when no worker contexts are supplied")
	}
}

func TestCheckMultiKueuePreflight_UnknownKindFails(t *testing.T) {
	workerA := &fakeRunner{responses: map[string]string{}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.Kind("SomethingNew"), Namespace: "ray", Name: "mystery"},
	}
	workers := []Worker{{Context: "worker-a", Runner: workerA}}

	report, _ := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	if report.OK() {
		t.Fatalf("expected an unknown dependency kind to fail closed, not pass silently")
	}
}

func TestCheckMultiKueuePreflight_NamespaceOverrideWinsOverManifestNamespace(t *testing.T) {
	worker := &fakeRunner{
		responses: map[string]string{
			"-n other get secret hf-token -o name":      "secret/hf-token",
			"-n other get pvc blob-training -o json":    pvcJSON("blob-premium-rwx"),
			"get storageclass blob-premium-rwx -o name": "storageclass.storage.k8s.io/blob-premium-rwx",
		},
		missing: map[string]bool{
			"-n ray get secret hf-token -o name":   true,
			"-n ray get pvc blob-training -o json": true,
		},
	}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindSecret, Namespace: "ray", Name: "hf-token", Roles: []depcontract.Role{depcontract.RoleJob}},
		{Kind: depcontract.KindPersistentVolumeClaim, Namespace: "ray", Name: "blob-training", Roles: []depcontract.Role{depcontract.RoleJob}},
	}
	workers := []Worker{{Context: "worker-a", Runner: worker}}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{NamespaceOverride: "other"})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected NamespaceOverride to take effect, got failures: %+v", report.Failures())
	}
	for _, res := range report.Results {
		if res.Kind == KindStorageClass {
			if res.Namespace != "" {
				t.Fatalf("cluster-scoped StorageClass namespace = %q, want empty", res.Namespace)
			}
			continue
		}
		if res.Namespace != "other" {
			t.Fatalf("expected namespaced dependency %s to report override namespace, got %+v", res.Kind, res)
		}
	}
	for _, call := range worker.calls {
		if strings.Contains(call, "-n ray ") {
			t.Fatalf("expected no kubectl call against the manifest's own namespace, got call %q", call)
		}
	}
}

func TestCheckMultiKueuePreflight_EmptyOverrideKeepsClassifiedNamespace(t *testing.T) {
	worker := &fakeRunner{responses: map[string]string{
		"-n ray get secret hf-token -o name": "secret/hf-token",
	}}
	deps := []depcontract.Dependency{
		{Kind: depcontract.KindSecret, Namespace: "ray", Name: "hf-token", Roles: []depcontract.Role{depcontract.RoleJob}},
	}
	workers := []Worker{{Context: "worker-a", Runner: worker}}

	report, err := CheckMultiKueuePreflight(context.Background(), workers, deps, Options{})
	if err != nil {
		t.Fatalf("CheckMultiKueuePreflight: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected classified namespace to be used as-is, got failures: %+v", report.Failures())
	}
	if got := report.Results[0].Namespace; got != "ray" {
		t.Fatalf("reported namespace = %q, want ray", got)
	}
}
