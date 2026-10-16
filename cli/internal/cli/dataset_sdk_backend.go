// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/storage"
)

type sdkBlobOps interface {
	read(ctx context.Context, blobName string) ([]byte, error)
	write(ctx context.Context, blobName string, data []byte, overwrite bool) error
	listChildren(ctx context.Context, prefix string) ([]string, error)
	remove(ctx context.Context, blobName string) error
}

type sdkAzBackend struct {
	ops        sdkBlobOps
	rootPrefix string
}

func newSDKAzBackend(account, container string) (*sdkAzBackend, error) {
	ops, err := newAzSDKBlobOps(account, container, nil)
	if err != nil {
		return nil, err
	}
	return &sdkAzBackend{ops: ops, rootPrefix: storage.DatasetRegistryDir}, nil
}

func (b *sdkAzBackend) blobName(p string) string {
	rel := strings.TrimPrefix(p, b.rootPrefix)
	return strings.TrimPrefix(rel, "/")
}

func (b *sdkAzBackend) ReadFile(ctx context.Context, p string) ([]byte, error) {
	return b.ops.read(ctx, b.blobName(p))
}

func (b *sdkAzBackend) WriteFile(ctx context.Context, p string, data []byte, overwrite bool) error {
	return b.ops.write(ctx, b.blobName(p), data, overwrite)
}

func (b *sdkAzBackend) List(ctx context.Context, dir string) ([]string, error) {
	prefix := b.blobName(dir)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	return b.ops.listChildren(ctx, prefix)
}

func (b *sdkAzBackend) Delete(ctx context.Context, p string) error {
	return b.ops.remove(ctx, b.blobName(p))
}

type azSDKBlobOps struct {
	container *container.Client
}

func newAzSDKBlobOps(account, containerName string, cred azcore.TokenCredential) (*azSDKBlobOps, error) {
	if strings.TrimSpace(account) == "" {
		return nil, fmt.Errorf("storage account must not be empty")
	}
	if strings.TrimSpace(containerName) == "" {
		return nil, fmt.Errorf("container must not be empty")
	}
	if cred == nil {
		var err error
		cred, err = azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("DefaultAzureCredential: %w", err)
		}
	}
	containerURL := fmt.Sprintf("https://%s.blob.core.windows.net/%s",
		url.PathEscape(account), url.PathEscape(containerName))
	c, err := container.NewClient(containerURL, cred, nil)
	if err != nil {
		return nil, fmt.Errorf("container client: %w", err)
	}
	return &azSDKBlobOps{container: c}, nil
}

func (o *azSDKBlobOps) read(ctx context.Context, blobName string) ([]byte, error) {
	bb := o.container.NewBlockBlobClient(blobName)
	resp, err := bb.DownloadStream(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, dataset.ErrNotExist
		}
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(resp.Body); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (o *azSDKBlobOps) write(ctx context.Context, blobName string, data []byte, overwrite bool) error {
	bb := o.container.NewBlockBlobClient(blobName)
	opts := &blockblob.UploadOptions{}
	if !overwrite {
		opts.AccessConditions = &blob.AccessConditions{
			ModifiedAccessConditions: &blob.ModifiedAccessConditions{
				IfNoneMatch: to.Ptr(azcore.ETagAny),
			},
		}
	}
	_, err := bb.Upload(ctx, streaming.NopCloser(bytes.NewReader(data)), opts)
	if err != nil {
		if !overwrite && bloberror.HasCode(err, bloberror.BlobAlreadyExists, bloberror.ConditionNotMet) {
			return dataset.ErrExist
		}
		return err
	}
	return nil
}

func (o *azSDKBlobOps) listChildren(ctx context.Context, prefix string) ([]string, error) {
	pager := o.container.NewListBlobsHierarchyPager("/", &container.ListBlobsHierarchyOptions{
		Prefix: to.Ptr(prefix),
	})
	seen := map[string]bool{}
	var children []string
	add := func(full string) {
		rest := strings.TrimPrefix(full, prefix)
		rest = strings.TrimSuffix(rest, "/")
		if rest == "" {
			return
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i]
		}
		if !seen[rest] {
			seen[rest] = true
			children = append(children, rest)
		}
	}
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		if page.Segment == nil {
			continue
		}
		for _, p := range page.Segment.BlobPrefixes {
			if p != nil && p.Name != nil {
				add(*p.Name)
			}
		}
		for _, bi := range page.Segment.BlobItems {
			if bi != nil && bi.Name != nil {
				add(*bi.Name)
			}
		}
	}
	return children, nil
}

func (o *azSDKBlobOps) remove(ctx context.Context, blobName string) error {
	bb := o.container.NewBlockBlobClient(blobName)
	_, err := bb.Delete(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil
		}
		return err
	}
	return nil
}

var _ dataset.Backend = (*sdkAzBackend)(nil)
var _ sdkBlobOps = (*azSDKBlobOps)(nil)
