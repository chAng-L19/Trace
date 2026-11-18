package web

import (
	"net/http"
	"os"
)

func (s *Server) getUsageReport(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	config := s.config.Costs
	s.mu.Unlock()
	state, err := s.store.Observe(r.PathValue("id"))
	if err == nil && state.Run.Deleted {
		err = os.ErrNotExist
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	report, err := s.store.UsageReport(state.Run.ID, config)
	respond(w, report, err)
}
