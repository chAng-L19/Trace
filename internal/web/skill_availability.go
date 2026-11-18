package web

import (
	"context"
	"errors"
	"net/http"
)

func (s *Server) putSkillAvailability(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Disabled *bool `json:"disabled"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Disabled == nil {
		writeError(w, 400, errors.New("disabled is required"))
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.configurationReady(); err != nil {
		writeError(w, 409, err)
		return
	}
	if !s.checkConfigRevision(w, r) {
		return
	}
	name := r.PathValue("name")
	found := false
	for _, skill := range s.a.Skills() {
		if skill.Name == name {
			found = true
			break
		}
	}
	if !found {
		writeError(w, 404, errors.New("skill not found"))
		return
	}
	next := cloneConfig(s.config)
	next.DisabledSkills = nil
	for _, previous := range s.config.DisabledSkills {
		if previous != name {
			next.DisabledSkills = append(next.DisabledSkills, previous)
		}
	}
	if *in.Disabled {
		next.DisabledSkills = append(next.DisabledSkills, name)
	}
	s.configuring = true
	s.wg.Add(1)
	defer func() { s.configuring = false; s.wg.Done() }()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	result, err := s.applyConfig(ctx, next)
	s.configRevisionHeader(w)
	respond(w, result, err)
}
