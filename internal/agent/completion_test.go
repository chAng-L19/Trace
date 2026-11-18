package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestContextProjectionPreservesArchiveAndToolPairs(t *testing.T) {
	s := Snapshot{Run: Run{ID: "ctx", Context: ContextConfig{WindowTokens: 5000, OutputReserve: 500, CompactionMode: "projection"}}, Prompt: "system"}
	s.Messages = []Message{{Role: "user", Content: "goal: preserve evidence"}}
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("c%d", i)
		s.Messages = append(s.Messages, Message{Role: "assistant", Content: strings.Repeat("decision ", 80), ToolCalls: []ToolCall{{ID: id, Name: "exec", Arguments: "{}"}}}, Message{Role: "tool", ToolCallID: id, Content: strings.Repeat("large result ", 500)})
	}
	s.Messages = append(s.Messages, Message{Role: "user", Content: "latest: report exact findings"})
	original := s.Messages[2].Content
	messages, n, cid, err := projectContext(&s, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if n > 4500 || cid == "" || len(s.Compactions) != 1 {
		t.Fatalf("projection not budgeted: %d %s", n, cid)
	}
	if s.Messages[2].Content != original || len(s.Messages) != 26 {
		t.Fatal("archive was modified")
	}
	if len(pendingToolCalls(&Snapshot{Messages: messages})) != 0 {
		t.Fatal("projection contains orphan calls")
	}
	calls := map[string]bool{}
	for _, m := range messages {
		for _, c := range m.ToolCalls {
			calls[c.ID] = true
		}
		if m.Role == "tool" && !calls[m.ToolCallID] {
			t.Fatalf("orphan tool result %s", m.ToolCallID)
		}
	}
	if messages[len(messages)-1].Content != "latest: report exact findings" {
		t.Fatal("latest user message lost")
	}
	again, n2, cid2, err := projectContext(&s, nil, "")
	if err != nil || n != n2 || cid != cid2 || len(again) != len(messages) || len(s.Compactions) != 1 {
		t.Fatalf("projection not stable: %v", err)
	}
}

func TestContextCountsSystemAndSchemaAndModelMaximum(t *testing.T) {
	a, _ := newTestAgent(t, successProvider{})
	if DefaultContextConfig().WindowTokens != 250000 {
		t.Fatal("default window")
	}
	if err := a.ConfigureContext(ContextConfig{WindowTokens: 9000, ModelMaxTokens: 1200, OutputReserve: 200}); err != nil {
		t.Fatal(err)
	}
	s := Snapshot{Run: Run{ID: "ctx", Context: a.contextConfig}, Prompt: strings.Repeat("x", 2000), Messages: []Message{{Role: "user", Content: "goal"}}}
	if _, _, _, err := projectContext(&s, nil, ""); err == nil {
		t.Fatal("system omitted from estimate")
	}
	s.Prompt = "system"
	tool := ToolDefinition{Type: "function"}
	tool.Function.Description = strings.Repeat("schema", 500)
	if _, _, _, err := projectContext(&s, []ToolDefinition{tool}, ""); err == nil {
		t.Fatal("schema omitted from estimate")
	}
}

func TestStreamDeltasAggregateAndTruncationArchivesPartial(t *testing.T) {
	for _, truncated := range []bool{false, true} {
		t.Run(fmt.Sprint(truncated), func(t *testing.T) {
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				var req map[string]any
				json.NewDecoder(r.Body).Decode(&req)
				if req["stream"] != true || req["max_tokens"] != float64(16384) {
					t.Errorf("stream request missing settings: %v", req)
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if !truncated && requests > 1 {
					raw, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]string{"content": fixtureReport("continued after unavailable tool")}, "finish_reason": "stop"}}})
					fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", raw)
					return
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"think \"}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"answer\",\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"external\",\"arguments\":\"{\"}}]}}]}\n\n")
				fmt.Fprint(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\n")
				if !truncated {
					fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":3,\"total_tokens\":11}}\n\ndata: [DONE]\n\n")
				}
			}))
			defer server.Close()
			a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "fixture"})
			if truncated {
				a.ConfigureBudget(BudgetOverride{MaxModelRequests: intPointer(3)})
			}
			id := createTestRun(t, a)
			snap, err := a.Run(context.Background(), id)
			if truncated {
				if err != nil || snap.Run.Status != StatusPaused || !strings.Contains(snap.Run.PauseReason, "budget") || len(snap.Actions) != 0 {
					t.Fatal("truncated stream accepted")
				}
			} else if err != nil || snap.Run.Status != StatusCompleted {
				t.Fatalf("complete stream failed: %v %s", err, snap.Run.Status)
			}
			saved, _ := store.Load(id)
			response := saved.Responses[0].Response
			if response.Content != "answer" || response.Thinking != "think " || len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments != "{}" {
				t.Fatalf("delta aggregation lost data: %+v", response)
			}
			if !truncated {
				first := saved.Messages[1]
				if first.Role != "assistant" || first.RequestID != saved.Responses[0].RequestID {
					t.Fatalf("assistant request identity missing: %+v", first)
				}
			}
			count := 0
			requestIDs := map[string]bool{}
			for _, request := range saved.Requests {
				requestIDs[request.ID] = true
			}
			if truncated && len(saved.Requests) != 3 {
				t.Fatalf("incomplete SSE explicit request budget changed: %d", len(saved.Requests))
			}
			var kinds []string
			var thinking, text, callID, callName, arguments string
			for i, event := range saved.Events {
				if event.Sequence != i+1 {
					t.Fatal("event sequence gap")
				}
				if event.Type == "model_stream" {
					count++
					raw, _ := json.Marshal(event.Data)
					var data struct {
						RequestID string      `json:"request_id"`
						Delta     StreamDelta `json:"delta"`
					}
					if err := json.Unmarshal(raw, &data); err != nil {
						t.Fatal(err)
					}
					if !requestIDs[data.RequestID] {
						t.Fatal("stream request identity lost")
					}
					if data.RequestID != saved.Requests[0].ID {
						count--
						continue
					}
					d := data.Delta
					if len(kinds) == 0 || kinds[len(kinds)-1] != d.Kind {
						kinds = append(kinds, d.Kind)
					}
					switch d.Kind {
					case "thinking":
						thinking += d.Text
					case "text":
						text += d.Text
					case "tool_call":
						callID += d.ID
						callName += d.Name
						arguments += d.Arguments
					}
				}
			}
			if count < 3 || count > 4 || strings.Join(kinds, ",") != "thinking,text,tool_call" || thinking != "think " || text != "answer" || callID != "c1" || callName != "external" || arguments != "{}" {
				t.Fatalf("batched stream changed order/content: events=%d kinds=%v thinking=%q text=%q call=%s/%s/%s", count, kinds, thinking, text, callID, callName, arguments)
			}
		})
	}
}

type mailboxProvider struct {
	mu       sync.Mutex
	n        int
	entered  chan struct{}
	release  chan struct{}
	requests []ModelRequest
}

func (p *mailboxProvider) Complete(ctx context.Context, r ModelRequest) (ModelResponse, error) {
	p.mu.Lock()
	p.n++
	n := p.n
	p.requests = append(p.requests, r)
	p.mu.Unlock()
	if n == 1 {
		close(p.entered)
		select {
		case <-p.release:
		case <-ctx.Done():
			return ModelResponse{}, ctx.Err()
		}
	}
	return ModelResponse{Content: fmt.Sprintf("answer %d", n)}, nil
}
func TestDurableMailboxDuringModelAndBranchIdentity(t *testing.T) {
	p := &mailboxProvider{entered: make(chan struct{}), release: make(chan struct{})}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	done := make(chan error, 1)
	go func() { _, err := a.Run(context.Background(), id); done <- err }()
	<-p.entered
	// A separate Store represents a second process/UI instance.
	otherStore, _ := NewStore(store.root)
	other, _ := New(otherStore, successProvider{}, "")
	if _, err := other.QueueMessage(id, "steer", "steering instruction"); err != nil {
		t.Fatal(err)
	}
	if _, err := other.QueueMessage(id, "follow_up", "follow-up instruction"); err != nil {
		t.Fatal(err)
	}
	queued, _ := otherStore.Load(id)
	if len(queued.Mailbox) != 2 || !queued.Mailbox[0].ConsumedAt.IsZero() {
		t.Fatal("queue not durable before boundary")
	}
	close(p.release)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("mailbox continuation hung")
	}
	s, _ := store.Load(id)
	if len(s.Requests) != 2 || s.Run.Status != StatusCompleted {
		t.Fatalf("mailbox not drained: %d %s", len(s.Requests), s.Run.Status)
	}
	second, observations := splitContextObservations(id, s.Requests[1].Messages)
	if len(observations) == 0 || len(second) < 2 || second[len(second)-2].Content != "steering instruction" || second[len(second)-1].Content != "follow-up instruction" {
		t.Fatal("final boundary did not deliver both queue kinds to the next request")
	}
	for _, q := range s.Mailbox {
		if q.ConsumedAt.IsZero() || q.MessageID == "" {
			t.Fatal("consumption not durable")
		}
	}
	parent := ""
	for _, m := range s.Messages {
		if m.ID == "" || m.ParentID != parent {
			t.Fatalf("invalid message chain: %+v", m)
		}
		parent = m.ID
	}
	branch, err := a.Branch(id, s.Messages[1].ID)
	if err != nil {
		t.Fatal(err)
	}
	if branch.Run.ParentRunID != id || branch.Run.ParentMessageID != s.Messages[1].ID || len(branch.Messages) != 2 || branch.Messages[0].ID != s.Messages[0].ID {
		t.Fatal("branch ancestry lost")
	}
	if _, err := a.Message(context.Background(), branch.Run.ID, "new branch"); err != nil {
		t.Fatal(err)
	}
	original, _ := store.Load(id)
	if len(original.Messages) != len(s.Messages) {
		t.Fatal("branch changed parent")
	}
}
func TestSkillsDiscoveryMetadataAndOnDemandBody(t *testing.T) {
	root := t.TempDir()
	os.Mkdir(filepath.Join(root, "fixture"), 0700)
	path := filepath.Join(root, "fixture", "SKILL.md")
	os.WriteFile(path, []byte("---\nname: fixture\ndescription: >\n  first line\n  second line\n---\nsecret body v1"), 0600)
	a, _ := newTestAgent(t, successProvider{})
	if err := a.ConfigureSkills([]string{root}); err != nil {
		t.Fatal(err)
	}
	if len(a.Skills()) != 1 || a.Skills()[0].Description != "first line second line" || strings.Contains(a.skillCatalog(), "secret body") {
		t.Fatal("body eagerly exposed")
	}
	os.WriteFile(path, []byte("body changed after discovery"), 0600)
	body, err := a.ReadSkill("fixture")
	if err != nil || body != "body changed after discovery" {
		t.Fatal("body not read on demand")
	}
	if _, err := a.ReadSkill("../outside"); err == nil {
		t.Fatal("unknown skill accepted")
	}
}

func TestContextPauseThenReconfigureResume(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	if err := a.ConfigureContext(ContextConfig{WindowTokens: 500, OutputReserve: 100}); err != nil {
		t.Fatal(err)
	}
	id := createTestRun(t, a)
	paused, err := a.Run(context.Background(), id)
	if err != nil || paused.Run.Status != StatusPaused || !strings.HasPrefix(paused.Run.PauseReason, "context budget") || len(paused.Requests) != 0 {
		t.Fatalf("oversized context not paused: %v %+v", err, paused.Run)
	}
	if err := a.ConfigureContext(DefaultContextConfig()); err != nil {
		t.Fatal(err)
	}
	resumed, err := a.Run(context.Background(), id)
	if err != nil || resumed.Run.Status != StatusCompleted {
		t.Fatalf("context resume failed: %v %+v", err, resumed.Run)
	}
	saved, _ := store.Load(id)
	if saved.Run.Context.WindowTokens != 250000 {
		t.Fatal("updated limit not persisted")
	}
}
func TestBranchRejectsPartialMultiToolBatch(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	id := createTestRun(t, a)
	s, err := store.Update(id, func(s *Snapshot) error {
		s.Messages = []Message{{Role: "user", Content: "goal"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Name: "external"}, {ID: "b", Name: "external"}}}, {Role: "tool", ToolCallID: "a", Content: "one"}, {Role: "tool", ToolCallID: "b", Content: "two"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Branch(id, s.Messages[2].ID); err == nil {
		t.Fatal("partial batch branched")
	}
	if _, err := a.Branch(id, s.Messages[3].ID); err != nil {
		t.Fatal(err)
	}
}
func TestPausedMailboxSurvivesNewAgentAndCancelledRejects(t *testing.T) {
	p := &blockingProvider{entered: make(chan struct{}, 1), release: make(chan struct{})}
	a, store := newTestAgent(t, p)
	id := createTestRun(t, a)
	done := make(chan struct{})
	go func() { a.Run(context.Background(), id); close(done) }()
	<-p.entered
	if _, err := a.Pause(id); err != nil {
		t.Fatal(err)
	}
	if _, err := a.QueueMessage(id, "steer", "persistent correction"); err != nil {
		t.Fatal(err)
	}
	<-done
	restored, err := New(store, successProvider{}, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := restored.Run(context.Background(), id)
	if err != nil || s.Run.Status != StatusCompleted || len(s.Mailbox) != 1 || s.Mailbox[0].ConsumedAt.IsZero() {
		t.Fatalf("restart queue failed: %v %+v", err, s.Run)
	}
	other := createTestRun(t, a)
	a.Cancel(other)
	if _, err := a.QueueMessage(other, "follow_up", "should not enqueue"); err == nil {
		t.Fatal("cancelled accepted queue")
	}
}
func TestStreamCancellationRetainsPartialEvents(t *testing.T) {
	sent := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial text\"}}]}\n\n")
		w.(http.Flusher).Flush()
		close(sent)
		<-r.Context().Done()
	}))
	defer server.Close()
	a, store := newTestAgent(t, &OpenAIProvider{BaseURL: server.URL, Model: "fixture"})
	id := createTestRun(t, a)
	done := make(chan struct{})
	go func() { a.Run(context.Background(), id); close(done) }()
	<-sent
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, _ := store.Load(id)
		found := false
		for _, e := range s.Events {
			if e.Type == "model_stream" {
				found = true
			}
		}
		if found {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("stream event was not persisted before completion")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := a.Pause(id); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream cancellation hung")
	}
	s, _ := store.Load(id)
	if s.Run.Status != StatusPaused || len(s.Responses) != 1 || s.Responses[0].Response.Content != "partial text" || s.Responses[0].Error == "" {
		t.Fatalf("partial stream not archived: %+v", s.Responses)
	}
	for _, m := range s.Messages {
		if m.Role == "assistant" {
			t.Fatal("partial assistant advanced conversation")
		}
	}
}
