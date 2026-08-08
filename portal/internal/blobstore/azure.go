// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package blobstore

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
)

const AzureObjectStoreKind = "azblob"

type AzureOptions struct {
	Account     string
	Container   string
	AccountURL  string
	BaseURI     string
	Credential  azcore.TokenCredential
	BlobClient  azureBlobClient
	Concurrency uint16
}

type AzureStore struct {
	Account     string
	Container   string
	AccountURL  string
	BaseURI     string
	client      azureBlobClient
	concurrency uint16
}

type azureBlobClient interface {
	UploadFile(ctx context.Context, containerName string, blobName string, file *os.File, o *azblob.UploadFileOptions) (azblob.UploadFileResponse, error)
	DownloadStream(ctx context.Context, containerName string, blobName string, o *azblob.DownloadStreamOptions) (azblob.DownloadStreamResponse, error)
}

func NewAzureStore(ctx context.Context, opts AzureOptions) (*AzureStore, error) {
	account := strings.TrimSpace(opts.Account)
	container := strings.TrimSpace(opts.Container)
	accountURL := strings.TrimRight(strings.TrimSpace(opts.AccountURL), "/")
	if account == "" {
		return nil, fmt.Errorf("--account is required for --object-store=azblob")
	}
	if container == "" {
		return nil, fmt.Errorf("--container is required for --object-store=azblob")
	}
	if accountURL == "" {
		accountURL = "https://" + account + ".blob.core.windows.net"
	}
	baseURI := strings.TrimRight(strings.TrimSpace(opts.BaseURI), "/")
	if baseURI == "" {
		baseURI = AzureBaseURI(account, container)
	}
	client := opts.BlobClient
	if client == nil {
		if opts.Credential == nil {
			defaultCred, err := azidentity.NewDefaultAzureCredential(nil)
			if err != nil {
				return nil, err
			}
			opts.Credential = defaultCred
		}
		realClient, err := azblob.NewClient(accountURL, opts.Credential, nil)
		if err != nil {
			return nil, err
		}
		client = realClient
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &AzureStore{
		Account:     account,
		Container:   container,
		AccountURL:  accountURL,
		BaseURI:     baseURI,
		client:      client,
		concurrency: opts.Concurrency,
	}, nil
}

func AzureBaseURI(account, container string) string {
	account = strings.TrimSpace(account)
	container = strings.TrimSpace(container)
	return "azblob://" + account + ".blob.core.windows.net/" + container
}

func (s *AzureStore) UploadFile(ctx context.Context, ref DurableRef, path string) (DurableRef, error) {
	if err := ctx.Err(); err != nil {
		return DurableRef{}, err
	}
	ref = s.normalizeRef(ref)
	file, err := os.Open(path)
	if err != nil {
		return DurableRef{}, err
	}
	defer file.Close()
	headers := &blob.HTTPHeaders{}
	if strings.TrimSpace(ref.ContentType) != "" {
		headers.BlobContentType = &ref.ContentType
	}
	options := &azblob.UploadFileOptions{
		HTTPHeaders: headers,
		Metadata: map[string]*string{
			"tau_schema_version": &ref.SchemaVersion,
			"tau_digest":         &ref.Digest,
		},
	}
	if s.concurrency > 0 {
		options.Concurrency = s.concurrency
	}
	if _, err := s.client.UploadFile(ctx, ref.Container, ref.BlobPath, file, options); err != nil {
		return DurableRef{}, err
	}
	if err := s.Verify(ctx, ref); err != nil {
		return DurableRef{}, err
	}
	return ref, nil
}

func (s *AzureStore) Verify(ctx context.Context, ref DurableRef) error {
	ref = s.normalizeRef(ref)
	return s.downloadAndVerify(ctx, ref, io.Discard)
}

func (s *AzureStore) SignedURL(ctx context.Context, ref DurableRef, _ time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	ref = s.normalizeRef(ref)
	return ref.URI, nil
}

func (s *AzureStore) normalizeRef(ref DurableRef) DurableRef {
	ref.Account = firstNonEmpty(ref.Account, s.Account)
	ref.Container = firstNonEmpty(ref.Container, s.Container)
	if strings.TrimSpace(ref.URI) == "" && strings.TrimSpace(ref.BlobPath) != "" {
		ref.URI = strings.TrimRight(s.BaseURI, "/") + "/" + strings.TrimLeft(ref.BlobPath, "/")
	}
	return ref
}

func DownloadAndVerify(ctx context.Context, ref DurableRef, dst io.Writer) error {
	parsed, err := url.Parse(ref.URI)
	if err != nil {
		return err
	}
	switch parsed.Scheme {
	case "file":
		if err := ref.Validate(); err != nil {
			return err
		}
		return downloadFileAndVerify(ctx, ref, parsed.Path, dst)
	case "azblob", "https":
		ref, err = NormalizeAzureRef(ref)
		if err != nil {
			return err
		}
		return downloadAzureAndVerify(ctx, ref, dst)
	default:
		return fmt.Errorf("durable artifact uri scheme %q is not supported", parsed.Scheme)
	}
}

func NormalizeAzureRef(ref DurableRef) (DurableRef, error) {
	parsed, err := url.Parse(ref.URI)
	if err != nil {
		return DurableRef{}, err
	}
	host := parsed.Host
	if host == "" {
		return DurableRef{}, fmt.Errorf("azure durable ref uri host is required")
	}
	if strings.TrimSpace(ref.Account) == "" {
		ref.Account = accountFromAzureHost(host)
	}
	parts := strings.SplitN(strings.TrimPrefix(parsed.EscapedPath(), "/"), "/", 2)
	if strings.TrimSpace(ref.Container) == "" {
		if len(parts) == 0 || parts[0] == "" {
			return DurableRef{}, fmt.Errorf("azure durable ref uri container is required")
		}
		container, err := url.PathUnescape(parts[0])
		if err != nil {
			return DurableRef{}, err
		}
		ref.Container = container
	}
	if strings.TrimSpace(ref.BlobPath) == "" {
		if len(parts) < 2 || parts[1] == "" {
			return DurableRef{}, fmt.Errorf("azure durable ref uri blob path is required")
		}
		blobPath, err := url.PathUnescape(parts[1])
		if err != nil {
			return DurableRef{}, err
		}
		ref.BlobPath = blobPath
	}
	if err := ref.Validate(); err != nil {
		return DurableRef{}, err
	}
	return ref, nil
}

func downloadFileAndVerify(ctx context.Context, ref DurableRef, path string, dst io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return copyAndVerify(ref, file, dst)
}

func downloadAzureAndVerify(ctx context.Context, ref DurableRef, dst io.Writer) error {
	store, err := NewAzureStore(ctx, AzureOptions{
		Account:    ref.Account,
		Container:  ref.Container,
		AccountURL: AzureAccountURL(ref),
	})
	if err != nil {
		return err
	}
	return store.downloadAndVerify(ctx, ref, dst)
}

func AzureAccountURL(ref DurableRef) string {
	parsed, err := url.Parse(ref.URI)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return "https://" + parsed.Host
}

func accountFromAzureHost(host string) string {
	host = strings.TrimSpace(host)
	if host == "" {
		return ""
	}
	if idx := strings.IndexByte(host, '.'); idx > 0 {
		return host[:idx]
	}
	return host
}

func (s *AzureStore) downloadAndVerify(ctx context.Context, ref DurableRef, dst io.Writer) error {
	ref = s.normalizeRef(ref)
	resp, err := s.client.DownloadStream(ctx, ref.Container, ref.BlobPath, nil)
	if err != nil {
		return err
	}
	if resp.Body == nil {
		return fmt.Errorf("download blob %s: response body is empty", ref.URI)
	}
	defer resp.Body.Close()
	return copyAndVerify(ref, resp.Body, dst)
}

func copyAndVerify(ref DurableRef, src io.Reader, dst io.Writer) error {
	hash := sha256.New()
	written, err := io.Copy(io.MultiWriter(dst, hash), src)
	if err != nil {
		return err
	}
	digest := DigestWithAlgorithm(fmt.Sprintf("%x", hash.Sum(nil)))
	if digest != ref.Digest {
		return fmt.Errorf("verify blob %s: digest %s does not match %s", ref.URI, digest, ref.Digest)
	}
	if written != ref.SizeBytes {
		return fmt.Errorf("verify blob %s: size %d does not match %d", ref.URI, written, ref.SizeBytes)
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
