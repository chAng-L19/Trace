package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestActivityProjectionBoundedAndDetached(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store *Store
			if backend == "sqlite" {
				store = sqliteTestStore(t)
			} else {
				var err error
				store, err = NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			}
			snap, err := store.Create("activity", "private prompt", "hash")
			if err != nil {
				t.Fatal(err)
			}
			snap, err = store.Update(snap.Run.ID, func(s *Snapshot) error {
				s.Messages = nil
				s.Requests = nil
				s.Responses = nil
				s.Actions = nil
				for i := 0; i < 100; i++ {
					id := fmt.Sprintf("r%d", i)
					s.Messages = append(s.Messages, Message{Role: "assistant", RequestID: id, Content: id})
					s.Requests = append(s.Requests, ModelRequest{ID: id, Messages: []Message{{Content: "private duplicated history"}}})
					s.Responses = append(s.Responses, ResponseRecord{RequestID: id, Response: ModelResponse{Content: id}})
					s.Actions = append(s.Actions, Action{RequestID: id, CallID: "reused", Status: "completed", Result: &ToolResult{Stdout: "capture"}})
				}
				s.Requests = append(s.Requests, ModelRequest{ID: "inflight"})
				s.Actions = append(s.Actions, Action{RequestID: "inflight", CallID: "pending", Status: "started"})
				s.Workers = []AgentWorker{{ID: "old", Status: "completed", CleanupStatus: "completed"}, {ID: "paused", Status: "paused", CleanupStatus: "completed"}}
				s.ChildReports = []ChildReport{{SourceRunID: "does-not-exist", SourceStatus: "available", Current: true}}
				addEvent(s, "model_stream", "stream", map[string]string{"content": "not an activity event body"})
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			projection, err := store.ActivityWorkspace(snap.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			p := projection.Snapshot
			if len(p.Messages) != 40 || projection.MessagesPage.Total != 100 || projection.MessagesPage.Start != 60 || projection.MessagesPage.End != 100 || !projection.MessagesPage.HasBefore {
				t.Fatalf("window: %+v %d", projection.MessagesPage, len(p.Messages))
			}
			if p.Messages[0].ID != DisplayMessageID(snap.Run.ID, Message{}, 60) || len(p.Requests) != 41 || len(p.Actions) != 41 || len(p.Events) != 0 || p.Prompt != "" {
				t.Fatalf("projection shape: messages=%d requests=%d actions=%d events=%d", len(p.Messages), len(p.Requests), len(p.Actions), len(p.Events))
			}
			if len(p.Workers) != 1 || p.Workers[0].ID != "paused" || projection.Totals["workers"] != 2 || projection.Totals["messages"] != 100 {
				t.Fatal("display filtering corrupted totals")
			}
			if len(p.ChildReports) != 1 || p.ChildReports[0].SourceStatus != "unavailable" || p.ChildReports[0].Current {
				t.Fatal("receipt implied source validation")
			}
			for _, r := range p.Requests {
				if len(r.Messages) != 0 || len(r.Tools) != 0 {
					t.Fatal("duplicated request payload materialized")
				}
			}
			p.Messages[0].Content = "mutated"
			next, err := store.ActivityWorkspace(snap.Run.ID)
			if err != nil || next.Snapshot.Messages[0].Content == "mutated" {
				t.Fatal("cached projection is not detached")
			}
		})
	}
}

func TestActivityLegacyIdentityUsesFullBoundary(t *testing.T) {
	s := Snapshot{Run: Run{ID: "legacy"}}
	for i := 0; i < 45; i++ {
		s.Messages = append(s.Messages, Message{ID: fmt.Sprintf("m%d", i), Role: "user", Content: "q"})
	}
	s.Messages = append(s.Messages, Message{ID: "answer", Role: "assistant", Content: "same"}, Message{Role: "tool", Content: "tool"})
	s.Requests = []ModelRequest{{ID: "r", Messages: []Message{{ID: "m44"}}}}
	s.Responses = []ResponseRecord{{RequestID: "r", Response: ModelResponse{Content: "same"}}}
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	v := &storedView{}
	if err = v.retainSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	p, err := buildActivityRecords(v.fields, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Snapshot.Messages[38].RequestID != "r" || p.Snapshot.Messages[39].RequestID != "r" || p.MessagesPage.Start != 7 {
		t.Fatalf("lost full-history legacy boundary: %+v", p.MessagesPage)
	}
}

// Most requests are outside the visible window, but their manifests remain in
// the canonical snapshot and complete request details.
func activityRequestHistory(count, parts int) Snapshot {
	manifest := &ContextManifest{Boundary: "provider_messages_tools", Hash: strings.Repeat("a", 64)}
	for i := 0; i < parts; i++ {
		manifest.Parts = append(manifest.Parts, ContextManifestPart{Kind: "message", Index: i, ID: fmt.Sprint("m", i), Bytes: 1024, Hash: strings.Repeat("b", 64)})
	}
	s := Snapshot{Run: Run{ID: "activity-history"}}
	for i := 0; i < count; i++ {
		id := fmt.Sprint("r", i)
		s.Requests = append(s.Requests, ModelRequest{ID: id, ProviderID: "provider", ProviderModel: "model", ContextManifest: manifest, Messages: []Message{{ID: "context", Content: strings.Repeat("context ", 128)}}})
		if i != 3 { // An old request can still be in flight without a message.
			r := ResponseRecord{RequestID: id, Response: ModelResponse{Content: "answer"}}
			if i == 2 {
				r.Error = "recorded error"
			}
			s.Responses = append(s.Responses, r)
		}
	}
	// The latest response and latest request are independent retained identities.
	n := len(s.Responses)
	s.Responses[n-1], s.Responses[n-2] = s.Responses[n-2], s.Responses[n-1]
	for i := 0; i < 45; i++ {
		s.Messages = append(s.Messages, Message{ID: fmt.Sprint("m", i), Role: "user", Content: "question"})
	}
	s.Messages = append(s.Messages, Message{ID: "visible", Role: "assistant", RequestID: "r0", Content: "answer"})
	s.Actions = []Action{
		{RequestID: "r1", CallID: "uncertain", Status: "uncertain", Result: &ToolResult{OutcomeUncertain: true, WorkerCleanupError: "pending"}},
		{RequestID: "r2", CallID: "settled", Status: "completed", Result: &ToolResult{}},
		{RequestID: "r3", CallID: "pending", Status: "started"},
	}
	return s
}

func TestActivitySelectedMetadataPreservesIdentityAndInFlight(t *testing.T) {
	s := activityRequestHistory(100, 16)
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	v := &storedView{}
	if err = v.retainSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	p, err := buildActivityRecords(v.fields, nil)
	if err != nil {
		t.Fatal(err)
	}
	var expectedRequests []ModelRequest
	for _, index := range []int{0, 1, 3, 98, 99} {
		r := s.Requests[index]
		r.Messages, r.Tools = nil, nil
		r.ContextManifest = nil
		expectedRequests = append(expectedRequests, r)
	}
	expectedResponses := []ResponseRecord{s.Responses[0], s.Responses[1], s.Responses[97], s.Responses[98]}
	if !reflect.DeepEqual(p.Snapshot.Requests, expectedRequests) || !reflect.DeepEqual(p.Snapshot.Responses, expectedResponses) {
		t.Fatalf("selected identity/metadata changed: requests=%+v responses=%+v", p.Snapshot.Requests, p.Snapshot.Responses)
	}
	if !reflect.DeepEqual(p.Snapshot.Actions, []Action{s.Actions[0], s.Actions[2]}) || p.MessagesPage.Total != 46 || p.MessagesPage.Start != 6 || len(p.Snapshot.Messages) != 40 || p.Totals["requests"] != 100 {
		t.Fatal("in-flight, uncertain, or message-window semantics changed")
	}
	var original Snapshot
	if err = json.Unmarshal(v.snapshot, &original); err != nil || !reflect.DeepEqual(original.Requests, s.Requests) || !reflect.DeepEqual(original.Responses, s.Responses) {
		t.Fatal("activity changed canonical request/response details", err)
	}
	for _, fields := range []map[string]json.RawMessage{
		{"run": json.RawMessage(`{"id":"identity"}`), "requests": json.RawMessage(`[{"id":42},{"id":"latest"}]`)},
		{"run": json.RawMessage(`{"id":"identity"}`), "responses": json.RawMessage(`[{"request_id":42},{"request_id":"latest"}]`)},
		{"run": json.RawMessage(`{"id":"identity"}`), "requests": json.RawMessage(`[{"id":"latest","context_manifest":42}]`)},
	} {
		if _, err := buildActivityRecords(fields, nil); err == nil {
			t.Fatal("activity accepted malformed identity or selected request metadata")
		}
	}
	for _, invalid := range []string{`"context_manifest":42`, `"created_at":"bad"`, `"provider_model":42`} {
		fields := map[string]json.RawMessage{
			"run":       json.RawMessage(`{"id":"identity"}`),
			"requests":  json.RawMessage(`[{"id":"old",` + invalid + `},{"id":"latest"}]`),
			"responses": json.RawMessage(`[{"request_id":"old"},{"request_id":"latest"}]`),
		}
		if _, err := buildActivityRecords(fields, nil); err == nil {
			t.Fatal("activity accepted malformed unselected request metadata:", invalid)
		}
	}
}

func BenchmarkActivityProjectionHistoricalRequests(b *testing.B) {
	for _, count := range []int{100, 1000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s := activityRequestHistory(count, 128)
			for i := range s.Responses {
				s.Responses[i].Response.Thinking = strings.Repeat("archived response ", 1024)
			}
			raw, err := json.Marshal(s)
			if err != nil {
				b.Fatal(err)
			}
			v := &storedView{}
			if err = v.retainSnapshot(raw); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.SetBytes(int64(len(raw)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				p, err := buildActivityRecords(v.fields, nil)
				if err != nil || len(p.Snapshot.Requests) != 5 || len(p.Snapshot.Responses) != 4 || len(p.Snapshot.Messages) != 40 {
					b.Fatalf("invalid activity window: %v", err)
				}
			}
		})
	}
}

func TestActivityChildReportWindowPrefersCurrentVisits(t *testing.T) {
	for _, test := range []struct {
		name           string
		currentWorkers int
		filteredWorker bool
	}{
		{name: "older active receipt", currentWorkers: 1},
		{name: "active overflow stays bounded", currentWorkers: 16},
		{name: "task before worker filtering", currentWorkers: 1, filteredWorker: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := Snapshot{Run: Run{ID: "parent"}}
			for i := 0; i < test.currentWorkers; i++ {
				id := fmt.Sprintf("current-%02d", i)
				worker := AgentWorker{ID: id, TaskID: id, RunID: "child-" + id, Visit: 2, Status: "running", CleanupStatus: "pending"}
				if test.filteredWorker {
					worker.Status, worker.CleanupStatus = "completed", "completed"
				}
				s.Workers = append(s.Workers, worker)
				s.Plan.Tasks = append(s.Plan.Tasks, PlanTask{ID: id, WorkerID: id, ChildRunID: worker.RunID, Status: "running"})
				for _, revision := range []struct {
					visit uint64
					hash  string
				}{{1, "old-visit"}, {2, "early"}, {2, "final"}} {
					s.ChildReports = append(s.ChildReports, ChildReport{ParentRunID: s.Run.ID, SourceRunID: worker.RunID, TaskID: id, WorkerID: id, Visit: revision.visit, SourceHash: id + "-" + revision.hash})
				}
			}
			for i := 0; i < 30; i++ {
				s.ChildReports = append(s.ChildReports, ChildReport{ParentRunID: s.Run.ID, SourceRunID: fmt.Sprintf("history-%d", i), WorkerID: "historical", Visit: 1, SourceHash: fmt.Sprint(i)})
			}
			raw, err := json.Marshal(s)
			if err != nil {
				t.Fatal(err)
			}
			view := &storedView{}
			if err := view.retainSnapshot(raw); err != nil {
				t.Fatal(err)
			}
			projection, err := buildActivityRecords(view.fields, nil)
			if err != nil {
				t.Fatal(err)
			}
			if projection.Totals["child_reports"] != len(s.ChildReports) || len(projection.Snapshot.ChildReports) != 12 {
				t.Fatalf("receipt total/window: total=%d loaded=%d", projection.Totals["child_reports"], len(projection.Snapshot.ChildReports))
			}
			current := 0
			for _, report := range projection.Snapshot.ChildReports {
				if !strings.HasPrefix(report.SourceHash, "current-") {
					continue
				}
				current++
				if report.Visit != 2 || !strings.HasSuffix(report.SourceHash, "-final") || !report.Current || report.SourceStatus != "not_checked" {
					t.Fatalf("wrong active receipt or source claim: %+v", report)
				}
			}
			if current != min(test.currentWorkers, 12) {
				t.Fatalf("active receipts lost to history: got %d", current)
			}
			if test.filteredWorker && len(projection.Snapshot.Workers) != 0 {
				t.Fatal("completed worker was retained only to select a receipt")
			}
			again, err := buildActivityRecords(view.fields, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(projection.Snapshot.ChildReports, again.Snapshot.ChildReports) {
				t.Fatal("receipt overflow selection changed without a ledger change")
			}
		})
	}
}
