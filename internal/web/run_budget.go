package web

import (
	"errors"
	"net/http"

	"trace/internal/agent"
)

func (s *Server) putRunBudget(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MaxModelRequests  *int   `json:"max_model_requests"`
		MaxActions        *int   `json:"max_actions"`
		MaxSeconds        *int64 `json:"max_seconds"`
		CheckpointSeconds *int64 `json:"checkpoint_seconds,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.MaxModelRequests == nil || in.MaxActions == nil || in.MaxSeconds == nil {
		writeError(w, 400, errors.New("all three budget limits are required; 0 means unlimited"))
		return
	}
	budget := agent.Budget{MaxModelRequests: *in.MaxModelRequests, MaxActions: *in.MaxActions, MaxSeconds: *in.MaxSeconds}
	if in.CheckpointSeconds != nil {
		budget.CheckpointSeconds = *in.CheckpointSeconds
	}
	if err := agent.ValidateBudget(budget); err != nil {
		writeError(w, 400, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		writeError(w, 503, errors.New("server is stopping"))
		return
	}
	if s.configuring || s.active[r.PathValue("id")] {
		writeError(w, 409, errors.New("pause the run and wait for configuration changes before changing its budget"))
		return
	}
	snap, err := s.a.SetRunBudget(r.PathValue("id"), budget)
	respond(w, snap, err)
}

func (s *Server) putTreeBudget(w http.ResponseWriter, r *http.Request) {
	var in agent.TreeBudget
	if !decode(w, r, &in) {
		return
	}
	if err := agent.ValidateTreeBudget(in); err != nil {
		writeError(w, 400, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		writeError(w, 503, errors.New("server is stopping"))
		return
	}
	if s.configuring || s.active[r.PathValue("id")] {
		writeError(w, 409, errors.New("pause root before changing tree budget"))
		return
	}
	snap, err := s.a.SetTreeBudget(r.PathValue("id"), in)
	respond(w, snap, err)
}
