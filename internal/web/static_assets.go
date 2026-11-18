package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Harness 5719020 packages/host/webserver/src/index.ts: compressed text is
// negotiated separately from streams and ranges. Only embedded public assets
// use this cache; API responses, SSE and evidence retain their own policies.
var staticHandler = sync.OnceValue(func() http.Handler {
	files, _ := fs.Sub(static, "static")
	fallback := http.FileServer(http.FS(files))
	type asset struct {
		raw, compressed       []byte
		rawTag, compressedTag string
	}
	assets := make(map[string]asset)
	entries, _ := fs.ReadDir(files, ".")
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		raw, err := fs.ReadFile(files, entry.Name())
		if err != nil {
			panic(err)
		}
		a := asset{raw: raw, rawTag: fmt.Sprintf(`"%x"`, sha256.Sum256(raw))}
		if len(raw) > 0 {
			var buf bytes.Buffer
			writer, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			_, _ = writer.Write(raw)
			_ = writer.Close()
			a.compressed = buf.Bytes()
			a.compressedTag = fmt.Sprintf(`"%x"`, sha256.Sum256(a.compressed))
		}
		assets[entry.Name()] = a
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		a, ok := assets[name]
		if !ok || r.URL.Path == "/index.html" {
			fallback.ServeHTTP(w, r)
			return
		}
		// Unversioned ES modules must revalidate on every reuse after a release.
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("Vary", "Accept-Encoding")
		w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
		body, tag := a.raw, a.rawTag
		gzipOK, identityOK := acceptedEncodings(r.Header.Get("Accept-Encoding"))
		if len(a.compressed) > 0 && r.Header.Get("Range") == "" && gzipOK {
			body, tag = a.compressed, a.compressedTag
			w.Header().Set("Content-Encoding", "gzip")
		} else if !identityOK {
			http.Error(w, "no acceptable asset encoding", http.StatusNotAcceptable)
			return
		}
		w.Header().Set("ETag", tag)
		http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(body))
	})
})

func acceptedEncodings(header string) (gzipOK, identityOK bool) {
	quality := make(map[string]float64)
	for _, item := range strings.Split(header, ",") {
		parts := strings.Split(item, ";")
		encoding, q := strings.ToLower(strings.TrimSpace(parts[0])), 1.0
		for _, parameter := range parts[1:] {
			key, value, found := strings.Cut(strings.TrimSpace(parameter), "=")
			if found && strings.EqualFold(key, "q") {
				parsed, err := strconv.ParseFloat(value, 64)
				if err != nil || parsed < 0 || parsed > 1 {
					q = 0
				} else {
					q = parsed
				}
			}
		}
		quality[encoding] = q
	}
	q, present := quality["gzip"]
	if !present {
		q = quality["*"]
	}
	identity, explicit := quality["identity"]
	identityOK = !explicit || identity > 0
	if wildcard, set := quality["*"]; !explicit && set && wildcard == 0 {
		identityOK = false
	}
	return q > 0 && (!explicit || q >= identity), identityOK
}
