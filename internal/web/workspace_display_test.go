package web

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"trace/internal/agent"
)

func TestPlannerAndMessageHTTPDisplayWithoutAcceptance(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			root := t.TempDir()
			if backend == "sqlite" {
				if err := agent.MigrateStoreToSQLite(root); err != nil {
					t.Fatal(err)
				}
			}
			store, err := agent.NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			a, err := agent.New(store, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(a, Options{InitialConfig: Config{Context: agent.DefaultContextConfig()}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { s.Close() })
			created, err := a.Create("display tabs")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update(created.Run.ID, func(v *agent.Snapshot) error {
				v.Run.Status, v.Run.PauseReason = agent.StatusPaused, "operator_pause"
				v.Plan.Tasks = []agent.PlanTask{{ID: "task", Goal: "persisted work", Status: "running", WorkerID: "worker", ChildRunID: "missing-source"}}
				v.Workers = []agent.AgentWorker{{ID: "worker", TaskID: "task", RunID: "missing-source", Status: "uncertain", CleanupStatus: "pending"}, {ID: "old", Status: "completed", CleanupStatus: "completed"}}
				v.ChildReports = []agent.ChildReport{{ParentRunID: created.Run.ID, WorkerID: "worker", TaskID: "task", SourceRunID: "missing-source", SourceStatus: "available", Current: true}}
				v.Messages = nil
				for i := 0; i < 90; i++ {
					v.Messages = append(v.Messages, agent.Message{ID: fmt.Sprint("m", i), Role: "user", Content: fmt.Sprint("message", i)})
				}
				v.Requests = []agent.ModelRequest{{ID: "request", ContextManifest: &agent.ContextManifest{Hash: "detail-only"}, Messages: []agent.Message{{Content: "private context"}}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			base := "/api/runs/" + created.Run.ID
			for _, query := range []string{"?view=workspace&tab=planner", "?view=workspace&tab=events", "/messages?before=m80&limit=80"} {
				w := request(t, s.Handler(), "GET", base+query, nil, 200)
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &fields); err != nil {
					t.Fatal(err)
				}
				if len(fields["result"])+len(fields["coverage"]) != 0 {
					t.Fatal("display route asserted source acceptance")
				}
				var got workspaceView
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if got.Run.Status != agent.StatusPaused || got.Run.PauseReason != "operator_pause" {
					t.Fatal("display changed pause semantics")
				}
				for _, req := range got.Requests {
					if req.ContextManifest != nil || len(req.Messages) > 0 {
						t.Fatal("display retained request details")
					}
				}
				if query == "?view=workspace&tab=planner" {
					if got.Projection != "planner" || len(got.Workers) != 2 || len(got.Requests)+len(got.Actions)+len(got.Events) != 0 || got.ChildReports[0].SourceStatus != "not_checked" {
						t.Fatal("Planner loaded unrelated history or lost Worker identity")
					}
				}
				if query == "/messages?before=m80&limit=80" && (len(got.Messages) != 80 || got.MessagesPage.Start != 0 || got.MessagesPage.End != 80 || got.MessagesPage.Total != 90 || got.Messages[0].ID != "m0" || got.Messages[79].ID != "m79") {
					t.Fatal("HTTP history repaginated the selected source window")
				}
			}
			preview, err := a.Create("preview text")
			if err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("完整正文𠮷", 3000)
			_, err = store.Update(preview.Run.ID, func(v *agent.Snapshot) error {
				v.Messages = []agent.Message{{ID: "full-message", Role: "assistant", RequestID: "r", Content: body, Thinking: body, ToolCalls: []agent.ToolCall{{ID: "c", Name: "exec", Arguments: body}}}}
				v.Requests = []agent.ModelRequest{{ID: "r", Messages: []agent.Message{{Content: body}}}}
				v.Responses = []agent.ResponseRecord{{RequestID: "r", Response: agent.ModelResponse{Content: body, Thinking: body}}}
				v.Actions = []agent.Action{{RequestID: "r", CallID: "c", Status: "uncertain", Arguments: body, Result: &agent.ToolResult{Stdout: body, OutcomeUncertain: true}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			base = "/api/runs/" + preview.Run.ID
			for _, query := range []string{"?view=workspace&activity=true&workers=false", "/messages?limit=40", "/messages?around=full-message"} {
				w := request(t, s.Handler(), "GET", base+query, nil, 200)
				var got workspaceView
				if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				if len(got.Messages) != 1 || !got.Messages[0].Preview || got.Messages[0].ContentBytes != len(body) || got.Messages[0].ThinkingBytes != len(body) || len(got.Messages[0].Content) > 4096 || got.Messages[0].ToolCalls[0].ID != "c" {
					t.Fatal("HTTP preview discarded original source lengths or identity")
				}
			}
			var full agent.Message
			if err := json.Unmarshal(request(t, s.Handler(), "GET", base+"/messages/full-message", nil, 200).Body.Bytes(), &full); err != nil || full.Content != body || full.Thinking != body || full.ToolCalls[0].Arguments != body {
				t.Fatal("full message detail lost original text", err)
			}
			var action agent.Action
			if err := json.Unmarshal(request(t, s.Handler(), "GET", base+"/actions/c?request=r", nil, 200).Body.Bytes(), &action); err != nil || action.Arguments != body || action.Result.Stdout != body || !action.Result.OutcomeUncertain {
				t.Fatal("full action detail lost original outcome", err)
			}
		})
	}
}
