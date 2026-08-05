package blobstore

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"

	"github.com/Azure/taugrid/core/fileutil"
)

func TestParseDurableRefPreservesLegacyBlobPath(t *testing.T) {
	ref, err := ParseDurableRef(`{"schema_version":"tau.blobref.v1","uri":"azblob://acct.blob.core.windows.net/tau-artifacts/v1/project=vision/question=legacy-experiment/run=run-1/image/ab/confusion.png","digest":"sha256:abc","size_bytes":42,"content_type":"image/png","uploaded_at":"2026-06-17T00:00:00Z"}`)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Account != "acct" || ref.Container != "tau-artifacts" || ref.BlobPath != "v1/project=vision/question=legacy-experiment/run=run-1/image/ab/confusion.png" {
		t.Fatalf("unexpected normalized ref: %+v", ref)
	}
}

func TestAzureRefDerivesPrivateEndpointAccountURL(t *testing.T) {
	ref := DurableRef{
		SchemaVersion: DurableRefSchemaVersion,
		URI:           "https://acct.privatelink.blob.core.windows.net/tau-artifacts/v2/project=vision/experiment=image-resnet/run=run-1/image/confusion.png",
		Digest:        "sha256:abc",
		SizeBytes:     42,
		ContentType:   "image/png",
		UploadedAt:    "2026-06-17T00:00:00Z",
	}
	normalized, err := NormalizeAzureRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Account != "acct" || normalized.Container != "tau-artifacts" || normalized.BlobPath != "v2/project=vision/experiment=image-resnet/run=run-1/image/confusion.png" {
		t.Fatalf("unexpected normalized private endpoint ref: %+v", normalized)
	}
	if got := AzureAccountURL(normalized); got != "https://acct.privatelink.blob.core.windows.net" {
		t.Fatalf("AzureAccountURL = %q", got)
	}
}

func TestAzureStoreUploadsAndVerifiesWithInjectedClient(t *testing.T) {
	ctx := context.Background()
	fake := &fakeAzureBlobClient{}
	store, err := NewAzureStore(ctx, AzureOptions{
		Account:    "acct",
		Container:  "tau-artifacts",
		BlobClient: fake,
	})
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "confusion.png")
	payload := []byte("png-bytes")
	if err := os.WriteFile(source, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	digest, size, err := fileutil.FileSHA256(source)
	if err != nil {
		t.Fatal(err)
	}
	ref := NewDurableRef(Partition{
		Account:      "acct",
		Container:    "tau-artifacts",
		BaseURI:      store.BaseURI,
		Project:      "vision",
		ExperimentID: "image-resnet",
		RunGroupID:   "h200",
		RunID:        "run-1",
		ArtifactType: "image",
		ArtifactName: "confusion.png",
		Digest:       digest,
	}, size, "image/png", time.Date(2026, 6, 17, 0, 0, 0, 0, time.UTC))

	uploaded, err := store.UploadFile(ctx, ref, source)
	if err != nil {
		t.Fatal(err)
	}
	if uploaded.URI == "" || !strings.HasPrefix(uploaded.URI, "azblob://acct.blob.core.windows.net/tau-artifacts/") {
		t.Fatalf("uploaded URI = %q", uploaded.URI)
	}
	if !strings.Contains(uploaded.BlobPath, "/experiment=image-resnet/") {
		t.Fatalf("uploaded blob path does not contain the experiment partition: %q", uploaded.BlobPath)
	}
	if fake.uploadContainer != "tau-artifacts" || fake.uploadBlob != ref.BlobPath {
		t.Fatalf("unexpected upload target: container=%q blob=%q", fake.uploadContainer, fake.uploadBlob)
	}
	if fake.contentType != "image/png" {
		t.Fatalf("content type = %q, want image/png", fake.contentType)
	}
	if fake.metadata["tau_digest"] == nil || *fake.metadata["tau_digest"] != ref.Digest {
		t.Fatalf("missing digest metadata: %+v", fake.metadata)
	}
}

type fakeAzureBlobClient struct {
	uploadContainer string
	uploadBlob      string
	contentType     string
	metadata        map[string]*string
	body            []byte
}

func (f *fakeAzureBlobClient) UploadFile(_ context.Context, containerName string, blobName string, file *os.File, o *azblob.UploadFileOptions) (azblob.UploadFileResponse, error) {
	body, err := io.ReadAll(file)
	if err != nil {
		return azblob.UploadFileResponse{}, err
	}
	f.uploadContainer = containerName
	f.uploadBlob = blobName
	f.body = body
	if o != nil {
		f.metadata = o.Metadata
		if o.HTTPHeaders != nil && o.HTTPHeaders.BlobContentType != nil {
			f.contentType = *o.HTTPHeaders.BlobContentType
		}
	}
	return azblob.UploadFileResponse{}, nil
}

func (f *fakeAzureBlobClient) DownloadStream(_ context.Context, containerName string, blobName string, _ *azblob.DownloadStreamOptions) (azblob.DownloadStreamResponse, error) {
	f.uploadContainer = containerName
	f.uploadBlob = blobName
	return azblob.DownloadStreamResponse{
		DownloadResponse: blob.DownloadResponse{
			Body: io.NopCloser(bytes.NewReader(f.body)),
		},
	}, nil
}
