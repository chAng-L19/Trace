package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func round1FileCall(t *testing.T, id, name, path, content string) ToolCall {
	t.Helper()
	args, err := json.Marshal(map[string]string{"path": path, "content": content})
	if err != nil {
		t.Fatal(err)
	}
	return ToolCall{ID: id, Name: name, Arguments: string(args)}
}

// Call IDs are scoped to a response. A later response may reuse them in a
// different order, and a read following a write must see that write.
func TestRuntimeRound1ReusedCallIDsPreserveCurrentBatchOrder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ordered.txt")
	if err := os.WriteFile(path, []byte("before"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &scriptedProvider{responses: []ModelResponse{
		{ToolCalls: []ToolCall{
			round1FileCall(t, "a", "read", path, ""),
			round1FileCall(t, "b", "read", path, ""),
		}},
		{ToolCalls: []ToolCall{
			round1FileCall(t, "b", "write", path, "after"),
			round1FileCall(t, "a", "read", path, ""),
		}},
		{Content: "done"},
	}}
	a, _ := newTestAgent(t, p)
	s, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil {
		t.Fatal(err)
	}
	if s.Run.Status != StatusCompleted || len(s.Actions) != 4 {
		t.Fatalf("unexpected completion: status=%s actions=%d", s.Run.Status, len(s.Actions))
	}
	var content string
	for _, action := range s.Actions {
		if action.RequestID != s.Requests[1].ID || action.CallID != "a" {
			continue
		}
		if action.Result == nil || action.Result.Error != "" {
			t.Fatalf("second batch read failed: %+v", action)
		}
		var page struct{ Content string }
		if err := json.Unmarshal([]byte(action.Result.Stdout), &page); err != nil {
			t.Fatal(err)
		}
		content = page.Content
	}
	if content != "after" {
		t.Fatalf("current batch read observed %q; declared write-before-read order requires after", content)
	}
	if s.Actions[2].CallID != "b" || s.Actions[3].CallID != "a" {
		t.Fatalf("current batch executed in historical order: %s, %s", s.Actions[2].CallID, s.Actions[3].CallID)
	}
}

// Reading the contract back from a bookkeeping tool is not observation of
// the target. The expected marker appears in this output only as a predicate.
func TestRuntimeRound1RequirementEchoIsNotGoalEvidence(t *testing.T) {
	a, _ := newTestAgent(t, manualProvider(
		ToolCall{ID: "status", Name: "verification_status", Arguments: `{}`},
		ToolCall{ID: "hold", Name: "external", Arguments: `{}`},
	))
	id := createTestRun(t, a)
	r := VerificationRequirement{ID: "target", Description: "observe the target marker", Category: "goal", Predicate: EvidencePredicate{Kind: "contains", Value: "target-only-marker"}}
	if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
		t.Fatal(err)
	}
	waitTestRun(t, a, id)
	s, err := a.executePendingTool(context.Background(), id, ToolCall{ID: "status", Name: "verification_status", Arguments: `{}`})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Actions) != 1 || s.Actions[0].Name != "verification_status" || len(s.Evidence) != 1 {
		t.Fatalf("unexpected fixture actions/evidence: %d/%d", len(s.Actions), len(s.Evidence))
	}
	s, err = a.CheckEvidence(context.Background(), id, r.ID, []string{s.Evidence[0].ID})
	if err == nil && latestCheck(&s, r.ID).Status == "pass" {
		t.Fatal("reading the acceptance contract alone passed its target predicate")
	}
}

// A single corrupt action-routing field must not turn a different file read
// into proof of the fixed probe. The model request/response remain unchanged.
func TestRuntimeRound1ActionRouteTamperCannotSatisfyFixedProbe(t *testing.T) {
	for _, mutation := range []string{"name", "arguments"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			actual := filepath.Join(root, "actual.txt")
			wanted := filepath.Join(root, "unread-target.txt")
			if err := os.WriteFile(actual, []byte("observed-marker"), 0600); err != nil {
				t.Fatal(err)
			}
			call := round1FileCall(t, "probe", "read", actual, "")
			requiredCall := call
			if mutation == "name" {
				requiredCall.Name = "artifact_read"
			} else {
				requiredCall = round1FileCall(t, "probe", "read", wanted, "")
			}
			a, store := newTestAgent(t, manualProvider(call, ToolCall{ID: "hold", Name: "external", Arguments: `{}`}))
			id := createTestRun(t, a)
			r := VerificationRequirement{ID: "target", Description: "the exact configured probe must observe the marker", Category: "goal", ToolID: requiredCall.Name, ArgumentsHash: promptHash(requiredCall.Arguments), Predicate: EvidencePredicate{Kind: "contains", Value: "observed-marker"}}
			if _, err := a.SetVerificationRequirements(id, []VerificationRequirement{r}); err != nil {
				t.Fatal(err)
			}
			waitTestRun(t, a, id)
			s, err := a.executePendingTool(context.Background(), id, call)
			if err != nil || len(s.Evidence) != 1 {
				t.Fatalf("read fixture: evidence=%d err=%v", len(s.Evidence), err)
			}
			evidenceID := s.Evidence[0].ID
			s, err = a.CheckEvidence(context.Background(), id, r.ID, []string{evidenceID})
			if err != nil || latestCheck(&s, r.ID).Status == "pass" {
				t.Fatalf("unmodified different probe must not pass: err=%v", err)
			}
			_, err = store.Update(id, func(s *Snapshot) error {
				if mutation == "name" {
					s.Actions[0].Name = requiredCall.Name
				} else {
					s.Actions[0].Arguments = requiredCall.Arguments
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			s, err = a.CheckEvidence(context.Background(), id, r.ID, []string{evidenceID})
			if err == nil && latestCheck(&s, r.ID).Status == "pass" {
				t.Fatalf("changing only action %s made an unexecuted fixed probe pass", mutation)
			}
		})
	}
}

// One failed read must remain unverified without hiding the successful sibling
// action, and both results must reach the next model request.
func TestRuntimeRound1PartialToolFailurePreservesSiblingEvidence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "present.txt")
	if err := os.WriteFile(path, []byte("real-observation"), 0600); err != nil {
		t.Fatal(err)
	}
	p := manualProvider(
		round1FileCall(t, "missing", "read", filepath.Join(root, "missing.txt"), ""),
		round1FileCall(t, "present", "read", path, ""),
	)
	a, _ := newTestAgent(t, p)
	s, err := a.Run(context.Background(), createTestRun(t, a))
	if err != nil || s.Run.Status != StatusCompleted || len(s.Actions) != 2 || len(s.Evidence) != 2 || len(p.requests) != 2 {
		t.Fatalf("partial batch lost state: status=%s actions=%d evidence=%d requests=%d err=%v", s.Run.Status, len(s.Actions), len(s.Evidence), len(p.requests), err)
	}
	if s.Evidence[0].Verification == "verified" || s.Evidence[1].Verification != "verified" || s.Result.Status == "success" {
		t.Fatalf("failed/successful evidence or ordinary final reply misclassified: %+v %+v", s.Evidence, s.Result)
	}
	results := map[string]bool{}
	for _, m := range p.requests[1].Messages {
		if m.Role == "tool" {
			results[m.ToolCallID] = true
		}
	}
	if !results["missing"] || !results["present"] {
		t.Fatal("next model request did not receive both failed and successful tool results")
	}
}
