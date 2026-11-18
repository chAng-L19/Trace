package web

import (
	"errors"
	"net/http"
)

func (s *Server) selectTools(w http.ResponseWriter, r *http.Request) {
	var in struct {
		IDs     []string `json:"ids"`
		Replace bool     `json:"replace"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		writeError(w, 503, errors.New("server is stopping"))
		return
	}
	snap, err := s.a.SelectTools(r.PathValue("id"), in.IDs, in.Replace)
	respond(w, snap, err)
}
