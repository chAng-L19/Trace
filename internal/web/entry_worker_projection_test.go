package web

import (
	"crypto/sha256"
	"encoding/json"
	"reflect"
	"testing"

	"trace/internal/agent"
)

func TestCollapsedActivityWorkspaceDoesNotReadChildStore(t *testing.T) {
	// No child Store exists: the already committed parent projection is enough
	// to render the collapsed panel, including the parent-owned Worker status.
	s := &Server{}
	p := agent.ActivityProjection{Snapshot: agent.Snapshot{
		Run:      agent.Run{ID: "parent", EventSeq: 9},
		Workers:  []agent.AgentWorker{{ID: "worker", RunID: "child", Status: "paused"}},
		Requests: []agent.ModelRequest{{ID: "request", ContextManifest: &agent.ContextManifest{Hash: "original"}}},
	}}
	projected, err := s.activityWorkspace(p, false)
	if err != nil {
		t.Fatal(err)
	}
	view := projected.(workspaceView)
	if len(view.WorkerActivity) != 0 || len(view.Workers) != 1 || view.Workers[0].Status != "paused" || view.Run.EventSeq != 9 {
		t.Fatal("collapsed parent projection changed")
	}
	if len(view.Requests) != 1 || view.Requests[0].ContextManifest != nil || p.Snapshot.Requests[0].ContextManifest == nil || p.Snapshot.Requests[0].ContextManifest.Hash != "original" {
		t.Fatal("activity manifest omission mutated the detached input")
	}
}

func TestActivityWorkspaceDefersWorkerDetailsWithoutChangingParent(t *testing.T) {
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
			t.Cleanup(func() { _ = store.Close() })
			a, err := agent.New(store, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			s, err := New(a, Options{InitialConfig: Config{Context: agent.DefaultContextConfig()}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.Close() })
			parent, err := a.Create("entry parent")
			if err != nil {
				t.Fatal(err)
			}
			child, err := a.Create("entry child")
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update(child.Run.ID, func(v *agent.Snapshot) error {
				v.Run.ParentRunID, v.Run.ParentTaskID = parent.Run.ID, "task"
				v.Run.Status, v.Run.PauseReason = agent.StatusPaused, "operator_pause"
				v.Actions = []agent.Action{{RequestID: "request", CallID: "call", Name: "exec", Status: "uncertain", Result: &agent.ToolResult{OutcomeUncertain: true}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update(parent.Run.ID, func(v *agent.Snapshot) error {
				v.Run.Status, v.Run.PauseReason = agent.StatusPaused, "operator_pause"
				v.Messages = []agent.Message{{ID: "message", Role: "user", Content: "keep parent conversation"}}
				v.Workers = []agent.AgentWorker{{ID: "worker", TaskID: "task", RunID: child.Run.ID, Status: "paused", CleanupStatus: "completed"}}
				v.WorkerMessages = []agent.WorkerMessage{{ID: "help", From: "worker", To: "parent", Kind: "help", Content: "keep open help"}}
				v.Requests = []agent.ModelRequest{{ID: "parent-request", ContextManifest: &agent.ContextManifest{Boundary: "provider_messages_tools", Hash: "original", Parts: []agent.ContextManifestPart{{Kind: "message", ID: "message", Hash: "message-hash", Bytes: 24}}, Sections: []agent.ContextManifestSection{{Name: "user", Hash: "section-hash"}}, Omissions: []agent.ContextOmission{{Kind: "history", ID: "old", Reason: "not selected", SourceHash: "source-hash"}}}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			original, err := store.Load(parent.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			snapshotHash := func(value agent.Snapshot) [32]byte {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				return sha256.Sum256(data)
			}
			before := snapshotHash(original)
			base := "/api/runs/" + parent.Run.ID + "?view=workspace&activity=true"
			read := func(query string) map[string]json.RawMessage {
				t.Helper()
				w := request(t, s.Handler(), "GET", base+query, nil, 200)
				var value map[string]json.RawMessage
				if err := json.Unmarshal(w.Body.Bytes(), &value); err != nil {
					t.Fatal(err)
				}
				return value
			}
			collapsed := read("&workers=false")
			if _, ok := collapsed["worker_activity"]; ok {
				t.Fatal("collapsed activity read included child-history details")
			}
			var summaries []agent.ModelRequest
			if err := json.Unmarshal(collapsed["requests"], &summaries); err != nil || len(summaries) != 1 || summaries[0].ContextManifest != nil {
				t.Fatalf("activity request summary retained the context manifest: %v", err)
			}
			for _, query := range []string{"", "&workers=true"} {
				expanded := read(query)
				var rows []agent.WorkerActivity
				if err := json.Unmarshal(expanded["worker_activity"], &rows); err != nil {
					t.Fatal(err)
				}
				if len(rows) != 1 || !rows[0].Available || rows[0].Phase != "paused" || len(rows[0].Calls) != 1 || !rows[0].Calls[0].OutcomeUncertain {
					t.Fatalf("expanded/default route lost real child state: %+v", rows)
				}
				delete(expanded, "worker_activity")
				if !reflect.DeepEqual(collapsed, expanded) {
					t.Fatal("deferring child details changed the parent projection or committed watermark")
				}
			}
			detailResponse := request(t, s.Handler(), "GET", "/api/runs/"+parent.Run.ID+"/requests/parent-request", nil, 200)
			var detail agent.ModelRequest
			if err := json.Unmarshal(detailResponse.Body.Bytes(), &detail); err != nil || !reflect.DeepEqual(detail.ContextManifest, original.Requests[0].ContextManifest) {
				t.Fatalf("full request detail lost its original context manifest: %v", err)
			}
			fullResponse := request(t, s.Handler(), "GET", "/api/runs/"+parent.Run.ID+"?view=workspace", nil, 200)
			var full workspaceView
			if err := json.Unmarshal(fullResponse.Body.Bytes(), &full); err != nil || len(full.Requests) != 1 || !reflect.DeepEqual(full.Requests[0].ContextManifest, original.Requests[0].ContextManifest) {
				t.Fatalf("activity-only omission changed the full workspace contract: %v", err)
			}
			projection, err := store.ActivityWorkspace(parent.Run.ID)
			if err != nil || len(projection.Snapshot.Requests) != 1 || projection.Snapshot.Requests[0].ContextManifest != nil {
				t.Fatalf("Store activity cache retained detail-only context metadata: %v", err)
			}
			after, err := store.Load(parent.Run.ID)
			if err != nil || snapshotHash(after) != before {
				t.Fatalf("display-only reads changed the original snapshot hash: %v", err)
			}
			if _, err := store.Update(child.Run.ID, func(v *agent.Snapshot) error { v.Run.ParentTaskID = "different-task"; return nil }); err != nil {
				t.Fatal(err)
			}
			var rows []agent.WorkerActivity
			if err := json.Unmarshal(read("&workers=true")["worker_activity"], &rows); err != nil || len(rows) != 1 || rows[0].Available {
				t.Fatalf("expanded details accepted changed child identity: %+v %v", rows, err)
			}
			request(t, s.Handler(), "GET", "/api/runs/missing?view=workspace&activity=true&workers=false", nil, 404)
		})
	}
}
