// Package projectzip builds a deterministic zip archive of a research project
// directory for use as a Ray runtime_env working_dir.
//
// Ray resolves working_dir independently on every node and prepends the
// unpacked directory to PYTHONPATH, which is what makes sibling modules and
// local packages importable inside @ray.remote tasks on workers — not just in
// the driver. Ray requires remote working_dir URIs to point at a .zip, and it
// strips a single top-level directory when unpacking, so entries here are
// written at the archive root to keep import paths predictable.
//
// Archives are byte-for-byte deterministic: entries are sorted, timestamps are
// fixed, and compression is fixed. The same tree always produces the same
// bytes, so a rendered workload spec stays stable across submissions and the
// payload digest remains meaningful.
package projectzip

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultExcludes are directory and file patterns never worth shipping to a
// Ray cluster. They are matched per path segment for directories and against
// the base name for files.
var DefaultExcludes = []string{
	".git",
	".hg",
	".svn",
	".venv",
	"venv",
	"__pycache__",
	".mypy_cache",
	".pytest_cache",
	".ruff_cache",
	".ipynb_checkpoints",
	"node_modules",
	".tau",
	".DS_Store",
	"*.pyc",
	"*.pyo",
	"*.so",
	"*.egg-info",
}

// fixedModTime keeps archives reproducible. The zip format stores MS-DOS
// timestamps, whose epoch starts in 1980; this is the earliest representable
// value that round-trips cleanly.
var fixedModTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// Options controls archive construction.
type Options struct {
	// Dir is the project root to archive.
	Dir string
	// Excludes are additional glob patterns, matched against both the base
	// name and the archive-relative path.
	Excludes []string
	// MaxBytes caps the uncompressed total. Zero means unlimited.
	MaxBytes int64
}

// File is one archived entry, used for reporting.
type File struct {
	Name string
	Size int64
}

// Build walks Dir and returns a deterministic zip archive plus the entries it
// contains, sorted by descending size so callers can report the biggest
// contributors when a size limit is hit.
func Build(o Options) ([]byte, []File, error) {
	root, err := filepath.Abs(o.Dir)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving project dir: %w", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, fmt.Errorf("project dir %s: %w", o.Dir, err)
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("project dir %s is not a directory", o.Dir)
	}

	excludes := append(append([]string{}, DefaultExcludes...), o.Excludes...)

	var files []File
	var total int64
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if excluded(rel, d.Name(), excludes) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// Symlinks are skipped rather than followed: a link pointing outside
		// the project would silently widen what gets shipped, and a cyclic
		// link would not terminate.
		if !d.Type().IsRegular() {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		total += fi.Size()
		files = append(files, File{Name: rel, Size: fi.Size()})
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walking project dir %s: %w", o.Dir, err)
	}
	if len(files) == 0 {
		return nil, nil, fmt.Errorf("project dir %s contains no files to ship", o.Dir)
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	if o.MaxBytes > 0 && total > o.MaxBytes {
		return nil, largestFirst(files), fmt.Errorf(
			"project directory %s holds %d bytes of shippable files, which exceeds the limit of %d bytes",
			o.Dir, total, o.MaxBytes)
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range files {
		header := &zip.FileHeader{Name: f.Name, Method: zip.Deflate}
		header.Modified = fixedModTime
		header.SetMode(0o644)
		w, err := zw.CreateHeader(header)
		if err != nil {
			return nil, nil, fmt.Errorf("archiving %s: %w", f.Name, err)
		}
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f.Name)))
		if err != nil {
			return nil, nil, fmt.Errorf("reading %s: %w", f.Name, err)
		}
		if _, err := w.Write(data); err != nil {
			return nil, nil, fmt.Errorf("archiving %s: %w", f.Name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, nil, fmt.Errorf("finalizing project archive: %w", err)
	}
	return buf.Bytes(), largestFirst(files), nil
}

func largestFirst(files []File) []File {
	out := make([]File, len(files))
	copy(out, files)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Size != out[j].Size {
			return out[i].Size > out[j].Size
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// excluded reports whether an entry matches any exclude pattern, either by
// base name or by its archive-relative path.
func excluded(rel, name string, patterns []string) bool {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if ok, err := path.Match(pattern, name); err == nil && ok {
			return true
		}
		if ok, err := path.Match(pattern, rel); err == nil && ok {
			return true
		}
		if strings.HasPrefix(rel, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
	}
	return false
}

// DescribeLargest renders the biggest entries for an over-size error message.
func DescribeLargest(files []File, n int) string {
	if len(files) > n {
		files = files[:n]
	}
	var b strings.Builder
	for _, f := range files {
		fmt.Fprintf(&b, "\n  %8d bytes  %s", f.Size, f.Name)
	}
	return b.String()
}
