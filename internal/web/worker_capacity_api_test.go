package web

import (
	"encoding/json"
	"testing"

	"trace/internal/agent"
)

func TestWorkerCapacityReadOnlyAPI(t *testing.T) {
	s, a := fixture(t, nil)
	run, err := a.Create("capacity api")
	if err != nil {
		t.Fatal(err)
	}
	before, err := a.Store().Load(run.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	r := request(t, s.Handler(), "GET", "/api/runs/"+run.Run.ID+"/worker-capacity", nil, 200)
	var state agent.WorkerCapacityState
	if err := json.Unmarshal(r.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.RootRunID != run.Run.ID || state.Active != 0 || state.Capacity != 5 {
		t.Fatalf("unexpected tree state: %+v", state)
	}
	after, err := a.Store().Load(run.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if before.Run.EventSeq != after.Run.EventSeq {
		t.Fatal("read-only capacity endpoint changed lifecycle")
	}
	request(t, s.Handler(), "GET", "/api/runs/missing/worker-capacity", nil, 404)
}
