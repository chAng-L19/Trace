package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestD2MethodApplicabilityBeforeLimit(t *testing.T) {
	a, store, source, _ := catalogFixture(t)
	for i, name := range []string{"flag submit task", "solve flag worker", "flag method result", "ELF bytecode recovery", "原型污染路径"} {
		if _, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: name, Summary: fmt.Sprintf("fixture %d", i), Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ goal, name string }{
		{"solve flag task", ""},
		{"验证任务", ""},
		{"solve ELF bytecode task and submit flag", "ELF bytecode recovery"},
		{"验证原型污染", "原型污染路径"},
		{"型", ""},
	} {
		hits := a.recallWorkerMethods(tc.goal)
		if tc.name == "" && len(hits) != 0 || tc.name != "" && (len(hits) != 1 || hits[0].Name != tc.name) {
			t.Fatalf("%q: %+v", tc.goal, hits)
		}
	}
	explicit, _, err := store.SearchEvidenceChains("flag", 50)
	if err != nil || len(explicit) < 3 {
		t.Fatal("automatic filtering changed explicit catalog search", err)
	}
}

func TestD2RepeatedReportsKeepReceiptsAndUrgentMessages(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	for i := 0; i < 12; i++ {
		if _, err := a.SendWorkerMessageRelated(parent, "parent", "worker", strings.Repeat("checkpoint unchanged ", 80), "report", ""); err != nil {
			t.Fatal(err)
		}
	}
	snap, err := a.SendWorkerMessageRelated(parent, "parent", "worker", "need exact decision", "help", "")
	if err != nil {
		t.Fatal(err)
	}
	help := snap.WorkerMessages[len(snap.WorkerMessages)-1].ID
	for _, kind := range []string{"reply", "resolved"} {
		if _, err = a.SendWorkerMessageRelated(parent, "parent", "worker", "decision confirmed", kind, help); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = a.SendWorkerMessage(parent, "parent", "worker", "legacy unchanged"); err != nil {
		t.Fatal(err)
	}
	before, _ := a.store.Load(parent)
	baseline := []Message{}
	for _, m := range before.WorkerMessages {
		if m.To == "worker" {
			baseline = append(baseline, Message{Role: "user", Content: workerMessageContent(m)})
		}
	}
	for i := 0; i < 2; i++ {
		if err = a.DeliverWorkerMessages(child); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := a.store.Load(parent)
	receiver, _ := a.store.Load(child)
	markers := 0
	for _, event := range receiver.Events {
		if strings.HasPrefix(event.Type, "worker_message:") {
			markers++
		}
	}
	if len(after.WorkerMessages) != len(before.WorkerMessages) || markers != 16 || len(receiver.Messages) != 5 {
		t.Fatalf("history=%d markers=%d projected=%d", len(after.WorkerMessages), markers, len(receiver.Messages))
	}
	for i, m := range after.WorkerMessages {
		if m.Content != before.WorkerMessages[i].Content || m.To == "worker" && !m.Delivered {
			t.Fatal("lost body or receipt")
		}
	}
	if EstimateContextTokens(receiver.Messages, nil) >= EstimateContextTokens(baseline, nil) {
		t.Fatal("fixture projection did not reduce tokens")
	}
	// This is a new delivery turn, not an indefinitely deduplicated event.
	if _, err = a.SendWorkerMessageRelated(parent, "parent", "worker", before.WorkerMessages[1].Content, "report", ""); err != nil {
		t.Fatal(err)
	}
	if err = a.DeliverWorkerMessages(child); err != nil {
		t.Fatal(err)
	}
	next, _ := a.store.Load(child)
	if len(next.Messages) != 6 {
		t.Fatal("fresh report was suppressed across delivery turns")
	}
	t.Logf("fixture only: reports=12 projected_reports=1 raw_messages=16 projected_messages=5 estimated_tokens=%d->%d; model request count and real-world latency not inferred", EstimateContextTokens(baseline, nil), EstimateContextTokens(receiver.Messages, nil))
}

func TestD2ReportProjectionBoundaries(t *testing.T) {
	messages := []WorkerMessage{
		{ID: "old", From: "a", To: "parent", Kind: "report", Content: "same", Delivered: true},
		{ID: "new", From: "a", To: "parent", Kind: "report", Content: "same"},
		{ID: "duplicate", From: "a", To: "parent", Kind: "report", Content: "same"},
		{ID: "other-sender", From: "b", To: "parent", Kind: "report", Content: "same"},
		{ID: "help", From: "a", To: "parent", Kind: "help", Content: "same"},
		{ID: "after-help", From: "a", To: "parent", Kind: "report", Content: "same"},
		{ID: "changed", From: "a", To: "parent", Kind: "report", Content: "new conclusion"},
		{ID: "legacy", From: "a", To: "parent", Content: "new conclusion"},
		{ID: "legacy-repeat", From: "a", To: "parent", Content: "new conclusion"},
	}
	pending := map[string]bool{}
	for _, m := range messages {
		pending[m.ID] = !m.Delivered
	}
	duplicates := duplicateWorkerReports(messages, pending, "parent")
	if len(duplicates) != 1 || !duplicates["duplicate"] {
		t.Fatalf("lost boundary: %+v", duplicates)
	}
}

func TestD2CommittedReceiverIsProjectionBoundary(t *testing.T) {
	a, parent, child := schedulingInbox(t, true, 0, 0)
	for i := 0; i < 2; i++ {
		if _, err := a.SendWorkerMessageRelated(parent, "parent", "worker", "same", "report", ""); err != nil {
			t.Fatal(err)
		}
	}
	root, _ := a.store.Load(parent)
	first := root.WorkerMessages[1]
	// Simulate committed receiver + lost sender ACK before a new report arrived.
	if _, err := a.store.Update(child, func(s *Snapshot) error {
		s.Messages = append(s.Messages, Message{Role: "user", Content: workerMessageContent(first)})
		addEvent(s, "worker_message:"+first.ID, "already committed", nil)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.DeliverWorkerMessages(child); err != nil {
		t.Fatal(err)
	}
	got, _ := a.store.Load(child)
	if len(got.Messages) != 2 {
		t.Fatal("sender ACK lag incorrectly erased a fresh receiver delivery")
	}
}

// Stateless scripted provider: actual Agent/tool/ledger path, not a claim about
// a live model choosing an optimal decomposition or TSB score improvement.
type d2EvidenceProvider struct {
	path   string
	direct bool
}

func (p d2EvidenceProvider) Complete(_ context.Context, request ModelRequest) (ModelResponse, error) {
	evidence := []string{}
	for _, m := range request.Messages {
		if m.Role == "tool" && strings.HasPrefix(m.ToolCallID, "d2-read") {
			var result ToolResult
			if err := json.Unmarshal([]byte(m.Content), &result); err != nil {
				return ModelResponse{}, err
			}
			if result.EvidenceID == "" {
				return ModelResponse{}, fmt.Errorf("read has no evidence")
			}
			evidence = append(evidence, result.EvidenceID)
		}
	}
	if len(evidence) == 0 {
		path := p.path
		for _, m := range request.Messages {
			if strings.HasPrefix(m.Content, "Worker task context (") {
				_, raw, _ := strings.Cut(m.Content, "\n")
				var task WorkerContext
				if err := json.Unmarshal([]byte(raw), &task); err != nil {
					return ModelResponse{}, err
				}
				if task.TaskID == "two" {
					path += ".two"
				}
			}
		}
		args, _ := json.Marshal(FileRequest{Path: path})
		calls := []ToolCall{{ID: "d2-read-1", Name: "read", Arguments: string(args)}}
		if p.direct {
			second, _ := json.Marshal(FileRequest{Path: p.path + ".two"})
			calls = append(calls, ToolCall{ID: "d2-read-2", Name: "read", Arguments: string(second)})
		}
		return ModelResponse{ToolCalls: calls}, nil
	}
	result := Result{Status: "success", EvidenceIDs: evidence, Answers: []Answer{{Value: "d2-proof", EvidenceIDs: evidence}}}
	raw, _ := json.Marshal(map[string]any{"trace_result": result})
	return ModelResponse{Content: fixtureReport(string(raw))}, nil
}

func TestD2DirectSerialParallelIndependentEvidence(t *testing.T) {
	for _, mode := range []string{"direct", "serial", "parallel"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proof.txt")
			if err := os.WriteFile(path, []byte("d2-proof"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path+".two", []byte("d2-proof-second-source"), 0600); err != nil {
				t.Fatal(err)
			}
			a, store := newTestAgent(t, d2EvidenceProvider{path: path, direct: mode == "direct"})
			a.SetRequireVerifiedCompletion(true)
			id := createTestRun(t, a)
			started := time.Now()
			runs := []Snapshot{}
			if mode == "direct" {
				s, err := a.Run(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				runs = append(runs, s)
			} else {
				tasks := []PlanTask{{ID: "one", Goal: "read independent source one"}, {ID: "two", Goal: "read independent source two"}}
				if mode == "serial" {
					tasks[1].Dependencies = []string{"one"}
				}
				if _, err := a.SetPlan(id, tasks); err != nil {
					t.Fatal(err)
				}
				for _, task := range tasks {
					if _, err := a.DispatchTask(context.Background(), id, task.ID); err != nil {
						t.Fatal(err)
					}
					if mode == "serial" {
						if _, err := waitAllTestWorkers(a, context.Background(), id, 5); err != nil {
							t.Fatal(err)
						}
					}
				}
				parent, err := waitAllTestWorkers(a, context.Background(), id, 5)
				if err != nil {
					t.Fatal(err)
				}
				for _, w := range parent.Workers {
					s, err := store.Load(w.RunID)
					if err != nil {
						t.Fatal(err)
					}
					runs = append(runs, s)
				}
			}
			requests, actions, tokens := 0, 0, 0
			for _, s := range runs {
				if s.Result.Status != "success" || s.Run.Status != StatusCompleted {
					t.Fatalf("%s: %s %+v", mode, s.Run.Status, s.Result)
				}
				for _, e := range s.Evidence {
					if _, ok := verifiedOutput(&s, e.ID); !ok {
						t.Fatal("unverified local evidence")
					}
				}
				requests += len(s.Requests)
				actions += len(s.Actions)
				for _, r := range s.Requests {
					tokens += EstimateContextTokens(r.Messages, nil)
				}
			}
			if actions != 2 || (mode == "direct" && requests != 2) || (mode != "direct" && requests != 4) {
				t.Fatalf("requests=%d actions=%d", requests, actions)
			}
			if len(runs) == 2 {
				if len(runs[0].Evidence) != 1 || len(runs[1].Evidence) != 1 {
					t.Fatal("source ledger copied into child")
				}
				first, second := runs[0].Evidence[0], runs[1].Evidence[0]
				if runs[0].Run.ID == runs[1].Run.ID || first.ContentHash == second.ContentHash || first.ContentHash != promptHash(first.Content) || second.ContentHash != promptHash(second.Content) {
					t.Fatal("fixture did not establish independent run identities and source hashes")
				}
				// Evidence IDs are Run-local. Looking up the same ID in another Run
				// correctly returns that Run's own proof, not the foreign record.
				for i := range runs {
					local := runs[i].Evidence[0]
					if output, ok := verifiedOutput(&runs[i], local.ID); !ok || !strings.Contains(output, "d2-proof") {
						t.Fatal("local proof rejected")
					}
					foreign := runs[1-i].Evidence[0]
					if verifyEvidence(&runs[i], &foreign) == "verified" {
						t.Fatal("foreign source record accepted against local ledger")
					}
					forged := runs[i]
					forged.Evidence = []Evidence{foreign}
					if _, ok := verifiedOutput(&forged, foreign.ID); ok {
						t.Fatal("foreign record replacement bypassed local ledger binding")
					}
				}
			}
			t.Logf("fixture only mode=%s requests=%d independent_reads=%d estimated_input_tokens=%d elapsed=%s", mode, requests, actions, tokens, time.Since(started))
		})
	}
}
