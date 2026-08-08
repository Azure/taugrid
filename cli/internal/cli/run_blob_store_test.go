package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeBlobVolumeReader struct {
	responses map[string]string
	errors    map[string]error
	calls     []string
}

func TestRunBlobVolumeAnnotationParsesTrustedEndpoint(t *testing.T) {
	want := runBlobVolume{
		SchemaVersion: runBlobVolumeSchema,
		AccountURL:    "https://trainingacct.blob.core.windows.net",
		Container:     "results",
	}
	got, err := parseRunBlobVolume(`{"schema_version":"tau.run.blob-volume.v1","account_url":"https://trainingacct.blob.core.windows.net","container":"results"}`)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestRunBlobVolumeAnnotationRejectsCredentialBearingURL(t *testing.T) {
	_, err := parseRunBlobVolume(`{"schema_version":"tau.run.blob-volume.v1","account_url":"https://user:secret@example.test","container":"results"}`)
	if err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("credential-bearing URL error = %v", err)
	}
}

func TestRunBlobVolumeAnnotationRejectsUntrustedHost(t *testing.T) {
	_, err := parseRunBlobVolume(`{"schema_version":"tau.run.blob-volume.v1","account_url":"https://attacker.example","container":"results"}`)
	if err == nil || !strings.Contains(err.Error(), "trusted Azure Blob endpoint") {
		t.Fatalf("untrusted host error = %v", err)
	}
}

func TestAzureRunArtifactStoreRejectsUntrustedHostBeforeCredentialUse(t *testing.T) {
	_, err := newAzureRunArtifactStore(runBlobVolume{
		SchemaVersion: runBlobVolumeSchema,
		AccountURL:    "https://attacker.example",
		Container:     "results",
	})
	if err == nil || !strings.Contains(err.Error(), "trusted Azure Blob endpoint") {
		t.Fatalf("untrusted store error = %v", err)
	}
}

func (f *fakeBlobVolumeReader) Raw(_ context.Context, args []string, _ []byte) (string, error) {
	key := strings.Join(args, " ")
	f.calls = append(f.calls, key)
	if err := f.errors[key]; err != nil {
		return "", err
	}
	return f.responses[key], nil
}

func TestResolveRunBlobVolumeFromDynamicCSIHandle(t *testing.T) {
	reader := &fakeBlobVolumeReader{responses: map[string]string{
		"get pvc blob-training -n research -o json": `{"spec":{"volumeName":"pvc-123"}}`,
		"get pv pvc-123 -o json":                    `{"spec":{"csi":{"driver":"blob.csi.azure.com","volumeHandle":"rg#trainingacct#results#uuid#research#subscription","volumeAttributes":{"storageEndpointSuffix":"core.windows.net"}}}}`,
	}}
	volume, err := resolveRunBlobVolume(context.Background(), reader, "research", "blob-training")
	if err != nil {
		t.Fatal(err)
	}
	if volume.AccountURL != "https://trainingacct.blob.core.windows.net" || volume.Container != "results" {
		t.Fatalf("resolved volume = %+v", volume)
	}
	if len(reader.calls) != 2 {
		t.Fatalf("calls = %v", reader.calls)
	}
}

func TestResolveRunBlobVolumePrefersStaticAttributesAndPrivateServer(t *testing.T) {
	reader := &fakeBlobVolumeReader{responses: map[string]string{
		"get pvc blob-training -n research -o json": `{"spec":{"volumeName":"static-pv"}}`,
		"get pv static-pv -o json":                  `{"spec":{"csi":{"driver":"blob.csi.azure.com","volumeHandle":"opaque","volumeAttributes":{"storageAccount":"trainingacct","containerName":"artifacts","server":"trainingacct.privatelink.blob.core.windows.net"}}}}`,
	}}
	volume, err := resolveRunBlobVolume(context.Background(), reader, "research", "blob-training")
	if err != nil {
		t.Fatal(err)
	}
	if volume.AccountURL != "https://trainingacct.privatelink.blob.core.windows.net" || volume.Container != "artifacts" {
		t.Fatalf("resolved volume = %+v", volume)
	}
}

func TestResolveRunBlobVolumeRejectsUnsupportedStorageWithoutCreatingReader(t *testing.T) {
	reader := &fakeBlobVolumeReader{responses: map[string]string{
		"get pvc shared -n research -o json": `{"spec":{"volumeName":"nfs-pv"}}`,
		"get pv nfs-pv -o json":              `{"spec":{"csi":{"driver":"file.csi.azure.com","volumeHandle":"opaque"}}}`,
	}}
	_, err := resolveRunBlobVolume(context.Background(), reader, "research", "shared")
	if err == nil || !strings.Contains(err.Error(), "will not create a PVC-reader pod") {
		t.Fatalf("unsupported storage error = %v", err)
	}
}

func TestResolveRunBlobVolumeDoesNotReadSecretBackedIdentity(t *testing.T) {
	reader := &fakeBlobVolumeReader{responses: map[string]string{
		"get pvc shared -n research -o json": `{"spec":{"volumeName":"secret-pv"}}`,
		"get pv secret-pv -o json":           `{"spec":{"csi":{"driver":"blob.csi.azure.com","volumeHandle":"opaque","volumeAttributes":{"secretName":"storage-key"}}}}`,
	}}
	_, err := resolveRunBlobVolume(context.Background(), reader, "research", "shared")
	if err == nil || !strings.Contains(err.Error(), "will not read Secret credentials") {
		t.Fatalf("secret-backed identity error = %v", err)
	}
	for _, call := range reader.calls {
		if strings.Contains(call, " secret ") {
			t.Fatalf("resolver attempted to read a Secret: %v", reader.calls)
		}
	}
}

func TestResolveRunBlobVolumeSurfacesMetadataAuthorizationFailure(t *testing.T) {
	reader := &fakeBlobVolumeReader{
		responses: map[string]string{},
		errors: map[string]error{
			"get pvc blob-training -n research -o json": errors.New("forbidden"),
		},
	}
	_, err := resolveRunBlobVolume(context.Background(), reader, "research", "blob-training")
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("authorization error = %v", err)
	}
}
