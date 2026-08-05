package datasetingest_test

import (
	"testing"

	"github.com/Azure/taugrid/cli/internal/datasetingest"
)

func TestValidateAzureURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid az", "az://acct/container/prefix", false},
		{"valid az no prefix", "az://acct/container", false},
		{"valid file", "file:///data/ds", false},
		{"empty", "", true},
		{"whitespace", "  az://a/b  ", true},
		{"http plaintext", "http://acct.blob.core.windows.net/c", true},
		{"public https source", "https://huggingface.co/datasets/org/ds/resolve/main", false},
		{"https query rejected", "https://huggingface.co/datasets/org/ds?token=nope", true},
		{"no scheme", "acct/container", true},
		{"unsupported scheme", "s3://bucket/key", true},
		{"sas query sig", "az://acct/container?sig=abc", true},
		{"sas query sv", "az://acct/container/p?sv=2020&sig=x", true},
		{"any query", "az://acct/container?foo=bar", true},
		{"userinfo", "az://user:pass@acct/container", true},
		{"fragment", "az://acct/container#frag", true},
		{"traversal", "az://acct/container/../secret", true},
		{"traversal file", "file:///data/../etc/passwd", true},
		{"accountkey", "az://acct/container/p;accountkey=abc", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := datasetingest.ValidateAzureURL(tc.url)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
		})
	}
}
