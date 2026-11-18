package web

import (
	"context"
	"errors"
	"net/http"
)

func (s *Server) probeSearchSource(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	if s.closed || s.configuring {
		s.mu.Unlock()
		writeError(w, 409, errors.New("service configuration is busy or stopping"))
		return
	}
	a := s.a
	s.configuring = true
	s.wg.Add(1)
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.configuring = false; s.mu.Unlock(); s.wg.Done() }()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	result, err := a.ProbeSearchSource(ctx, r.PathValue("source"))
	if err != nil {
		writeError(w, 400, err)
		return
	}
	writeJSON(w, 200, result)
}
