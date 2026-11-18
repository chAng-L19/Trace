package web

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"

	"trace/internal/agent"
)

func messageReceipt(id string, q agent.QueuedMessage) map[string]any {
	status := "queued"
	if !q.ConsumedAt.IsZero() {
		status = "delivered"
	}
	if !q.CancelledAt.IsZero() {
		status = "cancelled"
	}
	return map[string]any{"accepted": true, "id": id, "operation_id": q.OperationID, "message_id": q.ID, "status": status, "revision": q.Revision}
}

func (s *Server) registerSessionRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/runs/{id}/messages", s.messageHistory)
	mux.HandleFunc("GET /api/runs/{id}/actions/{call}", s.getWorkspaceAction)
	mux.HandleFunc("PATCH /api/runs/{id}", s.updateRunMetadata)
	mux.HandleFunc("GET /api/runs/search", s.searchRunMessages)
	mux.HandleFunc("GET /api/runs/{id}/requirements", s.getRunRequirements)
	mux.HandleFunc("GET /api/runs/{id}/operations/{operation}", func(w http.ResponseWriter, r *http.Request) {
		q, err := s.store.MessageOperation(r.PathValue("id"), r.PathValue("operation"))
		respond(w, messageReceipt(r.PathValue("id"), q), err)
	})
	mux.HandleFunc("GET /api/runs/{id}/messages/{message}", func(w http.ResponseWriter, r *http.Request) {
		snap, err := s.store.Load(r.PathValue("id"))
		if err != nil {
			respond(w, nil, err)
			return
		}
		if snap.Run.Deleted {
			respond(w, nil, os.ErrNotExist)
			return
		}
		for i, message := range snap.Messages {
			if message.ID == "" {
				message.ID = fmt.Sprintf("%s-m%06d", snap.Run.ID, i+1)
			}
			if message.ID == r.PathValue("message") {
				respond(w, message, nil)
				return
			}
		}
		respond(w, nil, os.ErrNotExist)
	})
	mux.HandleFunc("PATCH /api/runs/{id}/mailbox/{message}", s.editQueuedMessage)
	mux.HandleFunc("DELETE /api/runs/{id}/mailbox/{message}", s.editQueuedMessage)
	mux.HandleFunc("PUT /api/runs/{id}/mailbox/order", s.orderQueuedMessages)
}

func (s *Server) updateRunMetadata(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title    *string `json:"title"`
		Archived *bool   `json:"archived"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Title == nil && in.Archived == nil {
		writeError(w, 400, errors.New("title or archived is required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		writeError(w, 503, errors.New("server is stopping"))
		return
	}
	snap, err := s.store.UpdateRunMetadata(r.PathValue("id"), in.Title, in.Archived)
	respond(w, snap.Run, err)
}

func (s *Server) searchRunMessages(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if value := r.URL.Query().Get("limit"); value != "" {
		var err error
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 200 {
			writeError(w, 400, errors.New("limit must be 1..200"))
			return
		}
	}
	value, err := s.store.SearchMessages(r.URL.Query().Get("q"), r.URL.Query().Get("include_archived") == "true", limit)
	respond(w, value, err)
}

func (s *Server) getRunRequirements(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.Load(r.PathValue("id"))
	if err == nil && snap.Run.Deleted {
		err = os.ErrNotExist
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	editable := len(snap.Requests) == 0 && len(snap.Actions) == 0 && snap.Run.Status != agent.StatusRunning && snap.Run.Status != agent.StatusWaitingTool
	respond(w, map[string]any{"requirements": snap.Requirements, "success_contains": snap.Run.SuccessContains, "editable": editable}, nil)
}

func (s *Server) editQueuedMessage(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Content  *string `json:"content"`
		Revision *int    `json:"revision"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Revision == nil {
		writeError(w, 400, errors.New("message revision is required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		writeError(w, 503, errors.New("server is stopping"))
		return
	}
	snap, err := s.store.EditQueuedMessage(r.PathValue("id"), r.PathValue("message"), *in.Revision, in.Content, r.Method == "DELETE")
	respond(w, snap, err)
}

func (s *Server) orderQueuedMessages(w http.ResponseWriter, r *http.Request) {
	var in struct {
		MessageIDs []string `json:"message_ids"`
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
	snap, err := s.store.OrderQueuedMessages(r.PathValue("id"), in.MessageIDs)
	respond(w, snap, err)
}
