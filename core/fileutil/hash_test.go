package fileutil

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSHA256Hex(t *testing.T) {
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	if got := SHA256Hex([]byte("hello")); got != want {
		t.Fatalf("SHA256Hex() = %q, want %q", got, want)
	}
}

func TestFileSHA256(t *testing.T) {
	path := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, size, err := FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	if size != 5 {
		t.Fatalf("FileSHA256() size = %d, want 5", size)
	}
	if digest != SHA256Hex([]byte("hello")) {
		t.Fatalf("FileSHA256() digest = %q, want %q", digest, SHA256Hex([]byte("hello")))
	}
}

func TestShortDigest(t *testing.T) {
	if got := ShortDigest("abcdef", 3); got != "abc" {
		t.Fatalf("ShortDigest() = %q, want %q", got, "abc")
	}
	if got := ShortDigest("abcdef", 0); got != "abcdef" {
		t.Fatalf("ShortDigest() with non-positive length = %q, want full digest", got)
	}
	if got := ShortDigest("abc", 12); got != "abc" {
		t.Fatalf("ShortDigest() with short digest = %q, want %q", got, "abc")
	}
}
