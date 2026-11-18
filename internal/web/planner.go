package web

import (
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"trace/internal/agent"
)

func (s *Server) registerPlannerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runs/{id}/worker-capacity", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		closed, a := s.closed, s.a
		s.mu.Unlock()
		if closed {
			writeError(w, 503, errors.New("server is stopping"))
			return
		}
		v, err := a.WorkerCapacityContext(r.Context(), r.PathValue("id"))
		respond(w, v, err)
	})
	mux.HandleFunc("POST /api/runs/{id}/plan/dispatch", func(w http.ResponseWriter, r *http.Request) {
		var in struct{}
		if !decode(w, r, &in) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			writeError(w, 503, errors.New("server is stopping"))
			return
		}
		if s.configuring {
			writeError(w, 409, errors.New("integration operation is in progress"))
			return
		}
		v, e := s.a.DispatchFrontier(s.ctx, r.PathValue("id"))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/runs/{id}/plan/capacity", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Capacity int `json:"capacity"`
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
		v, e := s.a.SetPlanCapacity(r.PathValue("id"), in.Capacity)
		respond(w, v, e)
	})
	mux.HandleFunc("PUT /api/runs/{id}/plan", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Mode  string           `json:"mode,omitempty"`
			Tasks []agent.PlanTask `json:"tasks"`
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
		v, e := s.a.UpdatePlan(r.PathValue("id"), in.Tasks, in.Mode)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/runs/{id}/tasks/{task}/{op}", s.taskOperation)
	mux.HandleFunc("POST /api/runs/{id}/workers/{worker}/{op}", s.workerOperation)
	mux.HandleFunc("GET /api/runs/{id}/worker-message-operations/{operation}", func(w http.ResponseWriter, r *http.Request) {
		v, err := s.store.WorkerMessageOperation(r.PathValue("id"), r.PathValue("operation"))
		respond(w, v, err)
	})
	mux.HandleFunc("POST /api/runs/{id}/workers/recover", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			writeError(w, 503, errors.New("server is stopping"))
			return
		}
		v, e := s.a.RecoverWorkers(r.PathValue("id"))
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/runs/{id}/assets", func(w http.ResponseWriter, r *http.Request) {
		var in agent.Asset
		if !decode(w, r, &in) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			writeError(w, 503, errors.New("server is stopping"))
			return
		}
		v, e := s.a.UpsertAsset(r.PathValue("id"), in)
		respond(w, v, e)
	})
	mux.HandleFunc("POST /api/runs/{id}/relations", func(w http.ResponseWriter, r *http.Request) {
		var in agent.AssetRelation
		if !decode(w, r, &in) {
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.closed {
			writeError(w, 503, errors.New("server is stopping"))
			return
		}
		v, e := s.a.RelateAssets(r.PathValue("id"), in)
		respond(w, v, e)
	})
	mux.HandleFunc("GET /api/artifacts/{hash}", s.artifact)
	mux.HandleFunc("GET /api/artifacts/{hash}/download", s.downloadArtifact)
	mux.HandleFunc("GET /api/runs/{id}/coverage", func(w http.ResponseWriter, r *http.Request) {
		snap, e := s.store.Load(r.PathValue("id"))
		if e == nil && snap.Run.Deleted {
			e = os.ErrNotExist
		}
		if e != nil {
			respond(w, nil, e)
			return
		}
		respond(w, agent.Coverage(snap), nil)
	})
	mux.HandleFunc("POST /api/runs/{id}/workers/wait", s.waitWorkers)
}

func (s *Server) taskOperation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		WorkerID          string   `json:"worker_id"`
		LeaseSeconds      int      `json:"lease_seconds"`
		Status            string   `json:"status"`
		EvidenceIDs       []string `json:"evidence_ids"`
		Reason            string   `json:"reason"`
		ReplacementTaskID string   `json:"replacement_task_id"`
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
	id, task := r.PathValue("id"), r.PathValue("task")
	var v any
	var e error
	switch r.PathValue("op") {
	case "dispatch":
		if s.configuring {
			writeError(w, 409, errors.New("integration operation is in progress"))
			return
		}
		v, e = s.a.DispatchTask(s.ctx, id, task)
	case "retry":
		v, e = s.a.RetryTask(id, task)
	case "dispose":
		v, e = s.a.DisposeTask(id, task, in.Status, in.Reason, in.ReplacementTaskID)
	case "claim":
		v, e = s.a.ClaimTask(id, task, in.WorkerID, in.LeaseSeconds)
	case "complete":
		v, e = s.a.CompleteTask(id, task, in.WorkerID, in.Status, in.EvidenceIDs)
	default:
		writeError(w, 404, errors.New("unknown task operation"))
		return
	}
	respond(w, v, e)
}

func (s *Server) workerOperation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OperationID string `json:"operation_id"`
		Content     string `json:"content"`
		Kind        string `json:"kind"`
		ReplyTo     string `json:"reply_to"`
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
	var v any
	var e error
	switch r.PathValue("op") {
	case "message":
		var receipt agent.WorkerMessage
		v, receipt, _, e = s.a.SendWorkerMessageOperation(r.PathValue("id"), "parent", r.PathValue("worker"), in.Content, in.Kind, in.ReplyTo, in.OperationID)
		if e == nil {
			w.Header().Set("X-Worker-Message-ID", receipt.ID)
		}
	case "cancel":
		v, e = s.a.CancelWorker(r.PathValue("id"), r.PathValue("worker"))
	default:
		writeError(w, 404, errors.New("unknown worker operation"))
		return
	}
	respond(w, v, e)
}

func (s *Server) artifact(w http.ResponseWriter, r *http.Request) {
	offset, limit := int64(0), int64(65536)
	for _, item := range []struct {
		name   string
		target *int64
	}{{"offset", &offset}, {"limit", &limit}} {
		if value := r.URL.Query().Get(item.name); value != "" {
			n, e := strconv.ParseInt(value, 10, 64)
			if e != nil || n < 0 {
				writeError(w, 400, errors.New("invalid artifact range"))
				return
			}
			*item.target = n
		}
	}
	v, e := s.store.ReadArtifact(r.PathValue("hash"), offset, limit)
	respond(w, v, e)
}

func (s *Server) downloadArtifact(w http.ResponseWriter, r *http.Request) {
	f, e := s.store.OpenArtifact(r.PathValue("hash"))
	if e != nil {
		respond(w, nil, e)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+r.PathValue("hash")+`.bin"`)
	http.ServeContent(w, r, r.PathValue("hash"), time.Time{}, f)
}

func (s *Server) waitWorkers(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Seconds int `json:"seconds"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Seconds < 0 || in.Seconds > 60 {
		writeError(w, 400, errors.New("seconds must be between 0 and 60"))
		return
	}
	view, err := s.store.Observe(r.PathValue("id"))
	if err == nil && view.Run.Deleted {
		err = os.ErrNotExist
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	s.mu.Lock()
	a := s.a
	s.mu.Unlock()
	v, e := a.WaitWorkers(r.Context(), r.PathValue("id"), in.Seconds)
	if e == nil && v.Run.Deleted {
		e = os.ErrNotExist
	}
	respond(w, v, e)
}
