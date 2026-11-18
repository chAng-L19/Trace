package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"trace/internal/agent"
)

func (s *Server) listEvidenceChains(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Has("limit") {
		limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("limit must be 1..100"))
			return
		}
		page, err := s.store.EvidenceChainPage(r.URL.Query().Get("run"), r.URL.Query().Has("run"), r.URL.Query().Get("query"), r.URL.Query().Get("before"), limit)
		respond(w, page, err)
		return
	}
	var entries []agent.EvidenceChainEntry
	var err error
	if r.URL.Query().Has("run") {
		entries, err = s.store.ListRunEvidenceChains(r.URL.Query().Get("run"))
	} else {
		entries, err = s.store.ListEvidenceChains()
	}
	respond(w, entries, err)
}

func (s *Server) getEvidenceChain(w http.ResponseWriter, r *http.Request) {
	entry, err := s.store.GetEvidenceChain(r.PathValue("run"), r.PathValue("chain"))
	if err == nil && r.URL.Query().Get("hash") != "" && r.URL.Query().Get("hash") != entry.Chain.ContentHash {
		writeError(w, http.StatusConflict, fmt.Errorf("fixed chain hash does not match"))
		return
	}
	respond(w, entry, err)
}

func decodeChainBody(r *http.Request, target any) error {
	d := json.NewDecoder(io.LimitReader(r.Body, 1<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("request must contain one JSON value")
	}
	return nil
}

func (s *Server) createEvidenceChain(w http.ResponseWriter, r *http.Request) {
	var in agent.EvidenceChain
	if err := decodeChainBody(r, &in); err != nil {
		writeError(w, 400, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.configuring {
		writeError(w, 409, errors.New("服务正在更新"))
		return
	}
	entry, err := s.store.CreateEvidenceChain(r.PathValue("id"), in)
	respond(w, entry, err)
}

func (s *Server) referenceEvidenceChain(w http.ResponseWriter, r *http.Request) {
	var ref agent.EvidenceChainReference
	if err := decodeChainBody(r, &ref); err != nil {
		writeError(w, 400, err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.configuring {
		writeError(w, 409, errors.New("服务正在更新"))
		return
	}
	if r.Method == http.MethodDelete {
		err := s.store.UnreferenceEvidenceChain(r.PathValue("id"), ref)
		respond(w, map[string]bool{"removed": err == nil}, err)
		return
	}
	snap, err := s.store.ReferenceEvidenceChain(r.PathValue("id"), ref)
	respond(w, map[string]any{"references": snap.EvidenceChainRefs}, err)
}

func (s *Server) deleteEvidenceChain(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || s.configuring || s.active[r.PathValue("run")] {
		writeError(w, 409, errors.New("请先等待运行或配置更新结束"))
		return
	}
	err := s.store.DeleteEvidenceChain(r.PathValue("run"), r.PathValue("chain"))
	respond(w, map[string]bool{"removed": err == nil}, err)
}
