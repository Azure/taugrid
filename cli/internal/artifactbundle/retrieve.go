package artifactbundle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

type Object struct {
	Name string `json:"name"`
	Size int64  `json:"size_bytes"`
}

type Store interface {
	Read(context.Context, string) ([]byte, error)
	List(context.Context, string) ([]Object, error)
	Download(context.Context, string, io.Writer) error
}

type DownloadedFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size_bytes"`
	SHA256 string `json:"sha256"`
}

func PVCRelativePath(absolute string) (string, error) {
	clean := path.Clean(strings.TrimSpace(absolute))
	if clean == "/data" {
		return "", nil
	}
	if !strings.HasPrefix(clean, "/data/") {
		return "", fmt.Errorf("durable artifact path %q is not under the Blob CSI mount /data", absolute)
	}
	return strings.TrimPrefix(clean, "/data/"), nil
}

func Load(ctx context.Context, store Store, resultRoot, bundleID string) (Manifest, error) {
	manifestPath := CurrentManifestPath(resultRoot)
	completionPath := CurrentCompletionPath(resultRoot)
	if strings.TrimSpace(bundleID) != "" {
		manifestPath = GenerationManifestPath(resultRoot, bundleID)
		completionPath = GenerationCompletionPath(resultRoot, bundleID)
	}
	manifestKey, err := PVCRelativePath(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	raw, err := store.Read(ctx, manifestKey)
	if err != nil {
		return Manifest{}, fmt.Errorf("read artifact bundle manifest %s: %w", manifestPath, err)
	}
	manifest, err := Parse(raw)
	if err != nil {
		return Manifest{}, err
	}
	if path.Clean(manifest.ResultRoot) != path.Clean(resultRoot) {
		return Manifest{}, fmt.Errorf("artifact bundle result root %q does not match requested root %q", manifest.ResultRoot, resultRoot)
	}
	if strings.TrimSpace(bundleID) != "" && manifest.BundleID != strings.TrimSpace(bundleID) {
		return Manifest{}, fmt.Errorf("artifact bundle manifest ID %q does not match workload ID %q", manifest.BundleID, bundleID)
	}
	completionKey, err := PVCRelativePath(completionPath)
	if err != nil {
		return Manifest{}, err
	}
	completion, err := store.Read(ctx, completionKey)
	if err != nil {
		return Manifest{}, fmt.Errorf("artifact bundle is not acknowledged at %s: %w", completionPath, err)
	}
	if strings.TrimSpace(string(completion)) != "complete "+manifest.BundleID {
		return Manifest{}, fmt.Errorf("artifact bundle acknowledgement %s is invalid", completionPath)
	}
	if manifest.Publication != nil {
		key, err := PVCRelativePath(manifest.Publication.Completion)
		if err != nil {
			return Manifest{}, err
		}
		raw, err := store.Read(ctx, key)
		if err != nil {
			return Manifest{}, fmt.Errorf("staged artifacts are not completely published: %w", err)
		}
		if strings.TrimSpace(string(raw)) != "complete "+manifest.Publication.ID {
			return Manifest{}, fmt.Errorf("staged artifact publication marker %s is invalid", manifest.Publication.Completion)
		}
	}
	return manifest, nil
}

func Enumerate(ctx context.Context, store Store, manifest Manifest) ([]Object, error) {
	objects := map[string]Object{}
	for _, spec := range manifest.Paths {
		prefixPath := spec.Path
		if spec.Kind == "glob" {
			prefixPath = globPrefix(spec.Path)
		}
		prefix, err := PVCRelativePath(prefixPath)
		if err != nil {
			return nil, err
		}
		if prefix != "" && !strings.HasSuffix(prefix, "/") && spec.Kind == "tree" {
			prefix += "/"
		}
		listed, err := store.List(ctx, prefix)
		if err != nil {
			return nil, fmt.Errorf("enumerate artifact bundle path %s: %w", spec.Path, err)
		}
		matches := 0
		for _, object := range listed {
			if spec.Kind == "glob" {
				absolute := "/data/" + strings.TrimPrefix(object.Name, "/")
				ok, matchErr := path.Match(spec.Path, absolute)
				if matchErr != nil {
					return nil, fmt.Errorf("match artifact bundle glob %q: %w", spec.Path, matchErr)
				}
				if !ok {
					continue
				}
			} else if spec.Kind == "file" && object.Name != prefix {
				continue
			}
			objects[object.Name] = object
			matches++
		}
		if matches == 0 && !spec.Optional {
			return nil, fmt.Errorf("artifact bundle required path %s is empty", spec.Path)
		}
	}
	out := make([]Object, 0, len(objects))
	for _, object := range objects {
		out = append(out, object)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func globPrefix(pattern string) string {
	index := strings.IndexAny(pattern, "*?[")
	if index < 0 {
		return pattern
	}
	prefix := pattern[:index]
	if slash := strings.LastIndex(prefix, "/"); slash >= 0 {
		return prefix[:slash+1]
	}
	return "/data/"
}

func Download(ctx context.Context, store Store, manifest Manifest, objects []Object, destination string) ([]DownloadedFile, error) {
	if strings.TrimSpace(destination) == "" {
		return nil, fmt.Errorf("artifact bundle destination is required")
	}
	root, err := filepath.Abs(destination)
	if err != nil {
		return nil, fmt.Errorf("resolve artifact bundle destination: %w", err)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create artifact bundle destination: %w", err)
	}
	if info, err := os.Lstat(root); err != nil {
		return nil, err
	} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("artifact bundle destination %s must be a real directory", root)
	}
	targets := make([]string, len(objects))
	seenTargets := make(map[string]string, len(objects))
	for i, object := range objects {
		target, err := downloadTarget(root, object.Name)
		if err != nil {
			return nil, err
		}
		if previous, exists := seenTargets[target]; exists {
			return nil, fmt.Errorf("artifact bundle objects %q and %q resolve to the same destination", previous, object.Name)
		}
		seenTargets[target] = object.Name
		if _, err := os.Lstat(target); err == nil {
			return nil, fmt.Errorf("artifact bundle destination file already exists: %s", target)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		targets[i] = target
	}
	metadataDir := filepath.Join(root, ".tau-bundle")
	for _, target := range []string{
		filepath.Join(metadataDir, "manifest.json"),
		filepath.Join(metadataDir, "files.json"),
	} {
		if _, err := os.Lstat(target); err == nil {
			return nil, fmt.Errorf("artifact bundle metadata file already exists: %s", target)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	files := make([]DownloadedFile, 0, len(objects))
	for i, object := range objects {
		target := targets[i]
		if err := ensureSafeDirectory(root, filepath.Dir(target)); err != nil {
			return nil, err
		}
		file, err := os.CreateTemp(filepath.Dir(target), ".tau-download-*")
		if err != nil {
			return nil, err
		}
		tmp := file.Name()
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			_ = os.Remove(tmp)
			return nil, err
		}
		hash := sha256.New()
		writer := io.MultiWriter(file, hash)
		downloadErr := store.Download(ctx, object.Name, writer)
		closeErr := file.Close()
		if downloadErr != nil {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("download artifact bundle object %s: %w", object.Name, downloadErr)
		}
		if closeErr != nil {
			_ = os.Remove(tmp)
			return nil, closeErr
		}
		info, err := os.Stat(tmp)
		if err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		if object.Size >= 0 && info.Size() != object.Size {
			_ = os.Remove(tmp)
			return nil, fmt.Errorf("downloaded artifact %s has %d bytes, expected %d", object.Name, info.Size(), object.Size)
		}
		if err := os.Link(tmp, target); err != nil {
			_ = os.Remove(tmp)
			return nil, err
		}
		if err := os.Remove(tmp); err != nil {
			_ = os.Remove(target)
			return nil, err
		}
		files = append(files, DownloadedFile{
			Path:   object.Name,
			Size:   info.Size(),
			SHA256: hex.EncodeToString(hash.Sum(nil)),
		})
	}
	if err := ensureSafeDirectory(root, metadataDir); err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(filepath.Join(metadataDir, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	if err := writeJSONAtomic(filepath.Join(metadataDir, "files.json"), files); err != nil {
		return nil, err
	}
	return files, nil
}

func downloadTarget(root, objectName string) (string, error) {
	if objectName == "" || strings.HasPrefix(objectName, "/") || path.Clean(objectName) != objectName {
		return "", fmt.Errorf("artifact bundle object name %q is not a canonical relative path", objectName)
	}
	target := filepath.Join(root, filepath.FromSlash(objectName))
	if !pathWithin(root, target) {
		return "", fmt.Errorf("artifact bundle object %q escapes destination", objectName)
	}
	return target, nil
}

func ensureSafeDirectory(root, directory string) error {
	rel, err := filepath.Rel(root, directory)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("artifact bundle directory %q escapes destination", directory)
	}
	current := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		switch {
		case os.IsNotExist(err):
			if err := os.Mkdir(current, 0o755); err != nil {
				return err
			}
		case err != nil:
			return err
		case info.Mode()&os.ModeSymlink != 0 || !info.IsDir():
			return fmt.Errorf("artifact bundle destination parent %s must be a real directory", current)
		}
	}
	return nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func writeJSONAtomic(target string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	file, err := os.CreateTemp(filepath.Dir(target), ".tau-metadata-*")
	if err != nil {
		return err
	}
	tmp := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Link(tmp, target); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Remove(tmp); err != nil {
		_ = os.Remove(target)
		return err
	}
	return nil
}
