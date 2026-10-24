package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ethandong16/due-radar/web"
)

func staticRequest(h http.Handler, method, path, encoding, etag string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("Accept-Encoding", encoding)
	if etag != "" {
		r.Header.Set("If-None-Match", etag)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestStaticTransferMeasurement(t *testing.T) {
	s, _ := testApp(t)
	h := s.Handler(web.Files)
	total, repeat := 0, 0
	for _, path := range []string{"/", "/theme.js", "/style.css", "/tasks.js", "/app.js"} {
		w := staticRequest(h, "GET", path, "gzip", "")
		r := staticRequest(h, "GET", path, "gzip", w.Header().Get("ETag"))
		total += w.Body.Len()
		repeat += r.Body.Len()
		t.Logf("path=%s status=%d bytes=%d encoding=%q cache=%q repeat_status=%d repeat_bytes=%d", path, w.Code, w.Body.Len(), w.Header().Get("Content-Encoding"), w.Header().Get("Cache-Control"), r.Code, r.Body.Len())
	}
	t.Logf("TOTAL first_bytes=%d conditional_repeat_bytes=%d", total, repeat)
}

func TestStaticGzipNegotiation(t *testing.T) {
	s, _ := testApp(t)
	body := []byte(strings.Repeat("console.log('public asset');\n", 200))
	h := s.Handler(fstest.MapFS{"app.js": {Data: body}, "image.png": {Data: body}})
	for _, tc := range []struct {
		accept string
		gzip   bool
		status int
	}{
		{"", false, 200}, {"identity", false, 200}, {"br, gzip", true, 200},
		{"gzip;q=0", false, 200}, {"*;q=1, gzip;q=0", false, 200},
		{"gzip;q=0.8, identity;q=0.2", true, 200}, {"gzip;q=0.2, identity;q=0.8", false, 200},
		{"*;q=0.5", true, 200}, {"gzip;q=bogus", false, 200},
		{"gzip;q=0, identity;q=0", false, 406}, {"*;q=0", false, 406},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			w := staticRequest(h, "GET", "/app.js", tc.accept, "")
			if w.Code != tc.status {
				t.Fatalf("status=%d want=%d", w.Code, tc.status)
			}
			if w.Code != 200 {
				return
			}
			if got := w.Header().Get("Content-Encoding") == "gzip"; got != tc.gzip {
				t.Fatalf("gzip=%v want=%v", got, tc.gzip)
			}
			if !strings.Contains(w.Header().Get("Vary"), "Accept-Encoding") {
				t.Error("missing Vary: Accept-Encoding")
			}
			if !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
				t.Errorf("type=%q", w.Header().Get("Content-Type"))
			}
			if w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
				t.Error("wrong Content-Length")
			}
			decoded := w.Body.Bytes()
			if tc.gzip {
				zr, err := gzip.NewReader(bytes.NewReader(decoded))
				if err != nil {
					t.Fatal(err)
				}
				decoded, err = io.ReadAll(zr)
				if err != nil {
					t.Fatal(err)
				}
				zr.Close()
				if w.Body.Len() >= len(body) {
					t.Error("gzip did not reduce bytes")
				}
			}
			if !bytes.Equal(decoded, body) {
				t.Error("representation changed content")
			}
			head := staticRequest(h, "HEAD", "/app.js", tc.accept, "")
			if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != w.Header().Get("Content-Length") || head.Header().Get("Content-Encoding") != w.Header().Get("Content-Encoding") {
				t.Fatalf("invalid HEAD: %d %v", head.Code, head.Header())
			}
		})
	}
	w := staticRequest(h, "GET", "/image.png", "gzip", "")
	if w.Header().Get("Content-Encoding") != "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("non-text fallback compressed/cached")
	}
}

func TestSensitiveResponsesStayUncompressedAndUncached(t *testing.T) {
	_, h := testApp(t)
	cookie := loginCookie(t, h)
	for _, authenticated := range []bool{false, true} {
		for _, path := range []string{"/api/session", "/api/tasks", "/api/accounts", "/api/settings", "/api/bark", "/api/events", "/api/status", "/api/missing"} {
			r := httptest.NewRequest("GET", path+"?v=current", nil)
			r.Header.Set("Accept-Encoding", "gzip")
			r.Header.Set("If-None-Match", "*")
			if authenticated {
				r.AddCookie(cookie)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Encoding") != "" || w.Header().Get("ETag") != "" || w.Code == 304 {
				t.Fatalf("sensitive response: %s %d %v", path, w.Code, w.Header())
			}
		}
	}
}

func TestStaticVersionCacheAndDeploymentUpdate(t *testing.T) {
	s, _ := testApp(t)
	oldBody := []byte("console.log('old deployment');")
	newBody := []byte("console.log('new deployment');")
	page := []byte(`<script src="/app.js" defer></script><link href="/style.css" rel="stylesheet">`)
	files := fstest.MapFS{"index.html": {Data: page}, "app.js": {Data: oldBody}, "style.css": {Data: []byte("body{color:green}")}}
	h := s.Handler(files)
	versionURL := func(body []byte) string { return fmt.Sprintf("/app.js?v=%x", sha256.Sum256(body)) }
	oldURL, newURL := versionURL(oldBody), versionURL(newBody)
	oldHTML := staticRequest(h, "GET", "/", "identity", "")
	if !strings.Contains(oldHTML.Body.String(), oldURL) {
		t.Fatalf("HTML does not reference content version: %s", oldHTML.Body.String())
	}
	if oldHTML.Header().Get("Cache-Control") != "no-cache" {
		t.Fatal("HTML must revalidate")
	}
	for _, path := range []string{oldURL, "/app.js", newURL, oldURL + "&v=wrong"} {
		var tags []string
		for _, encoding := range []string{"identity", "gzip"} {
			w := staticRequest(h, "GET", path, encoding, "")
			cache := "no-cache"
			if path == oldURL {
				cache = "public, max-age=31536000, immutable"
			}
			if w.Header().Get("Cache-Control") != cache {
				t.Errorf("%s cache=%q want=%q", path, w.Header().Get("Cache-Control"), cache)
			}
			tag := w.Header().Get("ETag")
			if tag == "" {
				t.Fatal("missing ETag")
			}
			tags = append(tags, tag)
			for _, validator := range []string{tag, "W/" + tag, `"unrelated", ` + tag, "*"} {
				for _, method := range []string{"GET", "HEAD"} {
					r := staticRequest(h, method, path, encoding, validator)
					if r.Code != 304 || r.Body.Len() != 0 || r.Header().Get("ETag") != tag || r.Header().Get("Vary") != "Accept-Encoding" || r.Header().Get("Cache-Control") != cache {
						t.Fatalf("invalid conditional response: %s %d %v", path, r.Code, r.Header())
					}
					if length := r.Header().Get("Content-Length"); length != "" && length != strconv.Itoa(w.Body.Len()) {
						t.Fatal("304 length does not describe selected representation")
					}
				}
			}
		}
		if tags[0] == tags[1] {
			t.Fatal("strong ETag shared by gzip and identity")
		}
		if w := staticRequest(h, "GET", path, "gzip", tags[0]); w.Code != 200 {
			t.Fatal("identity validator incorrectly matched gzip")
		}
	}
	files["app.js"] = &fstest.MapFile{Data: newBody}
	next := s.Handler(files)
	newHTML := staticRequest(next, "GET", "/", "identity", oldHTML.Header().Get("ETag"))
	if newHTML.Code != 200 || !strings.Contains(newHTML.Body.String(), newURL) || strings.Contains(newHTML.Body.String(), oldURL) {
		t.Fatal("HTML did not switch content version on deployment")
	}
	for _, path := range []string{oldURL, "/app.js"} {
		previous := staticRequest(h, "GET", path, "identity", "")
		w := staticRequest(next, "GET", path, "identity", previous.Header().Get("ETag"))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), newBody) || w.Header().Get("Cache-Control") != "no-cache" {
			t.Fatal("stale URL did not revalidate to new content")
		}
	}
	if w := staticRequest(next, "GET", newURL, "gzip", ""); !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Fatal("current version is not immutable")
	}
	if w := staticRequest(next, "GET", "/", "identity", newHTML.Header().Get("ETag")); w.Code != 304 {
		t.Fatal("unchanged HTML must revalidate with 304")
	}
}
