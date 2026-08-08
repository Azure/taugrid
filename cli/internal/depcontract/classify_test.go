// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package depcontract

import (
	"strings"
	"testing"
)

const jobFixture = `
apiVersion: batch/v1
kind: Job
metadata:
  name: demo-job
  namespace: ray
spec:
  template:
    spec:
      serviceAccountName: tau-workload
      imagePullSecrets:
        - name: acr-secret
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: blob-training
        - name: script
          emptyDir: {}
      initContainers:
        - name: tau-payload
          image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
          env:
            - name: HF_TOKEN
              valueFrom:
                secretKeyRef:
                  name: hf-token
                  key: token
      containers:
        - name: main
          image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
          env:
            - name: HF_TOKEN
              valueFrom:
                secretKeyRef:
                  name: hf-token
                  key: token
            - name: LOG_LEVEL
              value: debug
          envFrom:
            - secretRef:
                name: team-bulk-secrets
          volumeMounts:
            - name: data
              mountPath: /data
`

func TestClassifyJob(t *testing.T) {
	workloads, err := Classify([]byte(jobFixture))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(workloads) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(workloads))
	}
	wd := workloads[0]
	if wd.Kind != "Job" || wd.Name != "demo-job" || wd.Namespace != "ray" {
		t.Fatalf("unexpected workload identity: %+v", wd)
	}

	byKind := indexByKind(wd.Dependencies)

	sa := mustOne(t, byKind[KindServiceAccount])
	if sa.Name != "tau-workload" || sa.Namespace != "ray" {
		t.Fatalf("unexpected ServiceAccount dependency: %+v", sa)
	}
	if !sa.HasRole(RoleJob) {
		t.Fatalf("ServiceAccount dependency missing job role: %+v", sa)
	}

	ips := mustOne(t, byKind[KindImagePullSecret])
	if ips.Name != "acr-secret" || ips.Namespace != "ray" {
		t.Fatalf("unexpected ImagePullSecret dependency: %+v", ips)
	}

	pvc := mustOne(t, byKind[KindPersistentVolumeClaim])
	if pvc.Name != "blob-training" || pvc.Namespace != "ray" {
		t.Fatalf("unexpected PVC dependency: %+v", pvc)
	}

	img := mustOne(t, byKind[KindImage])
	if img.Namespace != "" {
		t.Fatalf("image dependency should not be namespaced: %+v", img)
	}
	if img.Name != "mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0" {
		t.Fatalf("unexpected image dependency: %+v", img)
	}

	// hf-token is referenced by both the init container and the main
	// container's secretKeyRef -> a single deduped Secret dependency.
	// team-bulk-secrets is referenced only via envFrom on the main
	// container -> a second, distinct Secret dependency.
	secrets := byKind[KindSecret]
	if len(secrets) != 2 {
		t.Fatalf("expected 2 distinct Secret dependencies (hf-token, team-bulk-secrets), got %d: %+v", len(secrets), secrets)
	}
	names := map[string]Dependency{}
	for _, s := range secrets {
		names[s.Name] = s
	}
	if _, ok := names["hf-token"]; !ok {
		t.Fatalf("expected hf-token secret dependency, got %+v", secrets)
	}
	if _, ok := names["team-bulk-secrets"]; !ok {
		t.Fatalf("expected team-bulk-secrets secret dependency, got %+v", secrets)
	}

	// LOG_LEVEL is a plain env literal ("debug") — portable, must never
	// surface as a dependency of any kind, and its value must never leak
	// into any Dependency field.
	for _, d := range wd.Dependencies {
		if strings.Contains(d.Name, "debug") || strings.Contains(d.Detail, "debug") {
			t.Fatalf("plain env literal value leaked into classified dependency: %+v", d)
		}
	}

	// No unsupported dependencies in this fixture.
	if len(byKind[KindUnsupported]) != 0 {
		t.Fatalf("expected no Unsupported dependencies, got %+v", byKind[KindUnsupported])
	}
}

const rayJobFixture = `
apiVersion: ray.io/v1
kind: RayJob
metadata:
  name: demo-rayjob
  namespace: ray
spec:
  entrypoint: python train.py
  rayClusterSpec:
    rayVersion: "2.54.0"
    headGroupSpec:
      template:
        spec:
          imagePullSecrets:
            - name: acr-secret
          volumes:
            - name: data
              persistentVolumeClaim:
                claimName: blob-training
            - name: script
              emptyDir: {}
          initContainers:
            - name: tau-payload
              image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
          containers:
            - name: ray-head
              image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
              env:
                - name: HF_TOKEN
                  valueFrom:
                    secretKeyRef:
                      name: hf-token
                      key: token
    workerGroupSpecs:
      - groupName: workers
        replicas: 2
        template:
          spec:
            volumes:
              - name: data
                persistentVolumeClaim:
                  claimName: blob-training
            containers:
              - name: ray-worker
                image: mcr.microsoft.com/aks/ai-runtime/ray:py3.12-ray2.54.0-cuda13.0
                env:
                  - name: HF_TOKEN
                    valueFrom:
                      secretKeyRef:
                        name: hf-token
                        key: token
                  - name: WANDB_API_KEY
                    valueFrom:
                      secretKeyRef:
                        name: wandb-api-key
                        key: token
`

func TestClassifyRayJob(t *testing.T) {
	workloads, err := Classify([]byte(rayJobFixture))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(workloads) != 1 {
		t.Fatalf("expected 1 workload, got %d", len(workloads))
	}
	wd := workloads[0]
	if wd.Kind != "RayJob" || wd.Name != "demo-rayjob" {
		t.Fatalf("unexpected workload identity: %+v", wd)
	}

	byKind := indexByKind(wd.Dependencies)

	// blob-training PVC is referenced by both head and worker templates
	// -> one deduped dependency with both roles recorded.
	pvc := mustOne(t, byKind[KindPersistentVolumeClaim])
	if pvc.Name != "blob-training" {
		t.Fatalf("unexpected PVC dependency: %+v", pvc)
	}
	if !pvc.HasRole(RoleHead) || !pvc.HasRole(RoleWorker) {
		t.Fatalf("expected blob-training PVC to be referenced by both head and worker roles, got %+v", pvc.Roles)
	}

	// acr-secret imagePullSecret is only on the head template.
	ips := mustOne(t, byKind[KindImagePullSecret])
	if !ips.HasRole(RoleHead) || ips.HasRole(RoleWorker) {
		t.Fatalf("expected acr-secret to be head-only, got roles %+v", ips.Roles)
	}

	secrets := byKind[KindSecret]
	if len(secrets) != 2 {
		t.Fatalf("expected 2 distinct secrets (hf-token, wandb-api-key), got %d: %+v", len(secrets), secrets)
	}
	byName := map[string]Dependency{}
	for _, s := range secrets {
		byName[s.Name] = s
	}
	hf, ok := byName["hf-token"]
	if !ok {
		t.Fatalf("expected hf-token dependency, got %+v", secrets)
	}
	if !hf.HasRole(RoleHead) || !hf.HasRole(RoleWorker) {
		t.Fatalf("expected hf-token to be referenced by both head and worker, got %+v", hf.Roles)
	}
	wandb, ok := byName["wandb-api-key"]
	if !ok {
		t.Fatalf("expected wandb-api-key dependency, got %+v", secrets)
	}
	if !wandb.HasRole(RoleWorker) || wandb.HasRole(RoleHead) {
		t.Fatalf("expected wandb-api-key to be worker-only, got %+v", wandb.Roles)
	}
}

const unsupportedFixture = `
apiVersion: batch/v1
kind: Job
metadata:
  name: unsupported-job
  namespace: ray
spec:
  template:
    spec:
      volumes:
        - name: legacy-config
          configMap:
            name: legacy-config-map
        - name: host-path
          hostPath:
            path: /var/run/whatever
      containers:
        - name: main
          image: example.com/image:latest
          env:
            - name: FROM_CONFIGMAP
              valueFrom:
                configMapKeyRef:
                  name: legacy-config-map
                  key: value
          envFrom:
            - configMapRef:
                name: bulk-legacy-config
`

func TestClassifyUnsupportedDependencies(t *testing.T) {
	workloads, err := Classify([]byte(unsupportedFixture))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	byKind := indexByKind(workloads[0].Dependencies)
	unsupported := byKind[KindUnsupported]
	if len(unsupported) != 3 {
		t.Fatalf("expected 3 Unsupported dependencies (configMap volume, configMapKeyRef, configMapRef+hostPath merges to same name only if identical name), got %d: %+v", len(unsupported), unsupported)
	}
	names := map[string]int{}
	for _, d := range unsupported {
		names[d.Name]++
		if d.Detail == "" {
			t.Fatalf("Unsupported dependency must carry a Detail explaining why: %+v", d)
		}
	}
	if names["legacy-config-map"] == 0 {
		t.Fatalf("expected legacy-config-map to be flagged unsupported, got %+v", unsupported)
	}
	if names["bulk-legacy-config"] == 0 {
		t.Fatalf("expected bulk-legacy-config to be flagged unsupported, got %+v", unsupported)
	}
	if names["host-path"] == 0 {
		t.Fatalf("expected host-path volume to be flagged unsupported, got %+v", unsupported)
	}
}

const secretProviderClassFixture = `
apiVersion: batch/v1
kind: Job
metadata:
  name: kv-job
  namespace: ray
spec:
  template:
    spec:
      volumes:
        - name: kv-secrets
          csi:
            driver: secrets-store.csi.k8s.io
            readOnly: true
            volumeAttributes:
              secretProviderClass: kv-job-spc
        - name: weird-csi
          csi:
            driver: some.other.csi.driver
            volumeAttributes:
              foo: bar
      containers:
        - name: main
          image: example.com/image:latest
`

func TestClassifySecretProviderClassAndUnknownCSI(t *testing.T) {
	workloads, err := Classify([]byte(secretProviderClassFixture))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	byKind := indexByKind(workloads[0].Dependencies)

	spc := mustOne(t, byKind[KindSecretProviderClass])
	if spc.Name != "kv-job-spc" {
		t.Fatalf("unexpected SecretProviderClass dependency: %+v", spc)
	}
	if !strings.Contains(spc.Detail, "secrets-store.csi.k8s.io") {
		t.Fatalf("expected SecretProviderClass detail to name the CSI driver: %+v", spc)
	}

	unsupported := mustOne(t, byKind[KindUnsupported])
	if unsupported.Name != "weird-csi" {
		t.Fatalf("unexpected Unsupported dependency for unknown CSI driver: %+v", unsupported)
	}
	if !strings.Contains(unsupported.Detail, "some.other.csi.driver") {
		t.Fatalf("expected detail to name the unknown CSI driver: %+v", unsupported)
	}
}

func TestClassifyNeverExposesRedactedSecretValues(t *testing.T) {
	// This mirrors what a client dry-run redacted manifest looks like
	// (internal/envspec.RedactSecretRefs replaces name/key with
	// "<redacted>"). The classifier must still work mechanically — it only
	// reads whatever name is present — but the redacted name must be
	// clearly identifiable so a caller (internal/platform) can refuse to
	// preflight-check it instead of reporting a false pass/fail.
	redacted := strings.ReplaceAll(jobFixture, "name: hf-token", "name: <redacted>")
	redacted = strings.ReplaceAll(redacted, "key: token", "key: <redacted>")
	workloads, err := Classify([]byte(redacted))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	byKind := indexByKind(workloads[0].Dependencies)
	var found bool
	for _, s := range byKind[KindSecret] {
		if s.Name == RedactedPlaceholder {
			found = true
			if !s.IsRedactedPlaceholder() {
				t.Fatalf("IsRedactedPlaceholder() should be true for %+v", s)
			}
		}
	}
	if !found {
		t.Fatalf("expected a Secret dependency named %q from the redacted fixture", RedactedPlaceholder)
	}
}

func TestFlattenMergesAcrossWorkloads(t *testing.T) {
	workloads, err := Classify([]byte(jobFixture + "\n---\n" + rayJobFixture))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(workloads) != 2 {
		t.Fatalf("expected 2 workloads, got %d", len(workloads))
	}
	flat := Flatten(workloads)
	byKind := indexByKind(flat)

	// blob-training PVC is referenced by the Job (role=job) and by both
	// the RayJob's head and worker templates -> merged into a single
	// dependency with all three roles present.
	pvc := mustOne(t, byKind[KindPersistentVolumeClaim])
	for _, role := range []Role{RoleJob, RoleHead, RoleWorker} {
		if !pvc.HasRole(role) {
			t.Fatalf("expected merged PVC dependency to carry role %q, got %+v", role, pvc.Roles)
		}
	}

	// hf-token is referenced by the Job and by both RayJob pod templates.
	secrets := byKind[KindSecret]
	var hfCount int
	for _, s := range secrets {
		if s.Name == "hf-token" {
			hfCount++
		}
	}
	if hfCount != 1 {
		t.Fatalf("expected hf-token to be deduped into exactly one dependency across workloads, found %d", hfCount)
	}
}

func TestClassifyIgnoresNonWorkloadDocuments(t *testing.T) {
	manifest := jobFixture + "\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: unrelated\ndata:\n  key: value\n"
	workloads, err := Classify([]byte(manifest))
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if len(workloads) != 1 {
		t.Fatalf("expected ConfigMap document to be ignored, got %d workloads", len(workloads))
	}
}

func TestClassifyRejectsInvalidYAML(t *testing.T) {
	_, err := Classify([]byte("not: [valid"))
	if err == nil {
		t.Fatalf("expected an error for invalid YAML")
	}
}

func indexByKind(deps []Dependency) map[Kind][]Dependency {
	out := map[Kind][]Dependency{}
	for _, d := range deps {
		out[d.Kind] = append(out[d.Kind], d)
	}
	return out
}

func mustOne(t *testing.T, deps []Dependency) Dependency {
	t.Helper()
	if len(deps) != 1 {
		t.Fatalf("expected exactly 1 dependency, got %d: %+v", len(deps), deps)
	}
	return deps[0]
}
