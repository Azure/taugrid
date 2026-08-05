package expstore

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/fileutil"
	"github.com/Azure/taugrid/portal/internal/portalbin"
)

func (s *Store) Export(ctx context.Context, opts ExportOptions) (ExportResult, error) {
	var result ExportResult
	err := s.withWriteLock(ctx, func() error {
		var err error
		result, err = s.exportLocked(ctx, opts)
		return err
	})
	return result, err
}

func (s *Store) exportLocked(ctx context.Context, opts ExportOptions) (ExportResult, error) {
	if err := ctx.Err(); err != nil {
		return ExportResult{}, err
	}
	if strings.TrimSpace(opts.Out) == "" {
		return ExportResult{}, fmt.Errorf("--out is required")
	}
	dest, err := filepath.Abs(opts.Out)
	if err != nil {
		return ExportResult{}, err
	}
	dest = filepath.Clean(dest)
	if dest == s.Root {
		return ExportResult{}, fmt.Errorf("--out must differ from --store")
	}
	if isSubpath(s.Root, dest) {
		return ExportResult{}, fmt.Errorf("--out must not be inside --store")
	}
	if err := prepareExportDestination(dest, opts.Force); err != nil {
		return ExportResult{}, err
	}

	result := ExportResult{
		Source:        s.Root,
		Destination:   dest,
		SchemaVersion: SchemaVersion,
	}
	for _, item := range []string{ManifestFile, s.manifest.Index, s.manifest.AppendLogDir, s.manifest.MetricsDir, s.manifest.ArtifactsDir} {
		src := filepath.Join(s.Root, item)
		if _, err := os.Stat(src); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return ExportResult{}, err
		}
		if err := copyPath(ctx, src, filepath.Join(dest, item), &result); err != nil {
			return ExportResult{}, err
		}
		result.PacketFiles = append(result.PacketFiles, item)
	}
	readme := map[string]any{
		"kind":           "tau.exp.export",
		"schema_version": SchemaVersion,
		"source":         s.Root,
		"created_at":     time.Now().UTC().Format(time.RFC3339),
		"local_usage": []string{
			portalbin.ExperimentCmd + " list --store . -o json",
			portalbin.ExperimentCmd + " status <question-or-group-or-run> --store . -o json",
			portalbin.ExperimentCmd + " sql --store . \"select * from runs limit 10\" --format csv",
		},
	}
	if err := fileutil.WriteJSONFileAtomic(filepath.Join(dest, "README.json"), readme); err != nil {
		return ExportResult{}, err
	}
	result.FilesCopied++
	result.PacketFiles = append(result.PacketFiles, "README.json")
	return result, nil
}

func prepareExportDestination(dest string, force bool) error {
	info, err := os.Stat(dest)
	if err != nil {
		if os.IsNotExist(err) {
			return os.MkdirAll(dest, 0o755)
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("--out %s exists and is not a directory", dest)
	}
	empty, err := isDirEmpty(dest)
	if err != nil {
		return err
	}
	if !empty && !force {
		return fmt.Errorf("--out %s is not empty; pass --force to overwrite packet files", dest)
	}
	return nil
}

func copyPath(ctx context.Context, src, dest string, result *ExportResult) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dest, info.Mode(), result)
	}
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dest, rel)
		if d.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			result.DirsCreated++
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		return copyFile(path, target, info.Mode(), result)
	})
}

func copyFile(src, dest string, mode os.FileMode, result *ExportResult) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	result.FilesCopied++
	return nil
}

func isDirEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	_, err = f.Readdirnames(1)
	if err == io.EOF {
		return true, nil
	}
	return false, err
}

func isSubpath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != "." && rel != ".." && !strings.HasPrefix(rel, "../") && !strings.HasPrefix(rel, `..\`)
}
