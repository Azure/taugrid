// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package version

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

func Info(name string) string {
	return name + " " + Version + "\n" +
		"  commit: " + Commit + "\n" +
		"  built:  " + Date
}
