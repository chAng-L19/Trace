package web

import (
	"encoding/json"
	"testing"
	"trace/internal/agent"
)

func TestCreateRunExecutionConfigHTTP(t *testing.T) {
	server, a := fixture(t, nil)
	for _, body := range []map[string]any{
		{"goal": "defaults"},
		{"goal": "configured", "exec": map[string]any{"timeout_seconds": 17}, "tool_schema": map[string]any{"mode": "deferred"}},
	} {
		w := request(t, server.Handler(), "POST", "/api/runs", body, 201)
		var s agent.Snapshot
		if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil {
			t.Fatal(err)
		}
		saved, err := a.Store().Load(s.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		want := 300
		if body["exec"] != nil {
			want = 17
		}
		if saved.Run.Exec == nil || saved.Run.Exec.TimeoutSeconds != want {
			t.Fatalf("API config lost: %+v", saved.Run.Exec)
		}
		if want == 17 && (saved.Run.ToolSchema.Mode != "deferred" || saved.Run.ToolSelection == nil || len(saved.Run.ToolSelection.IDs) != 0) {
			t.Fatalf("API selection lost: %+v", saved.Run)
		}
	}
	for _, body := range []map[string]any{
		{"goal": "bad", "exec": map[string]any{"timeout_seconds": 0}},
		{"goal": "bad", "exec": map[string]any{"timeout_seconds": 1.5}},
		{"goal": "bad", "tool_schema": map[string]any{"mode": "unknown"}},
		{"goal": "bad", "tool_schema": map[string]any{"mode": "narrow", "ids": []string{"exec"}}},
	} {
		want := 409
		if config, ok := body["exec"].(map[string]any); ok && config["timeout_seconds"] == 1.5 {
			want = 400
		}
		request(t, server.Handler(), "POST", "/api/runs", body, want)
	}
}
