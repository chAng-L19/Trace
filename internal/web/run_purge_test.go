package web

import (
	"encoding/json"
	"testing"

	"trace/internal/agent"
)

func TestRunPermanentDeleteAndHiddenManagementAPI(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("remove history")
	if err != nil {
		t.Fatal(err)
	}
	h, path := s.Handler(), "/api/runs/"+snap.Run.ID
	request(t, h, "DELETE", path, map[string]string{"mode": "typo"}, 400)
	request(t, h, "DELETE", path, map[string]string{"mode": "hide"}, 200)
	w := request(t, h, "GET", "/api/runs?include_deleted=true", nil, 200)
	var runs []agent.Run
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil || len(runs) != 1 || !runs[0].Deleted {
		t.Fatalf("hidden list: %s %v", w.Body, err)
	}
	w = request(t, h, "DELETE", path, map[string]string{"mode": "permanent"}, 200)
	var result struct {
		Permanent bool
		RunIDs    []string `json:"run_ids"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.Permanent || len(result.RunIDs) != 1 {
		t.Fatalf("purge: %s %v", w.Body, err)
	}
	request(t, h, "GET", path, nil, 404)
	request(t, h, "GET", path+"/events", nil, 404)
	w = request(t, h, "GET", "/api/runs?include_deleted=true", nil, 200)
	if w.Body.String() != "[]\n" {
		t.Fatal(w.Body.String())
	}
}
