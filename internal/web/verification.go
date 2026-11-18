package web

import (
	"context"
	"errors"
	"net/http"
	"trace/internal/agent"
)

// An operator review owns the same lifecycle boundary as an executing run:
// configuration replacement is blocked and server shutdown cancels its model.
func (s *Server) beginVerification(ctx context.Context, id string) (*agent.Agent, context.Context, func(), error) {
	s.mu.Lock()
	if s.closed || s.configuring || s.active[id] {
		s.mu.Unlock()
		return nil, nil, nil, errors.New("run or configuration is active")
	}
	a := s.a
	s.active[id] = true
	s.wg.Add(1)
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	s.mu.Unlock()
	return a, ctx, func() {
		stop()
		cancel()
		s.mu.Lock()
		delete(s.active, id)
		s.mu.Unlock()
		s.wg.Done()
	}, nil
}

func (s *Server) registerVerificationRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/runs/{id}/root-contract", func(w http.ResponseWriter, r *http.Request) {
		var in agent.RootContract
		if !decode(w, r, &in) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			writeError(w, 503, errors.New("server is stopping"))
			return
		}
		if s.configuring || s.active[r.PathValue("id")] {
			writeError(w, 409, errors.New("run or configuration is active"))
			return
		}
		value, err := s.a.SetRootContract(r.PathValue("id"), in)
		respond(w, value, err)
	})
	mux.HandleFunc("PUT /api/runs/{id}/requirements", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Requirements    []agent.VerificationRequirement `json:"requirements"`
			SuccessContains *string                         `json:"success_contains,omitempty"`
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
		v, e := s.a.SetAcceptanceContract(r.PathValue("id"), in.Requirements, in.SuccessContains)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/runs/{id}/checks", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RequirementID string   `json:"requirement_id"`
			EvidenceIDs   []string `json:"evidence_ids"`
		}
		if !decode(w, r, &in) {
			return
		}
		// Independent model review runs outside the configuration mutex, with a
		// run lock so it cannot race another model/tool execution for this run.
		a, ctx, done, e := s.beginVerification(r.Context(), r.PathValue("id"))
		if e != nil {
			writeError(w, 409, e)
			return
		}
		defer done()
		release, e := s.store.AcquireRunLock(r.PathValue("id"))
		if e != nil {
			respond(w, nil, e)
			return
		}
		defer release()
		v, e := a.CheckEvidence(ctx, r.PathValue("id"), in.RequirementID, in.EvidenceIDs)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/runs/{id}/findings", func(w http.ResponseWriter, r *http.Request) {
		var in agent.Finding
		if !decode(w, r, &in) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			writeError(w, 503, errors.New("server is stopping"))
			return
		}
		v, e := s.a.SubmitFinding(r.PathValue("id"), in)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/runs/{id}/findings/{finding}/{operation}", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			EvidenceIDs []string `json:"evidence_ids"`
			Triage      string   `json:"triage"`
		}
		if !decode(w, r, &in) {
			return
		}
		a, ctx, done, e := s.beginVerification(r.Context(), r.PathValue("id"))
		if e != nil {
			writeError(w, 409, e)
			return
		}
		defer done()
		release, e := s.store.AcquireRunLock(r.PathValue("id"))
		if e != nil {
			respond(w, nil, e)
			return
		}
		defer release()
		var v any
		switch r.PathValue("operation") {
		case "review":
			v, e = a.ReviewFinding(ctx, r.PathValue("id"), r.PathValue("finding"))
		case "retest":
			v, e = a.RetestFinding(r.PathValue("id"), r.PathValue("finding"), in.EvidenceIDs)
		case "triage":
			v, e = a.TriageFinding(r.PathValue("id"), r.PathValue("finding"), in.Triage)
		default:
			writeError(w, 404, errors.New("unknown finding operation"))
			return
		}
		respond(w, v, e)
	})
}
