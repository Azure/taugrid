// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/storage"
	"github.com/Azure/taugrid/core/kube"
)

// datasetRegistryPaths wires the dataset.Registry to the storage path helpers.
func datasetRegistryPaths() dataset.Paths {
	return dataset.Paths{
		DatasetsDir:      storage.DatasetRegistryDatasetsDir,
		DatasetDir:       storage.DatasetRegistryDatasetDir,
		VersionDir:       storage.DatasetRegistryVersionDir,
		RecordFile:       storage.DatasetRegistryRecordFile,
		AliasesDir:       storage.DatasetRegistryAliasesDir,
		AliasFile:        storage.DatasetRegistryAliasFile,
		IngestStatusFile: storage.DatasetRegistryIngestStatusFile,
	}
}

// pvcBackend adapts the helper-pod-over-blob-training machinery (the same
// pattern tau data model uses) to the dataset.Backend interface. The dataset
// registry control-plane JSON lives on the blob-training PVC at
// /data/dataset-registry, alongside the model registry.
type pvcBackend struct {
	kubeContext string
	namespace   string
	pvcName     string
}

func newPVCBackend(kubeContext, namespace string) *pvcBackend {
	return &pvcBackend{kubeContext: kubeContext, namespace: namespace, pvcName: defaultTauPVCName}
}

func (b *pvcBackend) ReadFile(ctx context.Context, path string) ([]byte, error) {
	raw, err := fetchPVCFile(ctx, b.kubeContext, b.namespace, datasetRunLabel(path), b.pvcName, path)
	if err != nil {
		if isPVCNotFound(err) {
			return nil, dataset.ErrNotExist
		}
		return nil, err
	}
	return raw, nil
}

func (b *pvcBackend) WriteFile(ctx context.Context, path string, data []byte, overwrite bool) error {
	if !overwrite {
		// Best-effort immutability on the helper-pod path: refuse if the target
		// already exists. This is a narrow TOCTOU window (v1 assumes a single
		// registrar writer); Blob soft-delete/versioning is the recovery net.
		// The az registry backend provides server-enforced conditional writes.
		if _, err := b.ReadFile(ctx, path); err == nil {
			return dataset.ErrExist
		} else if !dataset.IsNotExist(err) {
			return err
		}
	}
	return writePVCFile(ctx, b.kubeContext, b.namespace, datasetRunLabel(path), b.pvcName, path, data)
}

func (b *pvcBackend) List(ctx context.Context, dir string) ([]string, error) {
	entries, err := fetchPVCList(ctx, b.kubeContext, b.namespace, datasetRunLabel(dir), b.pvcName, dir)
	if err != nil {
		if isPVCNotFound(err) {
			return nil, dataset.ErrNotExist
		}
		return nil, err
	}
	return entries, nil
}

func (b *pvcBackend) Delete(ctx context.Context, path string) error {
	return deletePVCFile(ctx, b.kubeContext, b.namespace, datasetRunLabel(path), b.pvcName, path)
}

// datasetRunLabel derives a short, pod-name-safe label from a registry path.
func datasetRunLabel(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), ".json")
	return "ds-" + sanitizePodName(base)
}

// deletePVCFile removes a single file on the named PVC via a one-shot helper
// pod, mirroring writePVCFile. Removing a missing path is not an error.
func deletePVCFile(ctx context.Context, kubeContext, namespace, runName, pvcName, path string) error {
	if pvcName == "" {
		pvcName = defaultTauPVCName
	}
	podName := fmt.Sprintf("tau-rm-%s-%d", sanitizePodName(runName), time.Now().Unix())
	if len(podName) > 60 {
		podName = podName[:60]
	}
	script := fmt.Sprintf("rm -f %s\n", shellSingleQuote(path))
	podYAML, err := helperPodYAML(helperPodSpec{
		Name:      podName,
		Namespace: namespace,
		LabelApp:  "tau-pvc-rm",
		Image:     pvcHelperImage,
		PVCName:   pvcName,
		TTLSec:    int(pvcHelperPodTTL.Seconds()),
		Script:    script,
	})
	if err != nil {
		return fmt.Errorf("render helper pod: %w", err)
	}
	r := kube.New(kubeContext)
	if _, err := r.Raw(ctx, []string{"apply", "-n", namespace, "-f", "-"}, podYAML); err != nil {
		return fmt.Errorf("create helper pod: %w", err)
	}
	defer func() {
		delCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_, _ = r.Raw(delCtx, []string{"delete", "pod", "-n", namespace, podName, "--wait=false", "--ignore-not-found"}, nil)
	}()
	phase, err := waitForHelperPodTerminal(ctx, r, namespace, podName, 90*time.Second)
	if err != nil {
		logs, _ := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		return fmt.Errorf("delete helper pod did not finish: %w (logs: %s)", err, strings.TrimSpace(logs))
	}
	if phase != "Succeeded" {
		logs, _ := r.Raw(ctx, []string{"logs", "-n", namespace, podName}, nil)
		return fmt.Errorf("delete helper pod did not succeed: phase=%s (logs: %s)", phase, strings.TrimSpace(logs))
	}
	return nil
}

// hashedFile is one file scanned by a dataSource.
type hashedFile struct {
	Path   string
	Bytes  int64
	SHA256 string
}

// dataSource scans and hashes the actual dataset bytes for register/verify. The
// local implementation hashes a directory in Go; the az implementation shells
// out to `az storage blob` using the caller's Azure RBAC (no SAS).
type dataSource interface {
	scan(ctx context.Context) ([]hashedFile, error)
	describe() string
}

// localDataSource hashes a local directory tree.
type localDataSource struct{ root string }

func (s localDataSource) describe() string { return "local:" + s.root }

func (s localDataSource) scan(ctx context.Context) ([]hashedFile, error) {
	info, err := os.Stat(s.root)
	if err != nil {
		return nil, err
	}
	var files []hashedFile
	if !info.IsDir() {
		hf, err := hashLocalFile(s.root, filepath.Base(s.root))
		if err != nil {
			return nil, err
		}
		return []hashedFile{hf}, nil
	}
	err = filepath.Walk(s.root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if fi.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		hf, err := hashLocalFile(p, rel)
		if err != nil {
			return err
		}
		files = append(files, hf)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func hashLocalFile(absPath, relPath string) (hashedFile, error) {
	f, err := os.Open(absPath)
	if err != nil {
		return hashedFile{}, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return hashedFile{}, err
	}
	return hashedFile{Path: relPath, Bytes: n, SHA256: hex.EncodeToString(h.Sum(nil))}, nil
}

// azDataSource lists and hashes blobs under account/container/prefix using the
// caller's Azure RBAC (az --auth-mode login). It never uses a SAS token.
type azDataSource struct {
	account   string
	container string
	prefix    string
}

func (s azDataSource) describe() string {
	return fmt.Sprintf("az://%s/%s/%s", s.account, s.container, s.prefix)
}

func (s azDataSource) scan(ctx context.Context) ([]hashedFile, error) {
	if _, err := exec.LookPath("az"); err != nil {
		return nil, fmt.Errorf("az CLI is required to scan %s (install Azure CLI and `az login`): %w", s.describe(), err)
	}
	listArgs := []string{
		"storage", "blob", "list",
		"--account-name", s.account,
		"--container-name", s.container,
		"--auth-mode", "login",
		"-o", "json",
	}
	// Treat a non-empty prefix as a directory boundary so a prefix of "foo"
	// matches foo/shard.bin but not foobar.bin, and the stripped relative path
	// round-trips through NodeLocalStage.BlobURL (which re-joins prefix+path).
	listPrefix := strings.TrimSuffix(s.prefix, "/")
	if listPrefix != "" {
		listPrefix += "/"
		listArgs = append(listArgs, "--prefix", listPrefix)
	}
	out, err := runAz(ctx, listArgs)
	if err != nil {
		return nil, err
	}
	var blobs []struct {
		Name       string `json:"name"`
		Properties struct {
			ContentLength int64 `json:"contentLength"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(out, &blobs); err != nil {
		return nil, fmt.Errorf("parse az blob list: %w", err)
	}
	var files []hashedFile
	for _, b := range blobs {
		rel := strings.TrimPrefix(b.Name, listPrefix)
		if rel == "" || strings.HasSuffix(b.Name, "/") {
			continue
		}
		sum, err := s.hashBlob(ctx, b.Name)
		if err != nil {
			return nil, err
		}
		files = append(files, hashedFile{Path: rel, Bytes: b.Properties.ContentLength, SHA256: sum})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func (s azDataSource) hashBlob(ctx context.Context, name string) (string, error) {
	tmp, err := os.CreateTemp("", "tau-ds-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	args := []string{
		"storage", "blob", "download",
		"--account-name", s.account,
		"--container-name", s.container,
		"--name", name,
		"--auth-mode", "login",
		"--no-progress",
		"--file", tmpName,
		"-o", "none",
	}
	if _, err := runAz(ctx, args); err != nil {
		return "", err
	}
	hf, err := hashLocalFile(tmpName, name)
	if err != nil {
		return "", err
	}
	return hf.SHA256, nil
}

func runAz(ctx context.Context, args []string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "az", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("az %s failed: %w (%s)", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// azBackend is a dataset.Backend over a blob container in the dedicated dataset
// storage account, using the caller's Azure RBAC (az --auth-mode login, never a
// SAS). It is the no-cluster registry path: register/verify can read and write
// records from a laptop or CI before any cluster exists. Registry paths are
// absolute (/data/dataset-registry/...); the backend maps them to container-
// relative blob names by stripping rootPrefix.
type azBackend struct {
	account    string
	container  string
	rootPrefix string
}

func newAzBackend(account, container string) *azBackend {
	return &azBackend{account: account, container: container, rootPrefix: storage.DatasetRegistryDir}
}

func (b *azBackend) blobName(p string) string {
	rel := strings.TrimPrefix(p, b.rootPrefix)
	return strings.TrimPrefix(rel, "/")
}

func (b *azBackend) ensureAz() error {
	if _, err := exec.LookPath("az"); err != nil {
		return fmt.Errorf("az CLI is required for the az registry backend (install Azure CLI and `az login`): %w", err)
	}
	return nil
}

func (b *azBackend) ReadFile(ctx context.Context, p string) ([]byte, error) {
	if err := b.ensureAz(); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "tau-ds-get-*")
	if err != nil {
		return nil, err
	}
	tmpName := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpName)
	args := []string{
		"storage", "blob", "download",
		"--account-name", b.account,
		"--container-name", b.container,
		"--name", b.blobName(p),
		"--auth-mode", "login",
		"--no-progress",
		"--file", tmpName,
		"-o", "none",
	}
	if _, err := runAz(ctx, args); err != nil {
		if isAzNotFound(err) {
			return nil, dataset.ErrNotExist
		}
		return nil, err
	}
	return os.ReadFile(tmpName)
}

func (b *azBackend) WriteFile(ctx context.Context, p string, data []byte, overwrite bool) error {
	if err := b.ensureAz(); err != nil {
		return err
	}
	tmp, err := os.CreateTemp("", "tau-ds-put-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	tmp.Close()
	defer os.Remove(tmpName)
	args := []string{
		"storage", "blob", "upload",
		"--account-name", b.account,
		"--container-name", b.container,
		"--name", b.blobName(p),
		"--auth-mode", "login",
		"--file", tmpName,
		"--overwrite", fmt.Sprintf("%t", overwrite),
		"-o", "none",
	}
	if _, err := runAz(ctx, args); err != nil {
		if !overwrite && (strings.Contains(err.Error(), "already exists") || strings.Contains(err.Error(), "BlobAlreadyExists")) {
			return dataset.ErrExist
		}
		return err
	}
	return nil
}

func (b *azBackend) List(ctx context.Context, dir string) ([]string, error) {
	if err := b.ensureAz(); err != nil {
		return nil, err
	}
	prefix := b.blobName(dir)
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	args := []string{
		"storage", "blob", "list",
		"--account-name", b.account,
		"--container-name", b.container,
		"--auth-mode", "login",
		"--delimiter", "/",
		"-o", "json",
	}
	if prefix != "" {
		args = append(args, "--prefix", prefix)
	}
	out, err := runAz(ctx, args)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, fmt.Errorf("parse az blob list: %w", err)
	}
	seen := map[string]bool{}
	var children []string
	for _, e := range entries {
		rest := strings.TrimPrefix(e.Name, prefix)
		rest = strings.TrimSuffix(rest, "/")
		if rest == "" {
			continue
		}
		if i := strings.Index(rest, "/"); i >= 0 {
			rest = rest[:i]
		}
		if !seen[rest] {
			seen[rest] = true
			children = append(children, rest)
		}
	}
	return children, nil
}

func (b *azBackend) Delete(ctx context.Context, p string) error {
	if err := b.ensureAz(); err != nil {
		return err
	}
	args := []string{
		"storage", "blob", "delete",
		"--account-name", b.account,
		"--container-name", b.container,
		"--name", b.blobName(p),
		"--auth-mode", "login",
		"-o", "none",
	}
	if _, err := runAz(ctx, args); err != nil {
		if isAzNotFound(err) {
			return nil
		}
		return err
	}
	return nil
}

func isAzNotFound(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "BlobNotFound") || strings.Contains(s, "ErrorCode:BlobNotFound") || strings.Contains(s, "does not exist")
}

// fileBackend is a dataset.Backend over a local filesystem directory. It maps
// the absolute registry layout (/data/dataset-registry/...) onto root by
// stripping rootPrefix, mirroring azBackend. It needs no cluster and no cloud,
// so the registry can be exercised end-to-end on a laptop or committed to a
// repo as a seed catalog, then promoted to the pvc/az backends unchanged.
type fileBackend struct {
	root       string
	rootPrefix string
}

func newFileBackend(root string) *fileBackend {
	return &fileBackend{root: root, rootPrefix: storage.DatasetRegistryDir}
}

// localPath maps an absolute registry path to a path under root. It fails
// closed if p escapes the registry prefix or resolves outside root, so an
// unsafe caller cannot read or write outside the seed directory.
func (b *fileBackend) localPath(p string) (string, error) {
	if p != b.rootPrefix && !strings.HasPrefix(p, b.rootPrefix+"/") {
		return "", fmt.Errorf("path %q is outside registry root %q", p, b.rootPrefix)
	}
	rel := strings.TrimPrefix(p, b.rootPrefix)
	rel = strings.TrimPrefix(rel, "/")
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes registry root", p)
	}
	full := filepath.Join(b.root, clean)
	rootAbs := filepath.Clean(b.root)
	if full != rootAbs && !strings.HasPrefix(full, rootAbs+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes registry root", p)
	}
	return full, nil
}

func (b *fileBackend) ReadFile(_ context.Context, p string) ([]byte, error) {
	lp, err := b.localPath(p)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(lp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, dataset.ErrNotExist
		}
		return nil, err
	}
	return data, nil
}

func (b *fileBackend) WriteFile(_ context.Context, p string, data []byte, overwrite bool) error {
	lp, err := b.localPath(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return err
	}
	if !overwrite {
		// O_EXCL gives a real create-or-fail so immutability does not rely on a
		// racy read-then-write (unlike the helper-pod pvc backend).
		f, err := os.OpenFile(lp, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			if errors.Is(err, os.ErrExist) {
				return dataset.ErrExist
			}
			return err
		}
		if _, err := f.Write(data); err != nil {
			return errors.Join(err, f.Close())
		}
		if err := f.Sync(); err != nil {
			return errors.Join(err, f.Close())
		}
		return f.Close()
	}
	return b.writeFileAtomic(lp, data)
}

// WriteFileAtomic implements dataset.AtomicWriteBackend for mutable ingest
// status files. The replacement is durable before rename and leaves no
// truncate-in-place window for concurrent status readers.
func (b *fileBackend) WriteFileAtomic(_ context.Context, p string, data []byte) error {
	lp, err := b.localPath(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lp), 0o755); err != nil {
		return err
	}
	return b.writeFileAtomic(lp, data)
}

func (b *fileBackend) writeFileAtomic(lp string, data []byte) error {
	// Overwrite (mutable status) path: write to a temp file in the same
	// directory and rename over the target so a reader never observes a
	// partially-written status file (atomic replace on POSIX).
	tmp, err := os.CreateTemp(filepath.Dir(lp), ".tau-ds-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpName, lp); err != nil {
		return err
	}
	cleanup = false
	return nil
}

func (b *fileBackend) List(_ context.Context, dir string) ([]string, error) {
	lp, err := b.localPath(dir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(lp)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, dataset.ErrNotExist
		}
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names, nil
}

func (b *fileBackend) Delete(_ context.Context, p string) error {
	lp, err := b.localPath(p)
	if err != nil {
		return err
	}
	if err := os.Remove(lp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
