package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"math"
	"net/http"
	"strconv"
	"strings"
)

type staticAsset struct {
	identity, gzip                []byte
	contentType                   string
	version, identityTag, gzipTag string
}

// Snapshot and compress only the public dashboard at handler construction.
// Requests share immutable bytes; no per-request compression on the device.
func staticHandler(files fs.FS) http.Handler {
	assets := make(map[string]staticAsset)
	for name, contentType := range map[string]string{
		"index.html": "text/html; charset=utf-8",
		"app.js":     "text/javascript; charset=utf-8",
		"tasks.js":   "text/javascript; charset=utf-8",
		"theme.js":   "text/javascript; charset=utf-8",
		"style.css":  "text/css; charset=utf-8",
	} {
		body, err := fs.ReadFile(files, name)
		if err != nil {
			continue
		}
		assets["/"+name] = staticAsset{identity: body, contentType: contentType, version: fmt.Sprintf("%x", sha256.Sum256(body))}
	}
	// Rewrite the served HTML from the same snapshot used for asset responses.
	// Content hashes change URLs across builds without a frontend build step.
	if html, ok := assets["/index.html"]; ok {
		for path, asset := range assets {
			if path != "/index.html" {
				html.identity = bytes.ReplaceAll(html.identity, []byte(`"`+path+`"`), []byte(`"`+path+"?v="+asset.version+`"`))
			}
		}
		assets["/index.html"] = html
	}
	for path, asset := range assets {
		var compressed bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&compressed, gzip.BestCompression)
		_, _ = zw.Write(asset.identity)
		_ = zw.Close()
		asset.gzip = compressed.Bytes()
		asset.identityTag = fmt.Sprintf(`"%x"`, sha256.Sum256(asset.identity))
		asset.gzipTag = fmt.Sprintf(`"%x"`, sha256.Sum256(asset.gzip))
		assets[path] = asset
	}
	fallback := http.FileServer(http.FS(files))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if path == "/" {
			path = "/index.html"
		}
		asset, ok := assets[path]
		if !ok {
			fallback.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Add("Vary", "Accept-Encoding")
		compressed, acceptable := selectGzip(strings.Join(r.Header.Values("Accept-Encoding"), ","))
		if !acceptable {
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		body := asset.identity
		tag := asset.identityTag
		if compressed {
			body = asset.gzip
			tag = asset.gzipTag
			w.Header().Set("Content-Encoding", "gzip")
		}
		w.Header().Set("Cache-Control", "no-cache")
		if versions := r.URL.Query()["v"]; path != "/index.html" && len(versions) == 1 && versions[0] == asset.version {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		w.Header().Set("ETag", tag)
		w.Header().Set("Content-Type", asset.contentType)
		for _, candidate := range strings.Split(r.Header.Get("If-None-Match"), ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || strings.TrimPrefix(candidate, "W/") == tag {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		if r.Method == http.MethodHead {
			return
		}
		_, _ = w.Write(body)
	})
}

func selectGzip(header string) (compressed, acceptable bool) {
	qualities := make(map[string]float64)
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		coding := strings.ToLower(strings.TrimSpace(parts[0]))
		if coding == "" {
			continue
		}
		quality := 1.0
		for _, parameter := range parts[1:] {
			key, value, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if strings.EqualFold(strings.TrimSpace(key), "q") {
				q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
				if !ok || err != nil || math.IsNaN(q) || q < 0 || q > 1 {
					quality = 0
				} else {
					quality = q
				}
			}
		}
		qualities[coding] = quality
	}
	gzipQ, exists := qualities["gzip"]
	if !exists {
		gzipQ = qualities["*"]
	}
	identityQ, explicitIdentity := qualities["identity"]
	if !explicitIdentity {
		identityQ = 1
		if wildcard, exists := qualities["*"]; exists && wildcard == 0 {
			identityQ = 0
		}
	}
	// Prefer compression when identity has no explicit preference, but never
	// override a q=0 exclusion (including an explicit gzip exclusion over '*').
	return gzipQ > 0 && (!explicitIdentity || gzipQ >= identityQ), gzipQ > 0 || identityQ > 0
}
