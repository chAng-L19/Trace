package web

import (
	"encoding/json"
	"testing"

	"trace/internal/agent"
)

func TestArchiveDeletedRunIsHiddenAndCannotStart(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("archive delete")
	if err != nil {
		t.Fatal(err)
	}
	h, path := s.Handler(), "/api/runs/"+snap.Run.ID
	request(t, h, "DELETE", path, nil, 200)
	request(t, h, "GET", path, nil, 404)
	for _, suffix := range []string{"/export", "/export?format=report", "/export?format=evidence", "/coverage", "/usage", "/events/history"} {
		request(t, h, "GET", path+suffix, nil, 404)
	}
	request(t, h, "POST", path+"/workers/wait", map[string]int{"seconds": 1}, 404)
	for _, operation := range []string{"run", "resume", "message"} {
		request(t, h, "POST", path+"/"+operation, map[string]string{"content": "restart"}, 409)
	}
	w := request(t, h, "GET", "/api/runs", nil, 200)
	var runs []agent.Run
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil || len(runs) != 0 {
		t.Fatalf("deleted run listed: %s %v", w.Body.String(), err)
	}
	source, err := a.Store().Load(snap.Run.ID)
	if err != nil || !source.Run.Deleted || source.Run.Status != agent.StatusCreated {
		t.Fatalf("source lost or restarted: %+v %v", source.Run, err)
	}
}

func TestArchiveDeleteRejectsActiveWebOperation(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("active archive")
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.active[snap.Run.ID] = true
	s.mu.Unlock()
	request(t, s.Handler(), "DELETE", "/api/runs/"+snap.Run.ID, nil, 409)
	s.mu.Lock()
	delete(s.active, snap.Run.ID)
	s.mu.Unlock()
	request(t, s.Handler(), "GET", "/api/runs/"+snap.Run.ID, nil, 200)
}

func TestArchiveSummaryIncludesArchivedHiddenRuns(t *testing.T) {
	s, a := fixture(t, nil)
	snap, err := a.Create("archived and hidden")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/runs/" + snap.Run.ID
	request(t, s.Handler(), "PATCH", path, map[string]bool{"archived": true}, 200)
	request(t, s.Handler(), "DELETE", path, nil, 200)
	w := request(t, s.Handler(), "GET", "/api/runs?include_archived=true&include_deleted=true", nil, 200)
	var runs []agent.Run
	if err := json.Unmarshal(w.Body.Bytes(), &runs); err != nil || len(runs) != 1 || !runs[0].Archived || !runs[0].Deleted {
		t.Fatalf("combined summary lost hidden archive: %s %v", w.Body.String(), err)
	}
}
