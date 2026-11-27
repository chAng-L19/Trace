package web

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestResourceLimitsCreationAPI(t *testing.T) {
	s, a := fixture(t, nil)
	h := s.Handler()
	limits := &agent.ResourceLimits{WorkerCeiling: &agent.WorkerCeiling{Capacity: 3, Source: "managed:fixture-pool", Scope: "root-tree"}}
	response := request(t, h, "POST", "/api/runs", map[string]any{"goal": "explicit worker ceiling", "resource_limits": limits}, 201)
	var root agent.Snapshot
	if err := json.Unmarshal(response.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	if root.Plan.Capacity != 3 || !reflect.DeepEqual(root.Run.ResourceLimits, limits) {
		t.Fatalf("creation API lost ceiling: %+v", root.Run.ResourceLimits)
	}
	base := "/api/runs/" + root.Run.ID
	response = request(t, h, "POST", base+"/plan/capacity", map[string]int{"capacity": 5}, 409)
	if !strings.Contains(response.Body.String(), "managed:fixture-pool") || !strings.Contains(response.Body.String(), "root-tree") {
		t.Fatal("capacity conflict omitted its declared source or scope")
	}
	request(t, h, "PUT", base+"/plan", map[string]any{"tasks": []agent.PlanTask{{ID: "first", Goal: "first unit"}}}, 200)
	request(t, h, "POST", base+"/plan/capacity", map[string]int{"capacity": 2}, 200)
	response = request(t, h, "GET", base+"/worker-capacity", nil, 200)
	var capacity agent.WorkerCapacityState
	if err := json.Unmarshal(response.Body.Bytes(), &capacity); err != nil {
		t.Fatal(err)
	}
	if capacity.Capacity != 2 || !reflect.DeepEqual(capacity.WorkerCeiling, limits.WorkerCeiling) {
		t.Fatalf("capacity API lost the creation fact: %+v", capacity)
	}
	saved, err := a.Store().Load(root.Run.ID)
	if err != nil || !reflect.DeepEqual(saved.Run.ResourceLimits, limits) || len(saved.Requests) != 0 {
		t.Fatalf("API changed pinned limits or started execution: %v", err)
	}
}

func TestResourceLimitsCreationAPIRejectsInvalidDeclarations(t *testing.T) {
	s, a := fixture(t, nil)
	for _, tc := range []struct {
		name     string
		capacity any
		source   any
		scope    string
		status   int
	}{
		{"string-number", "3", "user:request", "root-tree", 400},
		{"boolean-number", true, "user:request", "root-tree", 400},
		{"fraction", 2.5, "user:request", "root-tree", 400},
		{"zero", 0, "user:request", "root-tree", 409},
		{"missing-source", 3, "", "root-tree", 409},
		{"numeric-source", 3, 123, "root-tree", 400},
		{"foreign-scope", 3, "managed:pool", "external-instances", 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request(t, s.Handler(), "POST", "/api/runs", map[string]any{
				"goal":            "invalid explicit limit",
				"resource_limits": map[string]any{"worker_ceiling": map[string]any{"capacity": tc.capacity, "source": tc.source, "scope": tc.scope}},
			}, tc.status)
			runs, err := a.Store().List()
			if err != nil || len(runs) != 0 {
				t.Fatalf("rejected API creation persisted a run: %+v %v", runs, err)
			}
		})
	}
}
