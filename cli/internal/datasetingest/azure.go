// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package datasetingest

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/to"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/lease"
)

const (
	azBlockSize         = 8 * 1024 * 1024
	azLeaseDurationSecs = 60
)

var (
	errBlobNotFound  = errors.New("azure: blob not found")
	errBlobExists    = errors.New("azure: blob already exists")
	errLeaseConflict = errors.New("azure: lease already held")
)

type stagedBlob interface {
	StageBlock(ctx context.Context, blockID string, data []byte) error
	CommitBlockList(ctx context.Context, blockIDs []string, noOverwrite bool) error
	Stat(ctx context.Context) (size int64, exists bool, err error)
	Download(ctx context.Context) (io.ReadCloser, int64, error)
	EnsureSentinel(ctx context.Context) error
	AcquireLease(ctx context.Context, seconds int32) error
	RenewLease(ctx context.Context) error
	ReleaseLease(ctx context.Context) error
}

type blobStore interface {
	blob(name string) stagedBlob
}

type sdkBlobStore struct {
	accountURL string
	container  string
	cred       azcore.TokenCredential
}

func newSDKBlobStore(account, container string, cred azcore.TokenCredential) (*sdkBlobStore, error) {
	if cred == nil {
		var err error
		cred, err = azidentity.NewDefaultAzureCredential(nil)
		if err != nil {
			return nil, fmt.Errorf("DefaultAzureCredential: %w", err)
		}
	}
	return &sdkBlobStore{
		accountURL: "https://" + account + ".blob.core.windows.net",
		container:  container,
		cred:       cred,
	}, nil
}

func (s *sdkBlobStore) blob(name string) stagedBlob {
	return &sdkBlob{store: s, name: name}
}

func (s *sdkBlobStore) blobURL(name string) string {
	segments := strings.Split(name, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return s.accountURL + "/" + url.PathEscape(s.container) + "/" + strings.Join(segments, "/")
}

type sdkBlob struct {
	store *sdkBlobStore
	name  string

	bb *blockblob.Client
	lc *lease.BlobClient
}

func (b *sdkBlob) client() (*blockblob.Client, error) {
	if b.bb != nil {
		return b.bb, nil
	}
	c, err := blockblob.NewClient(b.store.blobURL(b.name), b.store.cred, nil)
	if err != nil {
		return nil, fmt.Errorf("new blockblob client: %w", err)
	}
	b.bb = c
	return c, nil
}

func (b *sdkBlob) StageBlock(ctx context.Context, blockID string, data []byte) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	_, err = c.StageBlock(ctx, blockID, streaming.NopCloser(strings.NewReader(string(data))), nil)
	return err
}

func noOverwriteConditions() *blob.AccessConditions {
	return &blob.AccessConditions{
		ModifiedAccessConditions: &blob.ModifiedAccessConditions{
			IfNoneMatch: to.Ptr(azcore.ETagAny),
		},
	}
}

func (b *sdkBlob) CommitBlockList(ctx context.Context, blockIDs []string, noOverwrite bool) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	var opts *blockblob.CommitBlockListOptions
	if noOverwrite {
		opts = &blockblob.CommitBlockListOptions{AccessConditions: noOverwriteConditions()}
	}
	_, err = c.CommitBlockList(ctx, blockIDs, opts)
	if err != nil {
		if bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists) {
			return errBlobExists
		}
		return err
	}
	return nil
}

func (b *sdkBlob) Stat(ctx context.Context) (int64, bool, error) {
	c, err := b.client()
	if err != nil {
		return 0, false, err
	}
	resp, err := c.GetProperties(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return 0, false, nil
		}
		return 0, false, err
	}
	var size int64
	if resp.ContentLength != nil {
		size = *resp.ContentLength
	}
	return size, true, nil
}

func (b *sdkBlob) Download(ctx context.Context) (io.ReadCloser, int64, error) {
	c, err := b.client()
	if err != nil {
		return nil, 0, err
	}
	resp, err := c.BlobClient().DownloadStream(ctx, nil)
	if err != nil {
		if bloberror.HasCode(err, bloberror.BlobNotFound) {
			return nil, 0, errBlobNotFound
		}
		return nil, 0, err
	}
	var size int64
	if resp.ContentLength != nil {
		size = *resp.ContentLength
	}
	return resp.Body, size, nil
}

func (b *sdkBlob) EnsureSentinel(ctx context.Context) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	_, err = c.Upload(ctx, streaming.NopCloser(strings.NewReader("")), &blockblob.UploadOptions{
		AccessConditions: noOverwriteConditions(),
	})
	if err != nil && !bloberror.HasCode(err, bloberror.ConditionNotMet, bloberror.BlobAlreadyExists) {
		return err
	}
	return nil
}

func (b *sdkBlob) AcquireLease(ctx context.Context, seconds int32) error {
	c, err := b.client()
	if err != nil {
		return err
	}
	leaseID, err := newLeaseID()
	if err != nil {
		return err
	}
	lc, err := lease.NewBlobClient(c, &lease.BlobClientOptions{LeaseID: &leaseID})
	if err != nil {
		return fmt.Errorf("new lease client: %w", err)
	}
	if _, err := lc.AcquireLease(ctx, seconds, nil); err != nil {
		if bloberror.HasCode(err, bloberror.LeaseAlreadyPresent, bloberror.LeaseIDMissing) {
			return errLeaseConflict
		}
		return err
	}
	b.lc = lc
	return nil
}

func (b *sdkBlob) RenewLease(ctx context.Context) error {
	if b.lc == nil {
		return errors.New("renew: no lease held")
	}
	_, err := b.lc.RenewLease(ctx, nil)
	return err
}

func (b *sdkBlob) ReleaseLease(ctx context.Context) error {
	if b.lc == nil {
		return nil
	}
	_, err := b.lc.ReleaseLease(ctx, nil)
	return err
}

func newLeaseID() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", fmt.Errorf("generate lease id: %w", err)
	}
	u[6] = (u[6] & 0x0f) | 0x40
	u[8] = (u[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}

type AzureSource struct {
	Account   string
	Container string
	Prefix    string

	store blobStore
}

func NewAzureSource(ctx context.Context, account, container, prefix string, cred azcore.TokenCredential) (*AzureSource, error) {
	store, err := newSDKBlobStore(account, container, cred)
	if err != nil {
		return nil, err
	}
	return &AzureSource{Account: account, Container: container, Prefix: prefix, store: store}, nil
}

func (s *AzureSource) Describe() string {
	return fmt.Sprintf("az://%s/%s/%s", s.Account, s.Container, s.Prefix)
}

func (s *AzureSource) Open(ctx context.Context, relPath string) (io.ReadCloser, int64, error) {
	blobName := joinPrefix(s.Prefix, relPath)
	rc, size, err := s.store.blob(blobName).Download(ctx)
	if err != nil {
		if errors.Is(err, errBlobNotFound) {
			return nil, 0, fmt.Errorf("source blob not found: %s", blobName)
		}
		return nil, 0, fmt.Errorf("download %s: %w", blobName, err)
	}
	return rc, size, nil
}

type AzureSink struct {
	Account   string
	Container string
	Prefix    string

	store blobStore
}

func NewAzureSink(ctx context.Context, account, container, prefix string, cred azcore.TokenCredential) (*AzureSink, error) {
	store, err := newSDKBlobStore(account, container, cred)
	if err != nil {
		return nil, err
	}
	return &AzureSink{Account: account, Container: container, Prefix: prefix, store: store}, nil
}

func (s *AzureSink) Describe() string {
	return fmt.Sprintf("az://%s/%s/%s", s.Account, s.Container, s.Prefix)
}

func (s *AzureSink) Write(ctx context.Context, destPath string, src io.Reader, expectedBytes int64, expectedSHA256 string) (WriteResult, error) {
	blobName := joinPrefix(s.Prefix, destPath)
	b := s.store.blob(blobName)

	if res, existed, err := s.verifyExisting(ctx, b, blobName, expectedBytes, expectedSHA256); err != nil {
		return WriteResult{}, err
	} else if existed {
		return res, nil
	}

	h := sha256.New()
	var total int64
	var blockIDs []string
	buf := make([]byte, azBlockSize)
	idx := 0
	for {
		n, rerr := io.ReadFull(src, buf)
		if n > 0 {
			chunk := buf[:n]
			id := blockID(idx)
			if err := b.StageBlock(ctx, id, chunk); err != nil {
				return WriteResult{}, fmt.Errorf("stage block %d for %s: %w", idx, destPath, err)
			}
			_, _ = h.Write(chunk)
			total += int64(n)
			blockIDs = append(blockIDs, id)
			idx++
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return WriteResult{}, fmt.Errorf("read source for %s: %w", destPath, rerr)
		}
	}

	got := hex.EncodeToString(h.Sum(nil))

	if total != expectedBytes {
		return WriteResult{}, fmt.Errorf(
			"byte count mismatch for %s: expected=%d got=%d (staged blocks not committed; destination unchanged)",
			destPath, expectedBytes, total,
		)
	}
	if got != expectedSHA256 {
		return WriteResult{}, fmt.Errorf(
			"sha256 mismatch for %s: expected=%s got=%s (staged blocks not committed; destination unchanged)",
			destPath, expectedSHA256, got,
		)
	}

	err := b.CommitBlockList(ctx, blockIDs, true)
	if err == nil {
		return WriteResult{Bytes: total, SHA256: got}, nil
	}
	if !errors.Is(err, errBlobExists) {
		return WriteResult{}, fmt.Errorf("commit %s: %w", destPath, err)
	}

	res, existed, verr := s.verifyExisting(ctx, b, blobName, expectedBytes, expectedSHA256)
	if verr != nil {
		return WriteResult{}, verr
	}
	if !existed {
		return WriteResult{}, fmt.Errorf("commit of %s reported conflict but blob is absent — refusing", blobName)
	}
	return res, nil
}

func (s *AzureSink) verifyExisting(ctx context.Context, b stagedBlob, blobName string, expectedBytes int64, expectedSHA256 string) (WriteResult, bool, error) {
	size, exists, err := b.Stat(ctx)
	if err != nil {
		return WriteResult{}, false, fmt.Errorf("stat %s: %w", blobName, err)
	}
	if !exists {
		return WriteResult{}, false, nil
	}
	if size != expectedBytes {
		return WriteResult{}, false, fmt.Errorf(
			"destination blob %s exists with size=%d but expected=%d — refusing overwrite of mismatched content",
			blobName, size, expectedBytes,
		)
	}
	rc, _, err := b.Download(ctx)
	if err != nil {
		if errors.Is(err, errBlobNotFound) {
			return WriteResult{}, false, nil
		}
		return WriteResult{}, false, fmt.Errorf("download existing %s: %w", blobName, err)
	}
	defer rc.Close()
	h := sha256.New()
	if _, err := io.Copy(h, rc); err != nil {
		return WriteResult{}, false, fmt.Errorf("hash existing %s: %w", blobName, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != expectedSHA256 {
		return WriteResult{}, false, fmt.Errorf(
			"destination blob %s exists with sha256=%s but expected=%s — refusing overwrite of mismatched content",
			blobName, got, expectedSHA256,
		)
	}
	return WriteResult{Skipped: true, SHA256: got}, true, nil
}

func blockID(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("block-%012d", idx)))
}

type AzureLocker struct {
	Account    string
	Container  string
	LockPrefix string

	store blobStore

	leaseSeconds  int32
	renewInterval time.Duration
	retryInterval time.Duration
	maxWait       time.Duration
}

func NewAzureLocker(ctx context.Context, account, container, lockPrefix string, cred azcore.TokenCredential) (*AzureLocker, error) {
	store, err := newSDKBlobStore(account, container, cred)
	if err != nil {
		return nil, err
	}
	return &AzureLocker{
		Account:       account,
		Container:     container,
		LockPrefix:    lockPrefix,
		store:         store,
		leaseSeconds:  azLeaseDurationSecs,
		renewInterval: (azLeaseDurationSecs / 2) * time.Second,
		retryInterval: 2 * time.Second,
		maxWait:       5 * time.Minute,
	}, nil
}

func (l *AzureLocker) Lock(ctx context.Context, name, version string) (context.Context, func() error, error) {
	b := l.store.blob(l.lockBlobName(name, version))

	if err := b.EnsureSentinel(ctx); err != nil {
		return nil, nil, fmt.Errorf("create lock sentinel for %s@%s: %w", name, version, err)
	}

	deadline := time.Now().Add(l.maxWait)
	for {
		err := b.AcquireLease(ctx, l.leaseSeconds)
		if err == nil {
			break
		}
		if !errors.Is(err, errLeaseConflict) {
			return nil, nil, fmt.Errorf("acquire lease for %s@%s: %w", name, version, err)
		}
		if time.Now().After(deadline) {
			return nil, nil, fmt.Errorf("timed out after %s waiting for ingest lock on %s@%s", l.maxWait, name, version)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(l.retryInterval):
		}
	}

	lockCtx, cancelLock := context.WithCancelCause(ctx)
	renewCtx, cancelRenew := context.WithCancel(context.Background())
	var renewWG sync.WaitGroup
	var renewErr error
	var renewMu sync.Mutex
	setRenewalFailure := func(err error) {
		renewMu.Lock()
		if renewErr == nil {
			renewErr = fmt.Errorf("renew lease for %s@%s: %w", name, version, err)
			cancelLock(renewErr)
			cancelRenew()
		}
		renewMu.Unlock()
	}
	renewWG.Add(1)
	go func() {
		defer renewWG.Done()
		ticker := time.NewTicker(l.renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if err := b.RenewLease(renewCtx); err != nil {
					setRenewalFailure(err)
					return
				}
			}
		}
	}()

	var once sync.Once
	var unlockErr error
	unlock := func() error {
		once.Do(func() {
			cancelRenew()
			renewWG.Wait()
			releaseCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			relErr := b.ReleaseLease(releaseCtx)
			renewMu.Lock()
			unlockErr = renewErr
			renewMu.Unlock()
			if relErr != nil {
				unlockErr = errors.Join(unlockErr, fmt.Errorf("release lease for %s@%s: %w", name, version, relErr))
			}
			cancelLock(nil)
		})
		return unlockErr
	}
	return lockCtx, unlock, nil
}

func (l *AzureLocker) lockBlobName(name, version string) string {
	return joinPrefix(l.LockPrefix, name+"@"+version+".lock")
}

func joinPrefix(prefix, rel string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}

func NewAzureDefaultCred() (azcore.TokenCredential, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf(
			"DefaultAzureCredential not available — ensure pod has workload identity "+
				"(azure.workload.identity/use: 'true') and AZURE_CLIENT_ID is set: %w", err,
		)
	}
	return cred, nil
}

func ValidateAzureURL(u string) error {
	if strings.TrimSpace(u) == "" {
		return fmt.Errorf("empty URI")
	}
	if u != strings.TrimSpace(u) {
		return fmt.Errorf("URI %q has leading/trailing whitespace", u)
	}

	parsed, err := url.Parse(u)
	if err != nil {
		return fmt.Errorf("invalid URI %q: %w", u, err)
	}

	scheme := strings.ToLower(parsed.Scheme)
	switch scheme {
	case "az", "file":
	case "http":
		return fmt.Errorf("plaintext HTTP not accepted in %q — use az:// with workload identity", u)
	case "https":
		if parsed.Host == "" {
			return fmt.Errorf("https URI %q must include a host", u)
		}
	case "":
		return fmt.Errorf("URI %q must have an explicit scheme (az://, file://, or https://)", u)
	default:
		return fmt.Errorf("unsupported scheme %q in %q — only az://, file://, and https:// are accepted", parsed.Scheme, u)
	}

	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("query strings are not accepted in %q — SAS tokens and query credentials are rejected; use workload identity", u)
	}
	if parsed.User != nil {
		return fmt.Errorf("userinfo is not accepted in %q — credentials must not be embedded in URIs", u)
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("URI fragments are not accepted in %q", u)
	}

	if hasTraversal(parsed.Path) {
		return fmt.Errorf("path traversal (..) not accepted in %q", u)
	}

	lower := strings.ToLower(u)
	if strings.Contains(lower, "sharedkeycredential") || strings.Contains(lower, "accountkey=") {
		return fmt.Errorf("storage account keys are not accepted in %q — use workload identity", u)
	}
	return nil
}

func hasTraversal(p string) bool {
	return slices.Contains(strings.Split(p, "/"), "..")
}
