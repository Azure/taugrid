// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package manifest

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Azure/taugrid/core/runconfig"
)

func TestManagedPassthroughCoversManifestSchema(t *testing.T) {
	paths := manifestYAMLPaths(reflect.TypeOf(Manifest{}), "")
	if len(paths) == 0 {
		t.Fatal("reflected no manifest paths; the guard is vacuous")
	}
	var missing []string
	for _, path := range paths {
		if !runconfig.IsKnownManagedKey(path) {
			missing = append(missing, path)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("manifest fields unknown to runconfig (they would warn spuriously): %s\n"+
			"add them to managedPassthroughPaths in core/runconfig/unknown.go",
			strings.Join(missing, ", "))
	}
}

func TestManagedPassthroughHasNoStaleEntries(t *testing.T) {
	paths := manifestYAMLPaths(reflect.TypeOf(Manifest{}), "")
	if len(paths) == 0 {
		t.Fatal("reflected no manifest paths; the guard is vacuous")
	}
	known := make(map[string]bool, len(paths))
	for _, path := range paths {
		known[path] = true
	}
	var stale []string
	for _, entry := range runconfig.ManagedPassthroughPaths() {
		if !known[entry] {
			stale = append(stale, entry)
		}
	}
	if len(stale) > 0 {
		t.Fatalf("managedPassthroughPaths entries with no matching manifest field: %s\n"+
			"the field was removed or renamed; drop the entry from "+
			"managedPassthroughPaths in core/runconfig/unknown.go so the key "+
			"warns again instead of being silently tolerated",
			strings.Join(stale, ", "))
	}
}

func manifestYAMLPaths(t reflect.Type, prefix string) []string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct {
		return nil
	}
	var paths []string
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		if field.PkgPath != "" {
			continue
		}
		tag := field.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			continue
		}
		path := name
		if prefix != "" {
			path = prefix + "." + name
		}
		paths = append(paths, path)
		ft := field.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		for ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array {
			ft = ft.Elem()
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
		}
		if ft.Kind() == reflect.Struct {
			paths = append(paths, manifestYAMLPaths(ft, path)...)
		}
	}
	return paths
}
