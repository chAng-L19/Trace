package web

import (
	"testing"
	"trace/internal/agent"
)

func TestTreeBudgetHTTPExplicitRootContract(t *testing.T) {
	s, a := fixture(t, nil)
	root, err := a.Create("tree budget")
	if err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	path := "/api/runs/" + root.Run.ID + "/tree-budget"
	request(t, h, "PUT", path, agent.TreeBudget{MaxWorkers: -1}, 400)
	s.active[root.Run.ID] = true
	request(t, h, "PUT", path, agent.TreeBudget{MaxWorkers: 5}, 409)
	delete(s.active, root.Run.ID)
	request(t, h, "PUT", path, agent.TreeBudget{MaxDepth: 3, MaxWorkers: 5, MaxModelRequests: 20}, 200)
	saved, err := a.Store().Load(root.Run.ID)
	if err != nil || saved.Run.TreeBudget.MaxWorkers != 5 {
		t.Fatalf("budget not saved: %+v %v", saved.Run.TreeBudget, err)
	}
	request(t, h, "PUT", path, agent.TreeBudget{}, 200)
}
