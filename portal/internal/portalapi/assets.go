// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package portalapi

import (
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed all:assets
var assetsFS embed.FS

func frontendFS() fs.FS {
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		panic(err)
	}
	return sub
}

func readIndexHTML() ([]byte, error) {
	return fs.ReadFile(frontendFS(), "index.html")
}

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
	if strings.HasPrefix(clean, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = w.Write(data)
	return true
}
