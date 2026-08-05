package portalapi

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

// assetsFS holds the portal frontend build. For the skeleton this is a
// hand-written placeholder index.html; a later increment replaces the contents
// of assets/ with the Vite build output (dist/). The Go backend does not care
// which produced the files — it serves whatever is embedded.
//
//go:embed all:assets
var assetsFS embed.FS

// frontendFS returns the embedded assets rooted at the assets/ directory so
// request paths map directly to file names (index.html, assets/app-*.js, ...).
func frontendFS() fs.FS {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		// The embed directive guarantees assets/ exists; a failure here is a
		// build-time programming error, not a runtime condition.
		panic(err)
	}
	return sub
}

// readIndexHTML returns the SPA entry document used both for the /portal shell
// and as the single-page-app fallback for unmatched /portal/* routes.
func readIndexHTML() ([]byte, error) {
	return fs.ReadFile(frontendFS(), "index.html")
}

// serveIndexHTML writes the embedded index.html with SPA-appropriate headers.
func serveIndexHTML(w http.ResponseWriter, status int) {
	body, err := readIndexHTML()
	if err != nil {
		http.Error(w, "portal frontend is not embedded", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// serveAsset serves a static file from the embedded frontend build. It reports
// whether the request was handled; a false return means the caller should fall
// back to the SPA index (client-side routing).
func serveAsset(w http.ResponseWriter, name string) bool {
	clean := path.Clean("/" + strings.TrimPrefix(name, "/"))
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "index.html" {
		return false
	}
	data, err := fs.ReadFile(frontendFS(), clean)
	if err != nil {
		return false
	}
	ctype := mime.TypeByExtension(path.Ext(clean))
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ctype)
	// Vite emits content-hashed filenames, so built assets are immutable. The
	// placeholder has no such files; the header is harmless for it.
	if strings.HasPrefix(clean, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = w.Write(data)
	return true
}
