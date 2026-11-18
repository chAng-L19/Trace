package agent

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

type ArtifactRegistration struct {
	Path    string `json:"path"`
	Name    string `json:"name,omitempty"`
	Purpose string `json:"purpose,omitempty"`
}

// RegisterArtifact copies a real deliverable into the existing CAS. Its caller
// records the reference in the normal action/result ledger; file registration
// proves captured bytes, not the correctness of the file's conclusions.
func (s *Store) RegisterArtifact(in ArtifactRegistration) (ArtifactRef, error) {
	if strings.TrimSpace(in.Path) == "" {
		return ArtifactRef{}, fmt.Errorf("path is required")
	}
	path, err := filepath.Abs(in.Path)
	if err != nil {
		return ArtifactRef{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return ArtifactRef{}, err
	}
	if !info.Mode().IsRegular() {
		return ArtifactRef{}, fmt.Errorf("artifact path must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return ArtifactRef{}, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return ArtifactRef{}, err
	}
	if !info.Mode().IsRegular() {
		return ArtifactRef{}, fmt.Errorf("artifact path must be a regular file")
	}
	reader := bufio.NewReader(f)
	header, err := reader.Peek(512)
	if err != nil && err != io.EOF {
		return ArtifactRef{}, err
	}
	mimeType := http.DetectContentType(header)
	ref, err := s.PutArtifactReader("file", reader)
	if err != nil {
		return ArtifactRef{}, err
	}
	ref.Name = strings.TrimSpace(in.Name)
	if ref.Name == "" {
		ref.Name = filepath.Base(path)
	}
	ref.Purpose = in.Purpose
	ref.MIMEType = mimeType
	return ref, nil
}
