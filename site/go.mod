module github.com/Azure/taugrid/site

go 1.26.0

// Docsy is a Hugo module. Keep this requirement explicit; go mod tidy cannot
// discover theme-only imports because this module contains no Go packages.
require github.com/google/docsy v0.11.0
