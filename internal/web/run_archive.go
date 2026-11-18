package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"trace/internal/agent"
)

func (s *Server) recordToolObservation(w http.ResponseWriter, r *http.Request) {
	var in struct {
		RequestID string `json:"request_id"`
		CallID    string `json:"call_id"`
		Content   string `json:"content"`
	}
	if !decode(w, r, &in) {
		return
	}
	id := r.PathValue("id")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.configuring || s.active[id] {
		writeError(w, 409, errors.New("请等待当前执行结束后再提交核对结果"))
		return
	}
	_, err := s.a.RecordToolObservation(id, in.RequestID, in.CallID, in.Content)
	respond(w, map[string]any{"recorded": err == nil, "id": id, "started": false}, err)
}

func (s *Server) deleteRun(w http.ResponseWriter, r *http.Request) {
	options := struct {
		RetainEvidence bool   `json:"retain_evidence"`
		Mode           string `json:"mode"`
	}{RetainEvidence: true}
	decoder := json.NewDecoder(io.LimitReader(r.Body, 1024))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&options); err != nil && err != io.EOF {
		writeError(w, 400, err)
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		writeError(w, 400, errors.New("request must contain one JSON value"))
		return
	}
	id := r.PathValue("id")
	if options.Mode != "" && options.Mode != "hide" && options.Mode != "permanent" {
		writeError(w, 400, errors.New("mode must be hide or permanent"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.configuring {
		writeError(w, 409, errors.New("服务正在更新，请稍后再试"))
		return
	}
	if s.active[id] {
		writeError(w, 409, errors.New("请先暂停会话，等待执行结束后再删除"))
		return
	}
	var result agent.EvidenceDeleteResult
	var err error
	if options.Mode == "permanent" {
		result, err = s.store.PurgeRun(id)
	} else {
		if options.Mode == "hide" {
			options.RetainEvidence = true
		}
		result, err = s.store.DeleteWithEvidence(id, options.RetainEvidence)
	}
	respond(w, map[string]any{"deleted": result.Deleted, "id": id, "permanent": result.Permanent, "run_ids": result.RunIDs, "retain_evidence": options.Mode != "permanent" && options.RetainEvidence, "artifacts_deleted": result.ArtifactsDeleted, "artifacts_retained": result.ArtifactsRetained, "cleanup_warning": result.CleanupWarning}, err)
}

func (s *Server) listEvidence(w http.ResponseWriter, r *http.Request) {
	var entries []agent.EvidenceCatalogEntry
	var err error
	if r.URL.Query().Has("run") {
		entries, err = s.store.ListRunEvidence(r.URL.Query().Get("run"))
	} else {
		entries, err = s.store.ListEvidence()
	}
	respond(w, entries, err)
}

func (s *Server) getEvidence(w http.ResponseWriter, r *http.Request) {
	entry, err := s.store.GetEvidence(r.PathValue("run"), r.PathValue("evidence"))
	respond(w, entry, err)
}

func (s *Server) deleteEvidence(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("run")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.configuring || s.active[id] {
		writeError(w, 409, errors.New("会话正在执行或服务正在更新，请等待结束后再删除证据"))
		return
	}
	result, err := s.store.DeleteEvidence(id, r.PathValue("evidence"))
	respond(w, map[string]any{"deleted": result.Deleted, "run_id": id, "evidence_id": r.PathValue("evidence"), "artifacts_deleted": result.ArtifactsDeleted, "artifacts_retained": result.ArtifactsRetained, "cleanup_warning": result.CleanupWarning}, err)
}
