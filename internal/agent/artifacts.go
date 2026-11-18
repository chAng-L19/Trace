package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

const artifactThreshold = 16 << 10

type ArtifactRef struct {
	Hash     string           `json:"hash"`
	Kind     string           `json:"kind"`
	Bytes    int64            `json:"bytes"`
	Name     string           `json:"name,omitempty"`
	Purpose  string           `json:"purpose,omitempty"`
	MIMEType string           `json:"mime_type,omitempty"`
	Preview  *ArtifactPreview `json:"preview,omitempty"`
}

// Preview is display-only metadata. All proof and downloads still read the
// integrity-checked CAS bytes; preview text is never reconstructed as output.
type ArtifactPreview struct {
	Path      string              `json:"path"`
	Lines     int64               `json:"lines"`
	Truncated bool                `json:"truncated"`
	Head      ArtifactPreviewPart `json:"head"`
	Tail      ArtifactPreviewPart `json:"tail"`
}
type ArtifactPreviewPart struct {
	Offset    int64  `json:"offset"`
	EndOffset int64  `json:"end_offset"`
	Text      string `json:"text"`
}

const artifactPreviewBytes = 2048

// pi output-accumulator.ts (MIT, e65b3b88) inspired bounded head/tail display.
// Trace writes the complete byte stream once to its existing CAS.
type outputPreviewWriter struct {
	head, tail   []byte
	bytes, lines int64
	last         byte
}

func (w *outputPreviewWriter) Write(p []byte) (int, error) {
	n := len(p)
	w.bytes += int64(n)
	w.lines += int64(bytes.Count(p, []byte{'\n'}))
	if n > 0 {
		w.last = p[n-1]
	}
	if len(w.head) < artifactPreviewBytes {
		w.head = append(w.head, p[:min(n, artifactPreviewBytes-len(w.head))]...)
	}
	if n >= artifactPreviewBytes {
		w.tail = append(w.tail[:0], p[n-artifactPreviewBytes:]...)
	} else {
		if excess := len(w.tail) + n - artifactPreviewBytes; excess > 0 {
			copy(w.tail, w.tail[excess:])
			w.tail = w.tail[:len(w.tail)-excess]
		}
		w.tail = append(w.tail, p...)
	}
	return n, nil
}
func previewPart(data []byte, offset int64, trimStart, trimEnd bool) ArtifactPreviewPart {
	if trimStart {
		for len(data) > 0 && !utf8.RuneStart(data[0]) {
			data, offset = data[1:], offset+1
		}
	}
	if trimEnd && len(data) > 0 {
		start := len(data) - 1
		for start > 0 && !utf8.RuneStart(data[start]) {
			start--
		}
		if !utf8.FullRune(data[start:]) {
			data = data[:start]
		}
	}
	return ArtifactPreviewPart{Offset: offset, EndOffset: offset + int64(len(data)), Text: strings.ToValidUTF8(string(data), "?")}
}
func (s *Store) putOutputArtifact(kind string, reader io.Reader) (ArtifactRef, error) {
	w := &outputPreviewWriter{}
	ref, err := s.PutArtifactReader(kind, io.TeeReader(reader, w))
	if err != nil {
		return ref, err
	}
	lines := w.lines
	if w.bytes > 0 && w.last != '\n' {
		lines++
	}
	head := previewPart(w.head, 0, false, w.bytes > int64(len(w.head)))
	tail := previewPart(w.tail, w.bytes-int64(len(w.tail)), w.bytes > int64(len(w.tail)), false)
	if tail.Offset < head.EndOffset {
		tail = previewPart(w.tail[head.EndOffset-(w.bytes-int64(len(w.tail))):], head.EndOffset, true, false)
	}
	path, err := filepath.Abs(filepath.Join(s.root, "artifacts", ref.Hash))
	if err != nil {
		return ref, err
	}
	ref.Preview = &ArtifactPreview{Path: path, Lines: lines, Truncated: head.EndOffset < tail.Offset, Head: head, Tail: tail}
	return ref, nil
}

type ArtifactSlice struct {
	ArtifactRef
	DataBase64 string `json:"data_base64"`
	NextOffset int64  `json:"next_offset"`
	Offset     int64  `json:"offset"`
	Content    string `json:"content"`
	EOF        bool   `json:"eof"`
}

func (s *Store) PutArtifact(kind string, data []byte) (ArtifactRef, error) {
	return s.PutArtifactReader(kind, bytes.NewReader(data))
}

func (s *Store) PutArtifactReader(kind string, reader io.Reader) (ArtifactRef, error) {
	var ref ArtifactRef
	dir := filepath.Join(s.root, "artifacts")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return ref, err
	}
	f, err := os.CreateTemp(dir, ".artifact-*")
	if err != nil {
		return ref, err
	}
	defer os.Remove(f.Name())
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, digest), reader)
	if err != nil {
		f.Close()
		return ref, err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return ref, err
	}
	if err = f.Close(); err != nil {
		return ref, err
	}
	ref = ArtifactRef{Hash: hex.EncodeToString(digest.Sum(nil)), Kind: kind, Bytes: n}
	if kind == "search_response" {
		// Publish the durable access marker before the raw bytes. Upstream
		// receipts may contain account data even without a known secret echo.
		if err = os.WriteFile(filepath.Join(dir, ref.Hash+".audit-only"), nil, 0600); err != nil {
			return ref, err
		}
		if err = syncStoreDirectory(dir); err != nil {
			return ref, err
		}
	}
	path := filepath.Join(dir, ref.Hash)
	if err = os.Rename(f.Name(), path); err != nil {
		existing, e := s.OpenArtifact(ref.Hash)
		if e != nil {
			return ref, err
		}
		existing.Close()
	}
	return ref, nil
}

// The marker is monotonic across configuration changes, copies and restarts.
// It is CAS access metadata, not evidence; audit downloads keep using raw CAS.
func (s *Store) checkModelArtifactRead(hash string) error {
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("invalid artifact hash")
	}
	_, err = os.Stat(filepath.Join(s.root, "artifacts", hash+".audit-only"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("raw search receipt is audit-only; use the normalized source_search items and status")
}

type artifactStamp struct {
	size     int64
	modified time.Time
}

// OpenArtifact returns an integrity-checked raw stream positioned at byte zero.
// Downloads and evidence checks always verify integrity. Paged previews cache
// unchanged file metadata so they do not hash the full blob for each page.
func (s *Store) OpenArtifact(hash string) (*os.File, error) { return s.openArtifact(hash, false) }

func (s *Store) openArtifact(hash string, cachedPreview bool) (*os.File, error) {
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != 32 {
		return nil, fmt.Errorf("invalid artifact hash")
	}
	f, err := os.Open(filepath.Join(s.root, "artifacts", hash))
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	stamp := artifactStamp{info.Size(), info.ModTime()}
	if cached, ok := s.artifactVerified.Load(hash); !cachedPreview || !ok || cached != stamp {
		digest := sha256.New()
		if _, err = io.Copy(digest, f); err != nil {
			f.Close()
			return nil, err
		}
		if hex.EncodeToString(digest.Sum(nil)) != hash {
			f.Close()
			return nil, fmt.Errorf("artifact hash mismatch")
		}
		s.artifactVerified.Store(hash, stamp)
	}
	_, err = f.Seek(0, io.SeekStart)
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (s *Store) ReadArtifact(hash string, offset, limit int64) (ArtifactSlice, error) {
	var out ArtifactSlice
	if offset < 0 || limit < 0 {
		return out, fmt.Errorf("negative range")
	}
	if limit == 0 || limit > 64<<10 {
		limit = 64 << 10
	}
	f, err := s.openArtifact(hash, true)
	if err != nil {
		return out, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return out, err
	}
	if offset > info.Size() {
		return out, fmt.Errorf("offset exceeds artifact length")
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return out, err
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return out, err
	}
	out = ArtifactSlice{ArtifactRef: ArtifactRef{Hash: hash, Bytes: info.Size()}, Offset: offset, DataBase64: base64.StdEncoding.EncodeToString(data), NextOffset: offset + int64(len(data)), Content: string(data), EOF: offset+int64(len(data)) == info.Size()}
	return out, nil
}

func (a *Agent) archiveOutput(r *ToolResult) error {
	// The action owner removes captures only after the result transaction commits.
	// CAS or snapshot failure must retain the exact output for manual recovery.
	for _, item := range []struct{ kind, path string }{{"stdout", r.StdoutPath}, {"stderr", r.StderrPath}} {
		if item.path == "" {
			continue
		}
		f, err := os.Open(item.path)
		if err != nil {
			return err
		}
		info, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if info.Size() < artifactThreshold {
			data, readErr := io.ReadAll(f)
			err = readErr
			if item.kind == "stdout" {
				r.Stdout = string(data)
			} else {
				r.Stderr = string(data)
			}
		} else {
			var ref ArtifactRef
			ref, err = a.store.putOutputArtifact(item.kind, f)
			if err == nil {
				r.Artifacts = append(r.Artifacts, ref)
				if item.kind == "stdout" {
					r.Stdout = ""
				} else {
					r.Stderr = ""
				}
			}
		}
		f.Close()
		if err != nil {
			return err
		}
	}
	r.StdoutPath = ""
	r.StderrPath = ""

	for _, item := range []struct{ kind, data string }{{"stdout", r.Stdout}, {"stderr", r.Stderr}, {"mcp_result", string(r.MCPResult)}} {
		if len(item.data) < artifactThreshold && utf8.ValidString(item.data) {
			continue
		}
		ref, err := a.store.putOutputArtifact(item.kind, strings.NewReader(item.data))
		if err != nil {
			return err
		}
		r.Artifacts = append(r.Artifacts, ref)
		switch item.kind {
		case "stdout":
			r.Stdout = ""
		case "stderr":
			r.Stderr = ""
		case "mcp_result":
			r.MCPResult = nil
		}
	}
	return nil
}

// artifactOutput preserves legacy inline results and reconstructs new CAS results.
func artifactOutput(root string, result ToolResult, kind string) (string, error) {
	for _, ref := range result.Artifacts {
		if ref.Kind != kind {
			continue
		}
		store := Store{root: root}
		f, err := store.OpenArtifact(ref.Hash)
		if err != nil {
			return "", err
		}
		defer f.Close()
		data, err := io.ReadAll(f)
		if err != nil {
			return "", err
		}
		if int64(len(data)) != ref.Bytes {
			return "", fmt.Errorf("artifact byte length mismatch")
		}
		return string(data), nil
	}
	if kind == "stdout" {
		return result.Stdout, nil
	}
	return result.Stderr, nil
}

func verifyArtifacts(root string, r *ToolResult) bool {
	store := Store{root: root}
	for _, ref := range r.Artifacts {
		f, err := store.OpenArtifact(ref.Hash)
		if err != nil {
			return false
		}
		stat, err := f.Stat()
		f.Close()
		if err != nil || stat.Size() != ref.Bytes {
			return false
		}
	}
	return true
}
