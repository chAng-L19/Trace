package web

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"trace/internal/agent"
)

func TestInputUploadCreateDownloadAndSeparation(t *testing.T) {
	s, a := fixture(t, agent.MockProvider{})
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	file, err := form.CreateFormFile("file", "original.txt")
	if err != nil {
		t.Fatal(err)
	}
	file.Write([]byte("original-input"))
	form.Close()
	req := httptest.NewRequest("POST", "/api/inputs", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var input agent.InputAttachment
	if err = json.Unmarshal(w.Body.Bytes(), &input); err != nil {
		t.Fatal(err)
	}
	if input.Source != "user_upload" || input.Name != "original.txt" {
		t.Fatalf("identity: %+v", input)
	}
	req = httptest.NewRequest("POST", "/api/runs", strings.NewReader(`{"goal":"inspect","attachments":["`+input.ID+`"]}`))
	req.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != 201 {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	var snap agent.Snapshot
	json.Unmarshal(w.Body.Bytes(), &snap)
	if len(snap.Messages) != 1 || snap.Messages[0].Attachments[0].Hash != input.Hash || len(snap.Evidence) != 0 {
		t.Fatal("input not bound independently")
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/inputs/"+input.ID, nil))
	if w.Code != 200 || w.Body.String() != "original-input" || !strings.Contains(w.Header().Get("Content-Disposition"), "original.txt") {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}
	artifact, err := a.Store().PutArtifact("stdout", []byte("tool output"))
	if err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest("GET", "/api/inputs/"+artifact.Hash, nil))
	if w.Code == http.StatusOK {
		t.Fatal("tool artifact accepted as user input")
	}
	req = httptest.NewRequest("POST", "/api/inputs", strings.NewReader("wrong"))
	req.Header.Set("Content-Type", "text/plain")
	w = httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("unbounded raw upload accepted: %d", w.Code)
	}
}
