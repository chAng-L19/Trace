package web

import (
	"encoding/json"
	"testing"

	"trace/internal/agent"
)

func TestEvidenceCatalogAPIIndependentOfConversation(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("catalog source")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Store().Update(snap.Run.ID, func(s *agent.Snapshot) error {
		s.Evidence = []agent.Evidence{{ID: "ev-1", Type: "manual_observation", Content: "original"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	path := "/api/evidence/" + snap.Run.ID + "/ev-1"
	request(t, h, "DELETE", "/api/runs/"+snap.Run.ID, map[string]bool{"retain_evidence": true}, 200)
	request(t, h, "GET", "/api/runs/"+snap.Run.ID, nil, 404)
	w := request(t, h, "GET", "/api/evidence", nil, 200)
	var entries []agent.EvidenceCatalogEntry
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil || len(entries) != 1 || !entries[0].RunDeleted {
		t.Fatalf("catalog: %s %v", w.Body, err)
	}
	request(t, h, "GET", path, nil, 200)
	request(t, h, "DELETE", path, nil, 200)
	request(t, h, "GET", path, nil, 404)
	request(t, h, "DELETE", path, nil, 404)
	w = request(t, h, "GET", "/api/evidence", nil, 200)
	if w.Body.String() != "[]\n" {
		t.Fatal("catalog should be empty", w.Body.String())
	}
}

func TestEvidenceDeletionAPIActiveAndRemoveOption(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("purge source")
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.Store().Update(snap.Run.ID, func(s *agent.Snapshot) error {
		s.Evidence = []agent.Evidence{{ID: "ev-1", Content: "original"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h, path := s.Handler(), "/api/runs/"+snap.Run.ID
	s.mu.Lock()
	s.active[snap.Run.ID] = true
	s.mu.Unlock()
	request(t, h, "DELETE", "/api/evidence/"+snap.Run.ID+"/ev-1", nil, 409)
	s.mu.Lock()
	delete(s.active, snap.Run.ID)
	s.mu.Unlock()
	request(t, h, "DELETE", path, map[string]string{"retain_evidence": "false"}, 400)
	request(t, h, "DELETE", path, map[string]bool{"retain_evidence": false}, 200)
	request(t, h, "GET", "/api/evidence/"+snap.Run.ID+"/ev-1", nil, 404)
	after, err := a.Store().Load(snap.Run.ID)
	if err != nil || !after.Run.Deleted || len(after.Evidence) != 0 {
		t.Fatalf("purge not persisted: %+v %v", after, err)
	}
}
