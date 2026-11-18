package web

import (
	"fmt"
	"mime"
	"net/http"
	"trace/internal/agent"
)

func (s *Server) uploadInput(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, agent.MaxInputAttachmentBytes+(1<<20))
	if err := r.ParseMultipartForm(agent.MaxInputAttachmentBytes); err != nil {
		writeError(w, 400, err)
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, header, err := r.FormFile("file")
	if err != nil {
		writeError(w, 400, err)
		return
	}
	defer f.Close()
	input, err := s.store.PutInputAttachment(header.Filename, f)
	if err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 201, input)
}

func (s *Server) inputIDs(ids []string) ([]agent.InputAttachment, error) {
	inputs := make([]agent.InputAttachment, 0, len(ids))
	for _, id := range ids {
		input, err := s.store.InputAttachment(id)
		if err != nil {
			return nil, err
		}
		f, err := s.store.OpenArtifact(input.Hash)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		f.Close()
		if err != nil || info.Size() != input.Bytes {
			return nil, fmt.Errorf("input byte length mismatch")
		}
		inputs = append(inputs, input)
	}
	return inputs, nil
}

// Input previews resolve upload identities; execution artifacts use separate routes.
func (s *Server) downloadInput(w http.ResponseWriter, r *http.Request) {
	input, err := s.store.InputAttachment(r.PathValue("input"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	f, err := s.store.OpenArtifact(input.Hash)
	if err != nil {
		respond(w, nil, err)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() != input.Bytes {
		writeError(w, 409, fmt.Errorf("input byte length mismatch"))
		return
	}
	disposition := "attachment"
	if r.URL.Query().Get("preview") == "1" {
		switch input.MIMEType {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			disposition = "inline"
		}
	}
	w.Header().Set("Content-Type", input.MIMEType)
	w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": input.Name}))
	http.ServeContent(w, r, input.Name, info.ModTime(), f)
}
