// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package datasetingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBlobStore is an in-memory blobStore for testing the Azure sink and
// locker without a live Azure subscription. It shares state across blob()
// calls keyed by blob name.
type fakeBlobStore struct {
	mu    sync.Mutex
	blobs map[string]*fakeBlobState
}

type fakeBlobState struct {
	committed []byte
	exists    bool
	staged    map[string][]byte
	leaseHeld bool

	renewCount   int
	releaseCount int
	renewErr     error

	// beforeCommit runs inside CommitBlockList before the overwrite check,
	// letting a test simulate a concurrent writer winning the race.
	beforeCommit func()
}

func newFakeBlobStore() *fakeBlobStore {
	return &fakeBlobStore{blobs: map[string]*fakeBlobState{}}
}

func (s *fakeBlobStore) state(name string) *fakeBlobState {
	st := s.blobs[name]
	if st == nil {
		st = &fakeBlobState{staged: map[string][]byte{}}
		s.blobs[name] = st
	}
	return st
}

func (s *fakeBlobStore) seedCommitted(name string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.state(name)
	st.committed = append([]byte(nil), data...)
	st.exists = true
}

func (s *fakeBlobStore) blob(name string) stagedBlob {
	return &fakeBlob{store: s, name: name}
}

type fakeBlob struct {
	store *fakeBlobStore
	name  string
	holds bool
}

func (b *fakeBlob) StageBlock(ctx context.Context, blockID string, data []byte) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	st := b.store.state(b.name)
	st.staged[blockID] = append([]byte(nil), data...)
	return nil
}

func (b *fakeBlob) CommitBlockList(ctx context.Context, blockIDs []string, noOverwrite bool) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	st := b.store.state(b.name)
	if st.beforeCommit != nil {
		fn := st.beforeCommit
		st.beforeCommit = nil
		b.store.mu.Unlock()
		fn()
		b.store.mu.Lock()
	}
	if noOverwrite && st.exists {
		return errBlobExists
	}
	var out []byte
	for _, id := range blockIDs {
		out = append(out, st.staged[id]...)
	}
	st.committed = out
	st.exists = true
	st.staged = map[string][]byte{}
	return nil
}

func (b *fakeBlob) Stat(ctx context.Context) (int64, bool, error) {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	st := b.store.state(b.name)
	if !st.exists {
		return 0, false, nil
	}
	return int64(len(st.committed)), true, nil
}

func (b *fakeBlob) Download(ctx context.Context) (io.ReadCloser, int64, error) {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	st := b.store.state(b.name)
	if !st.exists {
		return nil, 0, errBlobNotFound
	}
	return io.NopCloser(bytes.NewReader(st.committed)), int64(len(st.committed)), nil
}

func (b *fakeBlob) EnsureSentinel(ctx context.Context) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	st := b.store.state(b.name)
	if !st.exists {
		st.committed = []byte{}
		st.exists = true
	}
	return nil
}

func (b *fakeBlob) AcquireLease(ctx context.Context, seconds int32) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	st := b.store.state(b.name)
	if st.leaseHeld {
		return errLeaseConflict
	}
	st.leaseHeld = true
	b.holds = true
	return nil
}

func (b *fakeBlob) RenewLease(ctx context.Context) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	st := b.store.state(b.name)
	st.renewCount++
	return st.renewErr
}

func (b *fakeBlob) ReleaseLease(ctx context.Context) error {
	b.store.mu.Lock()
	defer b.store.mu.Unlock()
	st := b.store.state(b.name)
	if b.holds {
		st.leaseHeld = false
		st.releaseCount++
		b.holds = false
	}
	return nil
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func newTestSink(store blobStore) *AzureSink {
	return &AzureSink{Account: "acct", Container: "ctr", Prefix: "pre", store: store}
}

func TestAzureSink_CommitsWhenVerified(t *testing.T) {
	fs := newFakeBlobStore()
	sink := newTestSink(fs)
	data := []byte("hello dataset bytes")
	res, err := sink.Write(context.Background(), "a/b.txt", bytes.NewReader(data), int64(len(data)), sha256Hex(data))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if res.Skipped || res.Bytes != int64(len(data)) || res.SHA256 != sha256Hex(data) {
		t.Fatalf("unexpected result: %+v", res)
	}
	st := fs.blobs["pre/a/b.txt"]
	if st == nil || !st.exists || !bytes.Equal(st.committed, data) {
		t.Fatalf("blob not committed correctly: %+v", st)
	}
}

func TestAzureSink_HashMismatchLeavesUncommitted(t *testing.T) {
	fs := newFakeBlobStore()
	sink := newTestSink(fs)
	data := []byte("real content")
	// Expected sha for different content → mismatch.
	wrongSHA := sha256Hex([]byte("other content"))
	_, err := sink.Write(context.Background(), "x.bin", bytes.NewReader(data), int64(len(data)), wrongSHA)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("expected sha256 mismatch error, got %v", err)
	}
	st := fs.blobs["pre/x.bin"]
	if st != nil && st.exists {
		t.Fatalf("blob must NOT be committed on hash mismatch; got exists=%v", st.exists)
	}
}

func TestAzureSink_ByteCountMismatchLeavesUncommitted(t *testing.T) {
	fs := newFakeBlobStore()
	sink := newTestSink(fs)
	data := []byte("twelve bytes")
	// Correct sha but wrong expected byte count.
	_, err := sink.Write(context.Background(), "x.bin", bytes.NewReader(data), int64(len(data))+5, sha256Hex(data))
	if err == nil || !strings.Contains(err.Error(), "byte count mismatch") {
		t.Fatalf("expected byte count mismatch error, got %v", err)
	}
	if st := fs.blobs["pre/x.bin"]; st != nil && st.exists {
		t.Fatalf("blob must NOT be committed on byte mismatch")
	}
}

func TestAzureSink_ExistingMatchSkips(t *testing.T) {
	fs := newFakeBlobStore()
	sink := newTestSink(fs)
	data := []byte("already there")
	fs.seedCommitted("pre/dup.txt", data)
	res, err := sink.Write(context.Background(), "dup.txt", bytes.NewReader(data), int64(len(data)), sha256Hex(data))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if !res.Skipped {
		t.Fatalf("expected Skipped=true for matching existing blob, got %+v", res)
	}
	// No staged blocks should have been produced.
	if st := fs.blobs["pre/dup.txt"]; len(st.staged) != 0 {
		t.Fatalf("expected no staged blocks on skip, got %d", len(st.staged))
	}
}

func TestAzureSink_ExistingMismatchFails(t *testing.T) {
	fs := newFakeBlobStore()
	sink := newTestSink(fs)
	// Existing blob has same length but different content.
	fs.seedCommitted("pre/c.txt", []byte("AAAAAAAA"))
	newData := []byte("BBBBBBBB")
	_, err := sink.Write(context.Background(), "c.txt", bytes.NewReader(newData), int64(len(newData)), sha256Hex(newData))
	if err == nil || !strings.Contains(err.Error(), "refusing overwrite") {
		t.Fatalf("expected refusing-overwrite error, got %v", err)
	}
}

func TestAzureSink_RaceConflictVerifiesExisting(t *testing.T) {
	fs := newFakeBlobStore()
	sink := newTestSink(fs)
	data := []byte("raced content")
	// Simulate: Stat initially says not-exist, but just before our commit a
	// concurrent writer commits identical content, causing errBlobExists.
	st := fs.state("pre/race.txt")
	st.beforeCommit = func() {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		s := fs.state("pre/race.txt")
		s.committed = append([]byte(nil), data...)
		s.exists = true
	}
	res, err := sink.Write(context.Background(), "race.txt", bytes.NewReader(data), int64(len(data)), sha256Hex(data))
	if err != nil {
		t.Fatalf("Write should verify existing on race, got %v", err)
	}
	if !res.Skipped {
		t.Fatalf("expected Skipped after race verify, got %+v", res)
	}
}

func TestAzureSink_EmptyFileCommits(t *testing.T) {
	fs := newFakeBlobStore()
	sink := newTestSink(fs)
	res, err := sink.Write(context.Background(), "empty", bytes.NewReader(nil), 0, sha256Hex(nil))
	if err != nil {
		t.Fatalf("Write empty: %v", err)
	}
	if res.Skipped || res.Bytes != 0 {
		t.Fatalf("unexpected result for empty file: %+v", res)
	}
	if st := fs.blobs["pre/empty"]; st == nil || !st.exists {
		t.Fatalf("empty blob must be committed")
	}
}

func TestAzureSource_Open(t *testing.T) {
	fs := newFakeBlobStore()
	fs.seedCommitted("p/x/y.txt", []byte("payload"))
	src := &AzureSource{Account: "a", Container: "c", Prefix: "p", store: fs}
	rc, size, err := src.Open(context.Background(), "x/y.txt")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	if size != 7 {
		t.Fatalf("size = %d, want 7", size)
	}
	got, _ := io.ReadAll(rc)
	if string(got) != "payload" {
		t.Fatalf("content = %q", got)
	}
}

func TestAzureSource_OpenNotFound(t *testing.T) {
	fs := newFakeBlobStore()
	src := &AzureSource{Account: "a", Container: "c", Prefix: "p", store: fs}
	_, _, err := src.Open(context.Background(), "missing")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected not found, got %v", err)
	}
}

func newTestLocker(store blobStore) *AzureLocker {
	return &AzureLocker{
		Account:       "acct",
		Container:     "ctr",
		LockPrefix:    ".locks",
		store:         store,
		leaseSeconds:  60,
		renewInterval: 5 * time.Millisecond,
		retryInterval: 2 * time.Millisecond,
		maxWait:       60 * time.Millisecond,
	}
}

func TestAzureLocker_AcquireReleaseReacquire(t *testing.T) {
	fs := newFakeBlobStore()
	l := newTestLocker(fs)
	_, unlock, err := l.Lock(context.Background(), "ds", "v1")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	// Idempotent unlock.
	if err := unlock(); err != nil {
		t.Fatalf("second unlock: %v", err)
	}
	// Reacquire after release.
	_, unlock2, err := l.Lock(context.Background(), "ds", "v1")
	if err != nil {
		t.Fatalf("reacquire Lock: %v", err)
	}
	_ = unlock2()

	st := fs.blobs[".locks/ds@v1.lock"]
	if st.releaseCount < 1 {
		t.Fatalf("expected at least one release, got %d", st.releaseCount)
	}
}

func TestAzureLocker_ContentionTimesOutThenSucceeds(t *testing.T) {
	fs := newFakeBlobStore()
	a := newTestLocker(fs)
	b := newTestLocker(fs)

	_, unlockA, err := a.Lock(context.Background(), "ds", "v1")
	if err != nil {
		t.Fatalf("A Lock: %v", err)
	}

	// B cannot acquire while A holds it → bounded wait times out.
	_, _, err = b.Lock(context.Background(), "ds", "v1")
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected contention timeout, got %v", err)
	}

	// Release A; B can now acquire.
	if err := unlockA(); err != nil {
		t.Fatalf("unlock A: %v", err)
	}
	_, unlockB, err := b.Lock(context.Background(), "ds", "v1")
	if err != nil {
		t.Fatalf("B Lock after release: %v", err)
	}
	_ = unlockB()
}

func TestAzureLocker_RenewsWhileHeld(t *testing.T) {
	fs := newFakeBlobStore()
	l := newTestLocker(fs)
	_, unlock, err := l.Lock(context.Background(), "ds", "v1")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}

	time.Sleep(40 * time.Millisecond)
	if err := unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	st := fs.blobs[".locks/ds@v1.lock"]
	if st.renewCount == 0 {
		t.Fatalf("expected lease to be renewed at least once")
	}
	if st.releaseCount != 1 {
		t.Fatalf("expected exactly one release, got %d", st.releaseCount)
	}
}

func TestAzureLocker_RenewFailureCancelsScopedContextAndSurfaces(t *testing.T) {
	fs := newFakeBlobStore()
	l := newTestLocker(fs)
	fs.state(".locks/ds@v1.lock").renewErr = errors.New("renew denied")

	lockCtx, unlock, err := l.Lock(context.Background(), "ds", "v1")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	select {
	case <-lockCtx.Done():
	case <-time.After(250 * time.Millisecond):
		t.Fatal("lease renewal failure did not cancel the lock-scoped context")
	}
	if err := context.Cause(lockCtx); err == nil || !strings.Contains(err.Error(), "renew denied") {
		t.Fatalf("lock context cause = %v, want renewal error", err)
	}
	if err := unlock(); err == nil || !strings.Contains(err.Error(), "renew denied") {
		t.Fatalf("unlock must surface renewal error, got %v", err)
	}
	if err := unlock(); err == nil || !strings.Contains(err.Error(), "renew denied") {
		t.Fatalf("idempotent unlock must retain renewal error, got %v", err)
	}
}

func TestAzureLocker_ContextCancelWhileWaiting(t *testing.T) {
	fs := newFakeBlobStore()
	a := newTestLocker(fs)
	b := newTestLocker(fs)
	b.maxWait = time.Hour // ensure cancellation, not timeout, ends the wait

	_, unlockA, err := a.Lock(context.Background(), "ds", "v1")
	if err != nil {
		t.Fatalf("A Lock: %v", err)
	}
	defer unlockA()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	_, _, err = b.Lock(ctx, "ds", "v1")
	if err == nil {
		t.Fatalf("expected context cancellation error")
	}
}
