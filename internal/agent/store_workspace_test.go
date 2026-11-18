package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestWorkspaceSelectsDisplayFieldsAndKeepsLegacyBoundaries(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Create("workspace", "secret prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	snap, err = s.Update(snap.Run.ID, func(v *Snapshot) error {
		v.Messages = []Message{{ID: "m1", Role: "user", Content: "question"}, {ID: "m2", Role: "assistant", Content: "answer"}}
		v.Requests = []ModelRequest{{ID: "request", Messages: []Message{{ID: "m1", Role: "user", Content: "history body"}}, Tools: []ToolDefinition{{Type: "function"}}}}
		v.Responses = []ResponseRecord{{RequestID: "request", Response: ModelResponse{Content: "answer"}}}
		addEvent(v, "model_stream", "live", map[string]string{"request_id": "pending", "content": "answer"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.EventBatch(snap.Run.ID, 0); err != nil {
		t.Fatal(err)
	}
	got, err := s.Workspace(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "" || len(got.Requests) != 1 || len(got.Requests[0].Tools) != 0 || len(got.Requests[0].Messages) != 1 || got.Requests[0].Messages[0].ID != "m1" || got.Requests[0].Messages[0].Content != "" {
		t.Fatalf("workspace decoded duplicate payloads or lost legacy boundary: %+v", got.Requests)
	}
	got.Messages[0].Content = "caller mutation"
	got.Events[0].Data.(json.RawMessage)[0] = '!'
	_, events, err := s.EventBatch(snap.Run.ID, 0)
	if err != nil || !json.Valid(events[0]) {
		t.Fatal("workspace mutated SSE generation", err)
	}
	full, err := s.Load(snap.Run.ID)
	if err != nil || full.Prompt != "secret prompt" || full.Requests[0].Messages[0].Content != "history body" || full.Messages[0].Content != "question" {
		t.Fatalf("workspace altered full execution snapshot: %+v %v", full, err)
	}
	other, err := NewStore(s.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Update(snap.Run.ID, func(v *Snapshot) error { v.Run.Title = "external change"; return nil }); err != nil {
		t.Fatal(err)
	}
	got, err = s.Workspace(snap.Run.ID)
	if err != nil || got.Run.Title != "external change" {
		t.Fatalf("workspace missed external generation: %+v %v", got.Run, err)
	}
}

func TestWorkspaceKeepsProvenanceRevocationAndIdentityChecks(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Create("workspace", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	snap.Evidence = []Evidence{{ID: "missing-source", Verification: "verified"}}
	if err = s.saveLocked(snap); err != nil {
		t.Fatal(err)
	}
	got, err := s.Workspace(snap.Run.ID)
	if err != nil || got.Evidence[0].Verification == "verified" {
		t.Fatalf("workspace trusted stale source: %+v %v", got.Evidence, err)
	}
	data, err := os.ReadFile(filepath.Join(s.root, snap.Run.ID+".json"))
	if err != nil || !bytes.Contains(data, []byte("verification_invalidated")) {
		t.Fatalf("revocation not persisted: %s %v", data, err)
	}
	if err = os.WriteFile(filepath.Join(s.root, "wrong.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Workspace("wrong"); err == nil {
		t.Fatal("workspace accepted mismatched identity")
	}
	if _, err = s.Update(snap.Run.ID, func(v *Snapshot) error { v.Run.Deleted = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Workspace(snap.Run.ID); !os.IsNotExist(err) {
		t.Fatalf("workspace exposed deleted run: %v", err)
	}
}

func TestEventViewHoldsSurviveOtherReadersAndRelease(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Create("held feed", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	first, second := s.HoldEventView(id), s.HoldEventView(id)
	if _, err = s.viewFields(id, true); err != nil {
		t.Fatal(err)
	}
	s.ReleaseEventView(id)
	first()
	first() // Release is idempotent.
	if _, ok := s.eventViews.Load(id); !ok {
		t.Fatal("one reader discarded another reader's prefix")
	}
	if err = s.AppendEvents(id, []Event{{Type: "last-frame"}}); err != nil {
		t.Fatal(err)
	}
	run, events, err := s.EventBatch(id, 0)
	if err != nil || len(events) != 1 || run.EventSeq != 1 {
		t.Fatalf("held feed lost final frame: %+v %v", run, err)
	}
	second()
	if _, ok := s.eventViews.Load(id); ok {
		t.Fatal("last reader retained history")
	}
}

func TestWorkspaceTelemetryCacheKeepsLegacyOwnerAndFullLedger(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Create("display cache", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	release := s.HoldEventView(id)
	defer release()
	_, err = s.Update(id, func(v *Snapshot) error {
		v.Requests = []ModelRequest{{ID: "new"}}
		v.Actions = []Action{{RequestID: "old", CallID: "reused", Result: &ToolResult{}}, {RequestID: "new", CallID: "reused"}}
		addEvent(v, "model_request", "request", map[string]string{"request_id": "new"})
		addEvent(v, "action_started", "action", map[string]string{"call_id": "reused"})
		for i := 0; i < 300; i++ {
			addEvent(v, "worker_message", "metadata", nil)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Workspace(id); err != nil {
		t.Fatal(err)
	}
	if err = s.AppendEvents(id, []Event{{Type: "tool_output", Data: map[string]string{"call_id": "reused", "content": "current"}}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Workspace(id)
	if err != nil {
		t.Fatal(err)
	}
	output := got.Events[len(got.Events)-1]
	raw, _ := json.Marshal(output.Data)
	if output.Type != "tool_output" || !bytes.Contains(raw, []byte(`"request_id":"new"`)) {
		t.Fatalf("cached generation lost ownership: %s", raw)
	}
	full, err := s.Load(id)
	if err != nil || len(full.Events) != 303 {
		t.Fatalf("ledger changed: %d %v", len(full.Events), err)
	}
}

func TestWorkspaceMessagesSkipsEventProjectionWithoutWeakeningSources(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Create("page projection", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	_, err = s.Update(id, func(v *Snapshot) error {
		v.Messages = []Message{{ID: "message", Role: "assistant", RequestID: "archived", Content: "full message"}}
		addEvent(v, "model_stream", "frame", map[string]string{"request_id": "pending", "content": "stream body"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	release := s.HoldEventView(id)
	defer release()
	got, err := s.WorkspaceMessages(id)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 0 || len(got.Messages) != 1 || got.Messages[0].Content != "full message" {
		t.Fatalf("invalid message projection: %+v", got)
	}
	cached, _ := s.eventViews.Load(id)
	v := cached.(*storedView)
	if v.workspaceReady.Load() != nil || len(v.events) != 0 {
		t.Fatal("message page indexed or projected event ledger")
	}
	if bytes.Contains(v.workspaceBase.data, []byte("stream body")) {
		t.Fatal("display base retains event body")
	}
	got, err = s.Workspace(id)
	if err != nil || len(got.Events) != 1 {
		t.Fatalf("full workspace lost live event: %d %v", len(got.Events), err)
	}
	_, err = s.Update(id, func(v *Snapshot) error {
		v.Evidence = []Evidence{{ID: "missing-source", Verification: "verified"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.WorkspaceMessages(id)
	if err != nil || got.Evidence[0].Verification == "verified" {
		t.Fatalf("page trusted invalid source: %+v %v", got.Evidence, err)
	}
}

func TestWorkspaceConcurrentDisplayReadsDetachAndRelease(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := s.Create("concurrent display", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	_, err = s.Update(id, func(v *Snapshot) error {
		v.Messages = []Message{{ID: "m", Role: "user", Content: "original"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	release := s.HoldEventView(id)
	defer release()
	failures := make(chan error, 5)
	var wg sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				var got Snapshot
				var err error
				if reader%2 == 0 {
					got, err = s.Workspace(id)
				} else {
					got, err = s.WorkspaceMessages(id)
				}
				if err != nil {
					failures <- err
					return
				}
				if len(got.Messages) != 1 || got.Messages[0].Content != "original" {
					failures <- fmt.Errorf("shared message mutation: %+v", got.Messages)
					return
				}
				if reader%2 == 0 && len(got.Events) != got.Run.EventSeq {
					failures <- fmt.Errorf("mixed display generation: %d/%d", len(got.Events), got.Run.EventSeq)
					return
				}
				got.Messages[0].Content = "caller mutation"
				if len(got.Events) > 0 {
					raw := got.Events[0].Data.(json.RawMessage)
					raw[0] = '!'
				}
			}
		}(reader)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 25; i++ {
			if err := s.AppendEvents(id, []Event{{Type: "frame", Data: map[string]int{"index": i}}}); err != nil {
				failures <- err
				return
			}
		}
	}()
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if _, ok := s.eventViews.Load(id); !ok {
		t.Fatal("display read discarded held SSE view")
	}
	release()
	if _, ok := s.eventViews.Load(id); ok {
		t.Fatal("last reader retained display/event history")
	}
	if _, err = s.WorkspaceMessages(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.eventViews.Load(id); ok {
		t.Fatal("unheld message page retained full history")
	}
	if _, err = s.Workspace(id); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.eventViews.Load(id); ok {
		t.Fatal("unheld workspace retained full history")
	}
}
