package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"trace/internal/localtool"
)

func TestPlannerLeaseDependencyAndUnknownRecovery(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	tasks := []PlanTask{{ID: "a", Goal: "observe"}, {ID: "b", Goal: "verify", Dependencies: []string{"a"}}}
	s, err := a.SetPlan(id, tasks)
	if err != nil || len(s.Plan.Frontier) != 1 || s.Plan.Frontier[0] != "a" {
		t.Fatalf("frontier %+v %v", s.Plan, err)
	}
	if _, err = a.ClaimTask(id, "b", "worker", 30); err == nil {
		t.Fatal("claimed blocked dependency")
	}
	tasks[0].Dependencies = []string{"b"}
	if _, err = a.SetPlan(id, tasks); err == nil {
		t.Fatal("accepted cycle")
	}
	if _, err = a.ClaimTask(id, "a", "worker", 30); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(id, "a", "other", 30); err == nil {
		t.Fatal("duplicate claim")
	}
	if _, err = a.CompleteTask(id, "a", "worker", "verified", nil); err == nil {
		t.Fatal("business verification fabricated")
	}
	store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[0].LeaseUntil = time.Now().Add(-time.Second); return nil })
	s, err = a.RecoverWorkers(id)
	if err != nil || s.Plan.Tasks[0].Status != "uncertain" || len(s.Plan.Frontier) != 0 {
		t.Fatalf("recover %+v %v", s.Plan, err)
	}
	if _, err = a.ClaimTask(id, "a", "worker", 30); err == nil {
		t.Fatal("uncertain task replayed")
	}
}

func TestFilesArtifactsAndRealBuiltinChain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data.txt")
	a, store := newTestAgent(t, MockProvider{})
	body := strings.Repeat("raw-content\n", 4000)
	if _, err := fileOperation("write", FileRequest{Path: path, Content: body}); err != nil {
		t.Fatal(err)
	}
	if _, err := fileOperation("edit", FileRequest{Path: path, OldText: "raw-content", NewText: "changed"}); err == nil {
		t.Fatal("ambiguous edit accepted")
	}
	if _, err := fileOperation("write", FileRequest{Path: path, Content: "lost", ExpectedHash: "stale"}); err == nil {
		t.Fatal("stale write accepted")
	}
	data, _ := os.ReadFile(path)
	if string(data) != body {
		t.Fatal("failed edit changed file")
	}
	ref, err := store.PutArtifact("stdout", data)
	if err != nil {
		t.Fatal(err)
	}
	rangeOut, err := store.ReadArtifact(ref.Hash, 13, 31)
	if err != nil || rangeOut.Content != body[13:44] || rangeOut.Bytes != int64(len(body)) {
		t.Fatalf("range %+v %v", rangeOut, err)
	}
	if _, err = store.ReadArtifact("../escape", 0, 10); err == nil {
		t.Fatal("accepted bad ref")
	}
	os.WriteFile(filepath.Join(store.root, "artifacts", ref.Hash), []byte("corrupt"), 0600)
	if _, err = store.ReadArtifact(ref.Hash, 0, 10); err == nil {
		t.Fatal("accepted corrupted artifact")
	}
	args, _ := json.Marshal(FileRequest{Path: path, Offset: 0, Limit: 64000})
	a.provider = manualProvider(ToolCall{ID: "read-file", Name: "read", Arguments: string(args)})
	id := createTestRun(t, a)
	s, err := a.Run(context.Background(), id)
	if err != nil || len(s.Actions) != 1 || len(s.Actions[0].Result.Artifacts) != 1 || s.Evidence[0].Verification != "verified" {
		t.Fatalf("builtin chain: %+v %v", s.Actions, err)
	}
	output, valid := verifiedOutput(&s, s.Evidence[0].ID)
	if !valid || len(output) < len(body) {
		t.Fatal("raw evidence truncated")
	}
}

func TestChildAgentRealToolEvidenceAndMessage(t *testing.T) {
	manifest := localManifest(t, "echo")
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "child-call", Name: manifest.StableID(), Arguments: `{"message":"child-proof"}`}))
	if err := a.AddLocalTools([]localtool.Manifest{manifest}); err != nil {
		t.Fatal(err)
	}
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "task", Goal: "produce child-proof", SuccessContains: "child-proof"}, {ID: "next", Goal: "next", Dependencies: []string{"task"}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	w, err := a.DispatchTask(ctx, id, "task")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SendWorkerMessage(id, "parent", w.ID, "additional observation"); err != nil {
		t.Fatal(err)
	}
	var s Snapshot
	for ctx.Err() == nil {
		s, err = store.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		if len(s.Workers) > 0 && s.Workers[0].Status != "running" && len(s.WorkerMessages) > 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if s.Plan.Tasks[0].Status != "verified" || s.Workers[0].CleanupStatus != "completed" || len(s.Plan.Frontier) != 1 {
		t.Fatalf("worker chain %+v %+v", s.Plan, s.Workers)
	}
	child, err := store.Load(w.RunID)
	if err != nil || len(child.Actions) != 1 || child.Actions[0].Result.WorkerPID == 0 || child.Evidence[0].Verification != "verified" {
		t.Fatalf("child evidence %+v %v", child.Actions, err)
	}
	if child.Run.ParentRunID != id {
		t.Fatal("child ancestry lost")
	}
	if err = a.DeliverWorkerMessages(id); err != nil {
		t.Fatal(err)
	}
	s, _ = store.Load(id)
	count := len(s.Messages)
	if err = a.DeliverWorkerMessages(id); err != nil {
		t.Fatal(err)
	}
	s, _ = store.Load(id)
	if len(s.Messages) != count {
		t.Fatal("worker message delivered twice")
	}
	if _, err = a.UpsertAsset(id, Asset{ID: "x", Kind: "file", Value: "x", Coverage: "verified", Predicate: "child-proof", EvidenceIDs: []string{child.Evidence[0].ID}}); err == nil {
		t.Fatal("child evidence silently promoted into parent ledger")
	}
}

func TestFailedChildDoesNotUnlockFrontier(t *testing.T) {
	manifest := localManifest(t, "fail")
	a, store := newTestAgent(t, manualProvider(ToolCall{ID: "fail", Name: manifest.StableID(), Arguments: `{}`}))
	a.AddLocalTools([]localtool.Manifest{manifest})
	id := createTestRun(t, a)
	a.SetPlan(id, []PlanTask{{ID: "x", Goal: "fail", SuccessContains: "impossible"}, {ID: "y", Goal: "blocked", Dependencies: []string{"x"}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := a.DispatchTask(ctx, id, "x")
	if err != nil {
		t.Fatal(err)
	}
	for ctx.Err() == nil {
		s, _ := store.Load(id)
		if s.Workers[0].Status != "running" {
			if s.Plan.Tasks[0].Status != "failed" || len(s.Plan.Frontier) != 0 {
				t.Fatal("failed predicate unlocked dependency frontier")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("child did not finish")
}
