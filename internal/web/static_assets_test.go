package web

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStaticAssetCompressionRevalidationAndIsolation(t *testing.T) {
	s, _ := fixture(t, nil)
	h := s.Handler()
	get := func(method, path string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	raw := get("GET", "/app.js", nil)
	compressed := get("GET", "/app.js", map[string]string{"Accept-Encoding": "br, gzip"})
	if raw.Code != 200 || compressed.Code != 200 || compressed.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("static response: %d / %d %v", raw.Code, compressed.Code, compressed.Header())
	}
	z, err := gzip.NewReader(bytes.NewReader(compressed.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	_ = z.Close()
	if !bytes.Equal(decoded, raw.Body.Bytes()) || compressed.Body.Len() >= raw.Body.Len() {
		t.Fatal("compressed asset must retain exact source and reduce bytes")
	}
	if compressed.Header().Get("Vary") != "Accept-Encoding" || compressed.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatal("asset must negotiate and revalidate")
	}
	if raw.Header().Get("ETag") == "" || raw.Header().Get("ETag") == compressed.Header().Get("ETag") {
		t.Fatal("representations need distinct content validators")
	}
	for _, encoding := range []string{"gzip", "identity"} {
		original := raw
		if encoding == "gzip" {
			original = compressed
		}
		cached := get("GET", "/app.js", map[string]string{"Accept-Encoding": encoding, "If-None-Match": original.Header().Get("ETag")})
		if cached.Code != 304 || cached.Body.Len() != 0 {
			t.Fatalf("conditional %s: %d / %d bytes", encoding, cached.Code, cached.Body.Len())
		}
	}
	changed := get("GET", "/app.js", map[string]string{"If-None-Match": `"previous-release"`})
	if changed.Code != 200 || !bytes.Equal(changed.Body.Bytes(), raw.Body.Bytes()) {
		t.Fatal("stale validator must receive current source")
	}
	head := get("HEAD", "/app.js", map[string]string{"Accept-Encoding": "gzip"})
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("ETag") != compressed.Header().Get("ETag") || head.Header().Get("Content-Encoding") != "gzip" {
		t.Fatal("HEAD must identify the selected compressed representation without a body")
	}
	ranged := get("GET", "/app.js", map[string]string{"Accept-Encoding": "gzip", "Range": "bytes=0-9"})
	if ranged.Code != 206 || ranged.Header().Get("Content-Encoding") != "" || !bytes.Equal(ranged.Body.Bytes(), raw.Body.Bytes()[:10]) {
		t.Fatal("ranges must retain identity byte offsets")
	}
	api := get("GET", "/api/runs", map[string]string{"Accept-Encoding": "gzip"})
	if api.Code != 200 || api.Header().Get("Content-Encoding") != "" || api.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("API must remain outside static caching/compression")
	}
	for _, path := range []string{"/app.js", "/mark.svg"} {
		if w := get("GET", path, map[string]string{"Accept-Encoding": "gzip;q=0,identity;q=0"}); w.Code != 406 {
			t.Errorf("unacceptable encodings for %s: %d", path, w.Code)
		}
		if w := get("GET", path, map[string]string{"Accept-Encoding": "gzip,identity;q=0"}); w.Code != 200 || w.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("gzip-only %s: %d", path, w.Code)
		}
	}
	for _, path := range []string{"/app.js.backup", "/index.html.original", "/index-unified.html", "/style.css"} {
		if w := get("GET", path, nil); w.Code != 404 {
			t.Errorf("legacy asset %s is publicly embedded: %d", path, w.Code)
		}
	}
	index := get("GET", "/", map[string]string{"Accept-Encoding": "gzip"})
	if index.Code != 200 || !strings.Contains(index.Header().Get("Content-Type"), "text/html") || index.Header().Get("ETag") == "" {
		t.Fatal("HTML entry must be revalidated too")
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	conditional, _ := http.NewRequest("GET", ts.URL+"/app.js", nil)
	conditional.Header.Set("If-Match", `"stale"`)
	response, err := ts.Client().Do(conditional)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if _, err := io.ReadAll(response.Body); err != nil || response.StatusCode != 412 {
		t.Fatalf("failed precondition must have a complete HTTP body: status=%d err=%v", response.StatusCode, err)
	}
}

func TestStaticAssetEncodingPreferences(t *testing.T) {
	for _, tc := range []struct {
		header string
		gzip   bool
	}{
		{"", false}, {"br, gzip", true}, {"gzip;q=0", false}, {"*;q=1,gzip;q=0", false},
		{"gzip;q=.5,identity;q=1", false}, {"gzip;q=1,identity;q=.5", true}, {"*;q=.7", true}, {"gzip;q=invalid", false},
	} {
		t.Run(tc.header, func(t *testing.T) {
			if got, _ := acceptedEncodings(tc.header); got != tc.gzip {
				t.Fatalf("%q: %v", tc.header, got)
			}
		})
	}
}
