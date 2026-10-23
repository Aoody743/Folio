// Package webassets serves the compiled Next.js export from the release binary.
package webassets

import (
	"bytes"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var files embed.FS

// Handler only serves built public assets; runtime data always comes from API.
func Handler() http.Handler {
	root, err := fs.Sub(files, "dist")
	if err != nil {
		panic(err)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "index.html"
		}
		info, statErr := fs.Stat(root, name)
		if statErr == nil && info.IsDir() {
			name = path.Join(name, "index.html")
		}
		body, readErr := fs.ReadFile(root, name)
		if readErr != nil && path.Ext(name) == "" {
			name += ".html"
			body, readErr = fs.ReadFile(root, name)
		}
		if readErr != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
		if strings.HasSuffix(name, ".html") {
			nonceBytes := make([]byte, 24)
			if _, err := rand.Read(nonceBytes); err != nil {
				http.Error(w, "Cannot serve page", 500)
				return
			}
			nonce := base64.RawStdEncoding.EncodeToString(nonceBytes)
			// Next's exported hydration scripts are static and trusted. Per-request
			// nonces keep them working without permitting arbitrary inline scripts.
			body = bytes.ReplaceAll(body, []byte("<script"), []byte(`<script nonce="`+nonce+`"`))
			w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'nonce-"+nonce+"'; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'; form-action 'self'")
			w.Header().Set("Cache-Control", "no-store")
		} else if strings.HasPrefix(name, "_next/static/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	})
}
