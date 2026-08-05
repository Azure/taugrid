package datasetingest

// Azure implementations of ByteSource, StagedSink, and VersionLocker.
//
// All three use DefaultAzureCredential (workload identity in-cluster; env
// creds for local testing). They do NOT use az CLI, SAS tokens, storage
// account keys, or client secrets.
//
// Required Azure RBAC on the destination container:
//   - Microsoft.Storage/storageAccounts/blobServices/containers/blobs/write
//     (stage + commit data blocks; create the lease sentinel blob)
//   - Microsoft.Storage/storageAccounts/blobServices/containers/blobs/read
//     (download blobs for verification)
//   - Microsoft.Storage/storageAccounts/blobServices/containers/blobs/
//     lease/action (Acquire/Renew/Release for VersionLocker)
//
// NO delete permission is required. Data blobs are written by staging
// uncommitted blocks and only committing them (CommitBlockList) once the
// full sha256 and byte count match the immutable manifest. If verification
// fails the blocks are never committed, so the destination path never
// becomes visible; Azure garbage-collects uncommitted blocks automatically
// (~7 days) with no client action. The commit is conditional
// (If-None-Match: *) so a concurrent writer can never overwrite an existing
// final blob, and an existing final blob with mismatched content is a hard
// error rather than an overwrite.

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
	// azBlockSize is the size of each staged block. Azure permits up to
	// 50,000 blocks of up to 4000 MiB each; 8 MiB keeps memory bounded while
	// supporting multi-terabyte files.
	azBlockSize = 8 * 1024 * 1024
	// azLeaseDurationSecs is the fixed lease duration. Azure accepts 15-60s
	// for a fixed lease; we renew well within it.
	azLeaseDurationSecs = 60
)

// Sentinel errors used to classify Azure responses without substring
// matching. The real implementation maps bloberror codes onto these; the
// fake implementation returns them directly.
var (
	errBlobNotFound  = errors.New("azure: blob not found")
	errBlobExists    = errors.New("azure: blob already exists")
	errLeaseConflict = errors.New("azure: lease already held")
)

// stagedBlob is the per-blob operation seam used by AzureSource, AzureSink,
// and AzureLocker. Extracting it lets tests inject a fake without a live
// Azure subscription: neither blockblob.Client nor lease.BlobClient can be
// faked directly (they are concrete SDK types), so the real implementation
// (sdkBlob) delegates to them while satisfying this interface.
type stagedBlob interface {
	// StageBlock uploads an uncommitted block identified by blockID.
	StageBlock(ctx context.Context, blockID string, data []byte) error
	// CommitBlockList commits the staged blocks in order. When noOverwrite is
	// true the commit is conditional (If-None-Match: *) and returns
	// errBlobExists if a blob already exists at the path.
	CommitBlockList(ctx context.Context, blockIDs []string, noOverwrite bool) error
	// Stat returns the committed blob size and whether it exists.
	Stat(ctx context.Context) (size int64, exists bool, err error)
	// Download opens the committed blob for reading.
	Download(ctx context.Context) (io.ReadCloser, int64, error)
	// EnsureSentinel creates a zero-byte blob if absent, tolerating an
	// existing blob (used for the lock sentinel).
	EnsureSentinel(ctx context.Context) error
	// AcquireLease acquires an exclusive lease. Returns errLeaseConflict if
	// another holder currently owns the lease.
	AcquireLease(ctx context.Context, seconds int32) error
	// RenewLease renews the lease previously acquired on this blob.
	RenewLease(ctx context.Context) error
	// ReleaseLease releases the lease previously acquired on this blob.
	ReleaseLease(ctx context.Context) error
}

// blobStore mints per-blob handles scoped to a single container.
type blobStore interface {
	blob(name string) stagedBlob
}

// -----------------------------------------------------------------------------
// Real SDK-backed blobStore
// -----------------------------------------------------------------------------

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

// blobURL builds the fully-qualified, path-escaped blob URL.
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

// newLeaseID returns a random RFC-4122 v4 UUID string, required by Azure as
// the proposed lease ID.
func newLeaseID() (string, error) {
	var u [16]byte
	if _, err := rand.Read(u[:]); err != nil {
		return "", fmt.Errorf("generate lease id: %w", err)
	}
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]), nil
}

// -----------------------------------------------------------------------------
// AzureSource
// -----------------------------------------------------------------------------

// AzureSource implements ByteSource by reading blobs from an Azure Blob
// container using DefaultAzureCredential.
type AzureSource struct {
	Account   string
	Container string
	// Prefix is prepended to each manifest path when building the blob name.
	Prefix string

	store blobStore
}

// NewAzureSource constructs an AzureSource backed by DefaultAzureCredential.
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

// Open downloads the blob at Prefix/relPath for reading.
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

// -----------------------------------------------------------------------------
// AzureSink
// -----------------------------------------------------------------------------

// AzureSink implements StagedSink by staging blocks and committing them only
// after the full sha256 and byte count match the immutable manifest. A blob
// never becomes visible at its final path with unverified content, and an
// existing final blob is never overwritten (mismatch is a hard error).
type AzureSink struct {
	Account   string
	Container string
	Prefix    string

	store blobStore
}

// NewAzureSink constructs an AzureSink backed by DefaultAzureCredential.
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

// Write stages src into uncommitted blocks, verifies size + sha256, then
// commits with a no-overwrite condition. Fail-closed: on any mismatch the
// blocks are never committed and the destination path stays empty.
func (s *AzureSink) Write(ctx context.Context, destPath string, src io.Reader, expectedBytes int64, expectedSHA256 string) (WriteResult, error) {
	blobName := joinPrefix(s.Prefix, destPath)
	b := s.store.blob(blobName)

	// Fast path: if the final blob already exists, verify it matches (skip)
	// or fail before transferring any bytes.
	if res, existed, err := s.verifyExisting(ctx, b, blobName, expectedBytes, expectedSHA256); err != nil {
		return WriteResult{}, err
	} else if existed {
		return res, nil
	}

	// Stage blocks while hashing and counting.
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

	// FAIL-CLOSED: verify BEFORE commit. Staged blocks are not visible until
	// committed, so a mismatch here leaves the destination path empty.
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

	// Commit conditionally (no overwrite).
	err := b.CommitBlockList(ctx, blockIDs, true)
	if err == nil {
		return WriteResult{Bytes: total, SHA256: got}, nil
	}
	if !errors.Is(err, errBlobExists) {
		return WriteResult{}, fmt.Errorf("commit %s: %w", destPath, err)
	}

	// Lost a race: another writer committed the blob first. Verify it matches.
	res, existed, verr := s.verifyExisting(ctx, b, blobName, expectedBytes, expectedSHA256)
	if verr != nil {
		return WriteResult{}, verr
	}
	if !existed {
		return WriteResult{}, fmt.Errorf("commit of %s reported conflict but blob is absent — refusing", blobName)
	}
	return res, nil
}

// verifyExisting checks whether the final blob already exists. Returns
// (result, true, nil) when it exists and matches expected size+sha256 (Skipped),
// (zero, false, nil) when it does not exist, and (zero, false, err) when it
// exists with mismatched content.
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

// blockID encodes a block index as a fixed-width base64 ID (Azure requires all
// block IDs in a commit to share the same length).
func blockID(idx int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("block-%012d", idx)))
}

// -----------------------------------------------------------------------------
// AzureLocker
// -----------------------------------------------------------------------------

// AzureLocker implements VersionLocker using an Azure Blob lease on a
// per-version sentinel blob at <LockPrefix>/<name>@<version>.lock. A lease is
// acquired, renewed by a background goroutine, and released on unlock. No
// delete permission is required; the sentinel blob persists and its lease
// auto-expires if the process dies.
type AzureLocker struct {
	Account    string
	Container  string
	LockPrefix string

	store blobStore

	// Tunables (defaulted in the constructor; overridable in tests).
	leaseSeconds  int32
	renewInterval time.Duration
	retryInterval time.Duration
	maxWait       time.Duration
}

// NewAzureLocker constructs an AzureLocker backed by DefaultAzureCredential.
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

// Lock acquires the per-version blob lease, blocking (with bounded retry) while
// another holder owns it, and returns a context that is canceled if lease
// renewal fails plus an idempotent unlock function.
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

// -----------------------------------------------------------------------------
// Helpers
// -----------------------------------------------------------------------------

// joinPrefix joins a prefix and a relative path with a single slash, trimming a
// trailing slash on the prefix.
func joinPrefix(prefix, rel string) string {
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return rel
	}
	return prefix + "/" + rel
}

// NewAzureDefaultCred creates DefaultAzureCredential for workload identity.
// It rejects SAS tokens, storage keys, and anonymous access implicitly by
// only supporting the DefaultAzureCredential chain.
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

// ValidateAzureURL validates a source or destination URI. It accepts az://,
// file://, and unauthenticated https:// source roots. Callers must still
// constrain destinations to file:// or az://. It rejects query credentials,
// embedded userinfo, plaintext HTTP, unsupported schemes, and path traversal.
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

	// Reject any credential material embedded in the URL.
	if parsed.RawQuery != "" || parsed.ForceQuery {
		return fmt.Errorf("query strings are not accepted in %q — SAS tokens and query credentials are rejected; use workload identity", u)
	}
	if parsed.User != nil {
		return fmt.Errorf("userinfo is not accepted in %q — credentials must not be embedded in URIs", u)
	}
	if parsed.Fragment != "" {
		return fmt.Errorf("URI fragments are not accepted in %q", u)
	}

	// Reject path traversal in the path or host.
	if hasTraversal(parsed.Path) {
		return fmt.Errorf("path traversal (..) not accepted in %q", u)
	}

	// Defense-in-depth against key material spelled out in the raw string.
	lower := strings.ToLower(u)
	if strings.Contains(lower, "sharedkeycredential") || strings.Contains(lower, "accountkey=") {
		return fmt.Errorf("storage account keys are not accepted in %q — use workload identity", u)
	}
	return nil
}

// hasTraversal reports whether p contains a ".." path segment.
func hasTraversal(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}
