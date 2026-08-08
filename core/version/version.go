// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package version exposes build metadata for the binaries that link it.
//
// Version, Commit, and Date are set at build time via -ldflags -X; see the
// project Makefile and Dockerfile. They keep sensible defaults so `go run` and
// `go build` (without ldflags) still produce a usable, self-describing binary.
//
// The package is shared by tau and taugrid-portal, so it holds no product name
// of its own: callers pass theirs to Info.
package version

// These are overridden via -ldflags at build time. Defaults apply to local
// `go run`/`go build` invocations that do not pass ldflags.
var (
	// Version is the human-readable release version (e.g. "v1.2.3").
	Version = "dev"
	// Commit is the full git commit the binary was built from.
	Commit = "none"
	// Date is the source commit timestamp (RFC3339).
	Date = "unknown"
)

// Info returns a multi-line, human-readable description of the build, headed by
// the calling binary's name.
func Info(name string) string {
	return name + " " + Version + "\n" +
		"  commit: " + Commit + "\n" +
		"  built:  " + Date
}
