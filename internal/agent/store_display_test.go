package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestDisplayManifestOmittedBeforeCacheAndFullDetailPreserved(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			created, err := s.Create("large manifests", "", "")
			if err != nil {
				t.Fatal(err)
			}
			fixture := activityRequestHistory(50, 400)
			fixture.Messages = nil
			for i := range fixture.Requests {
				fixture.Messages = append(fixture.Messages, Message{ID: fmt.Sprint("m", i), Role: "assistant", RequestID: fixture.Requests[i].ID, Content: "answer"})
			}
			before, err := s.Update(created.Run.ID, func(v *Snapshot) error {
				v.Requests, v.Responses, v.Messages = fixture.Requests, fixture.Responses, fixture.Messages
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			manifest, _ := json.Marshal(fixture.Requests[0].ContextManifest)
			if 40*len(manifest) <= activityCacheMaxEntry {
				t.Fatal("fixture must exceed the original cache cap")
			}
			p, err := s.ActivityWorkspace(created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, request := range p.Snapshot.Requests {
				if request.ContextManifest != nil || request.Messages != nil || request.Tools != nil {
					t.Fatal("detail-only metadata retained")
				}
			}
			entry := s.activityCache.entries[created.Run.ID]
			if entry == nil || len(entry.data)+len(entry.run) > activityCacheMaxEntry {
				t.Fatal("compact large-history projection missed cache")
			}
			if _, err = s.ActivityWorkspace(created.Run.ID); err != nil || s.activityCache.entries[created.Run.ID] != entry {
				t.Fatal("stable warm projection rebuilt", err)
			}
			after, err := s.Load(created.Run.ID)
			if err != nil || !reflect.DeepEqual(before.Requests, after.Requests) || !reflect.DeepEqual(before.Responses, after.Responses) || !reflect.DeepEqual(before.Messages, after.Messages) {
				t.Fatal("display altered authoritative history", err)
			}
		})
	}
}

func TestDisplayPreviewKeepsOriginalLengthsIdentityAndDetails(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			created, err := s.Create("preview", "", "")
			if err != nil {
				t.Fatal(err)
			}
			body := strings.Repeat("完整正文𠮷", 3000)
			original, err := s.Update(created.Run.ID, func(v *Snapshot) error {
				v.Messages = []Message{{ID: "m", Role: "assistant", RequestID: "r", Content: body, Thinking: body}}
				for i := 0; i < 81; i++ {
					v.Messages[0].ToolCalls = append(v.Messages[0].ToolCalls, ToolCall{ID: fmt.Sprint("c", i), Name: "exec", Arguments: body})
				}
				v.Requests = []ModelRequest{{ID: "r", Messages: []Message{{Content: body}}}}
				v.Responses = []ResponseRecord{{RequestID: "r", Response: ModelResponse{Content: body, Thinking: body, ToolCalls: v.Messages[0].ToolCalls}}}
				v.Actions = []Action{{RequestID: "r", CallID: "c0", Name: "exec", Arguments: body, Status: "uncertain", Result: &ToolResult{Stdout: body, Stderr: body, Error: body, OutcomeUncertain: true}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.ActivityWorkspace(created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			m, meta := p.Snapshot.Messages[0], p.MessagePreviews["m"]
			if m.ID != "m" || m.RequestID != "r" || len(m.ToolCalls) != 80 || m.ToolCalls[79].ID != "c79" || !meta.Preview || meta.ContentBytes != len(body) || meta.ThinkingBytes != len(body) || len(m.Content) > 4096 || !utf8.ValidString(m.Content) || len(m.ToolCalls[0].Arguments) > 2048 {
				t.Fatal("preview lost original length, UTF8 boundary or tool identity")
			}
			if p.Snapshot.Responses[0].RequestID != "r" || p.Snapshot.Responses[0].Response.Content != "" || len(p.Snapshot.Responses[0].Response.ToolCalls) != 0 {
				t.Fatal("archived response body remained duplicated")
			}
			a := p.Snapshot.Actions[0]
			if a.RequestID != "r" || a.CallID != "c0" || a.Status != "uncertain" || !a.Result.OutcomeUncertain || len(a.Arguments) > 2048 || len(a.Result.Stdout) > 4096 {
				t.Fatal("action preview changed identity or uncertainty")
			}
			if s.activityCache.entries[created.Run.ID] == nil {
				t.Fatal("bounded original HTTP preview missed cache")
			}
			again, err := s.ActivityWorkspace(created.Run.ID)
			if err != nil || !reflect.DeepEqual(again.MessagePreviews, p.MessagePreviews) {
				t.Fatal("warm cache lost original preview lengths", err)
			}
			full, err := s.Load(created.Run.ID)
			if err != nil || !reflect.DeepEqual(full.Messages, original.Messages) || !reflect.DeepEqual(full.Actions, original.Actions) || !reflect.DeepEqual(full.Responses, original.Responses) || !reflect.DeepEqual(full.Requests, original.Requests) {
				t.Fatal("preview mutated full source detail", err)
			}
		})
	}
}

func TestDisplayMessagePagesKeepCommittedIdentity(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			created, err := s.Create("history", "", "")
			if err != nil {
				t.Fatal(err)
			}
			snap, err := s.Update(created.Run.ID, func(v *Snapshot) error {
				v.Messages = nil
				for i := 0; i < 105; i++ {
					id := fmt.Sprint("r", i)
					v.Messages = append(v.Messages, Message{Role: "assistant", RequestID: id, Content: id, ToolCalls: []ToolCall{{ID: "shared", Name: "exec", Arguments: id}}})
					v.Requests = append(v.Requests, ModelRequest{ID: id, Messages: []Message{{Content: "private history"}}})
					v.Responses = append(v.Responses, ResponseRecord{RequestID: id, Response: ModelResponse{Content: id}})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			cursor := DisplayMessageID(snap.Run.ID, snap.Messages[65], 65)
			for _, query := range []MessagePageQuery{{Before: cursor}, {After: cursor, Limit: 80}, {Around: cursor, Limit: 5}, {Request: "r20", Call: "shared"}, {Limit: 80}} {
				want, page, err := PageMessages(snap, query)
				if err != nil {
					t.Fatal(err)
				}
				got, err := s.MessageWorkspace(context.Background(), snap.Run.ID, query)
				if err != nil || !reflect.DeepEqual(want, got.Snapshot.Messages) || !reflect.DeepEqual(page, got.MessagesPage) {
					t.Fatalf("page %+v: got %+v want %+v: %v", query, got.MessagesPage, page, err)
				}
				if len(got.Snapshot.Events) != 0 || len(got.Snapshot.Evidence) != 0 || got.Snapshot.Result.Status != "" {
					t.Fatal("message window implied acceptance")
				}
			}
			if _, err := s.MessageWorkspace(context.Background(), snap.Run.ID, MessagePageQuery{Call: "shared"}); !errors.Is(err, os.ErrInvalid) {
				t.Fatal("ambiguous call cursor accepted", err)
			}
			if _, err := s.MessageWorkspace(context.Background(), snap.Run.ID, MessagePageQuery{Around: "absent"}); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("missing cursor accepted", err)
			}
		})
	}
}

func TestDisplayPlannerDoesNotLoadOrAttestChildSources(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			created, err := s.Create("planner", "", "")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Update(created.Run.ID, func(v *Snapshot) error {
				v.Plan.Tasks = []PlanTask{{ID: "t", Goal: "task", Status: "running", WorkerID: "w", ChildRunID: "missing"}}
				v.Workers = []AgentWorker{{ID: "w", TaskID: "t", RunID: "missing", Status: "uncertain", CleanupStatus: "pending"}, {ID: "historical", Status: "completed", CleanupStatus: "completed"}}
				v.ChildReports = []ChildReport{{ParentRunID: created.Run.ID, WorkerID: "w", TaskID: "t", SourceRunID: "missing", SourceStatus: "available", Current: true}}
				v.Requests = []ModelRequest{{ID: "r", Messages: []Message{{Content: "private context"}}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			p, err := s.PlannerWorkspace(context.Background(), created.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Snapshot.Workers) != 2 || p.Snapshot.Workers[0].Status != "uncertain" || p.Snapshot.ChildReports[0].SourceStatus != "not_checked" {
				t.Fatal("planner lost lifecycle or asserted source validation")
			}
			if len(p.Snapshot.Requests)+len(p.Snapshot.Responses)+len(p.Snapshot.Actions)+len(p.Snapshot.Events)+len(p.Snapshot.Evidence) != 0 || p.Snapshot.Result.Status != "" {
				t.Fatal("planner retained unrelated history/acceptance")
			}
		})
	}
}

func TestDisplayCancelledReadDoesNotWaitForAdmission(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			s := activityCacheStore(t, backend)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.activityReadMu.Lock()
			defer s.activityReadMu.Unlock()
			if _, err := s.ActivityWorkspaceContext(ctx, "run"); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := s.MessageWorkspace(ctx, "run", MessagePageQuery{Before: "m"}); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := s.PlannerWorkspace(ctx, "run"); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
