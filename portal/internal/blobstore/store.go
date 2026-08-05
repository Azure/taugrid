package blobstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
)

type Store interface {
	UploadFile(ctx context.Context, ref DurableRef, path string) (DurableRef, error)
	Verify(ctx context.Context, ref DurableRef) error
	SignedURL(ctx context.Context, ref DurableRef, expiry time.Duration) (string, error)
}

type FileStore struct {
	RootDir string
	BaseURI string
}

func NewFileStore(rootDir string) (*FileStore, error) {
	rootDir = strings.TrimSpace(rootDir)
	if rootDir == "" {
		return nil, fmt.Errorf("file object store root is required")
	}
	abs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	return &FileStore{RootDir: abs, BaseURI: fileURI(abs)}, nil
}

func (s *FileStore) UploadFile(ctx context.Context, ref DurableRef, path string) (DurableRef, error) {
	if err := ctx.Err(); err != nil {
		return DurableRef{}, err
	}
	dst, err := s.pathForRef(ref)
	if err != nil {
		return DurableRef{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return DurableRef{}, err
	}
	if err := copyFileAtomic(dst, path); err != nil {
		return DurableRef{}, err
	}
	if err := writeRefMetadata(dst, ref); err != nil {
		return DurableRef{}, err
	}
	if err := s.Verify(ctx, ref); err != nil {
		return DurableRef{}, err
	}
	return ref, nil
}

func (s *FileStore) Verify(ctx context.Context, ref DurableRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.pathForRef(ref)
	if err != nil {
		return err
	}
	digest, size, err := fileutil.FileSHA256(path)
	if err != nil {
		return err
	}
	if DigestWithAlgorithm(digest) != ref.Digest {
		return fmt.Errorf("verify blob %s: digest %s does not match %s", ref.URI, DigestWithAlgorithm(digest), ref.Digest)
	}
	if size != ref.SizeBytes {
		return fmt.Errorf("verify blob %s: size %d does not match %d", ref.URI, size, ref.SizeBytes)
	}
	return nil
}

func (s *FileStore) SignedURL(ctx context.Context, ref DurableRef, _ time.Duration) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	path, err := s.pathForRef(ref)
	if err != nil {
		return "", err
	}
	return fileURI(path), nil
}

func (s *FileStore) pathForRef(ref DurableRef) (string, error) {
	blobPath := filepath.Clean(filepath.FromSlash(strings.TrimSpace(ref.BlobPath)))
	if blobPath == "." || strings.HasPrefix(blobPath, ".."+string(filepath.Separator)) || blobPath == ".." || filepath.IsAbs(blobPath) {
		return "", fmt.Errorf("invalid blob path %q", ref.BlobPath)
	}
	candidate := filepath.Join(s.RootDir, blobPath)
	rel, err := filepath.Rel(s.RootDir, candidate)
	if err != nil {
		return "", err
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("blob path escapes object store root")
	}
	return candidate, nil
}

func ContentTypeForFile(path string) string {
	if typ := mime.TypeByExtension(strings.ToLower(filepath.Ext(path))); typ != "" {
		return typ
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	var buf [512]byte
	n, err := f.Read(buf[:])
	if err != nil && err != io.EOF {
		return "application/octet-stream"
	}
	return http.DetectContentType(buf[:n])
}

func copyFileAtomic(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func writeRefMetadata(path string, ref DurableRef) error {
	raw, err := json.MarshalIndent(ref, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteFileAtomic(path+".meta.json", append(raw, '\n'), 0o644)
}

func fileURI(path string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
}
