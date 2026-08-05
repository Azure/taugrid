// Package profile contains the internal render-time resource contract used by
// Tau manifest builders. It intentionally does not load legacy profile catalogs.
package profile

import "regexp"

var profileNameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// Profile is the in-memory resource contract consumed by renderers.
type Profile struct {
	Name        string
	SourceFile  string
	Extends     string
	Description string
	Lane        string
	Catalog     string
	Spec        map[string]any
}
