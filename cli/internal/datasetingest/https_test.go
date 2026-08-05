package datasetingest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Azure/taugrid/cli/internal/dataset"
	"github.com/Azure/taugrid/cli/internal/datasetingest"
)

func TestHTTPSSource_OpenSuccess(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/root/a.bin" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("payload"))
	}))
	defer server.Close()

	source, err := datasetingest.NewHTTPSSource(server.URL+"/root", server.Client())
	if err != nil {
		t.Fatalf("NewHTTPSSource: %v", err)
	}
	rc, size, err := source.Open(context.Background(), "a.bin")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != "payload" || size != int64(len(got)) {
		t.Fatalf("Open returned bytes=%q size=%d", got, size)
	}
}

func TestHTTPSSource_RejectsNotFoundAndUnsafeURLs(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect-http":
			http.Redirect(w, r, "http://example.test/nope", http.StatusFound)
		case "/redirect-query":
			http.Redirect(w, r, "/next?token=nope", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	if _, err := datasetingest.NewHTTPSSource(server.URL+"/root?sig=nope", server.Client()); err == nil {
		t.Fatal("source root query must be rejected")
	}
	source, err := datasetingest.NewHTTPSSource(server.URL, server.Client())
	if err != nil {
		t.Fatalf("NewHTTPSSource: %v", err)
	}
	for _, path := range []string{"missing", "redirect-http", "redirect-query", "../escape"} {
		if rc, _, err := source.Open(context.Background(), path); err == nil {
			if rc != nil {
				_ = rc.Close()
			}
			t.Fatalf("Open(%q) should fail", path)
		}
	}
}

func TestRunWorker_HTTPSSourceVerifiesSizeAndHash(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("abc"))
	}))
	defer server.Close()
	source, err := datasetingest.NewHTTPSSource(server.URL, server.Client())
	if err != nil {
		t.Fatalf("NewHTTPSSource: %v", err)
	}

	for _, tc := range []struct {
		name  string
		bytes int64
		hash  string
	}{
		{name: "size", bytes: 4, hash: sha256ForHTTPS([]byte("abc"))},
		{name: "hash", bytes: 3, hash: sha256ForHTTPS([]byte("def"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := makeReg(t)
			_ = makeRec(t, reg, "ds", "v1", []dataset.File{{Path: "a.bin", Bytes: tc.bytes, SHA256: tc.hash}})
			_, err := datasetingest.RunWorker(context.Background(), "ds", "v1", datasetingest.WorkerConfig{
				Registry: reg,
				Source:   source,
				Sink:     datasetingest.FileSink{Root: t.TempDir()},
				Locker:   datasetingest.FileLocker{Dir: t.TempDir()},
			})
			if err == nil || (!strings.Contains(err.Error(), "byte count mismatch") && !strings.Contains(err.Error(), "sha256 mismatch")) {
				t.Fatalf("RunWorker error = %v, want size/hash verification error", err)
			}
		})
	}
}

func sha256ForHTTPS(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
