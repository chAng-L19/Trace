package web

import (
	"encoding/json"
	"testing"
	"trace/internal/agent"
)

func TestEvidenceCatalogRunQuery(t *testing.T) {
	s, a := fixture(t, nil)
	first, _ := a.Create("first")
	second, _ := a.Create("second")
	for _, run := range []agent.Snapshot{first, second} {
		if _, err := a.Store().Update(run.Run.ID, func(s *agent.Snapshot) error {
			s.Evidence = []agent.Evidence{{ID: "e", Type: "manual_observation", Content: "one"}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	var entries []agent.EvidenceCatalogEntry
	w := request(t, s.Handler(), "GET", "/api/evidence?run="+first.Run.ID, nil, 200)
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil || len(entries) != 1 || entries[0].RunID != first.Run.ID {
		t.Fatalf("scoped catalog: %s %v", w.Body, err)
	}
	request(t, s.Handler(), "GET", "/api/evidence?run=", nil, 404)
	request(t, s.Handler(), "GET", "/api/evidence-chains?run=", nil, 404)
}
