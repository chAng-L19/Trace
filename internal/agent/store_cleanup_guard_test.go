package agent

import (
	"strings"
	"testing"
)

func TestCleanupGuardPreservesOwnerLedger(t *testing.T) {
	for _, op := range []string{"hide", "evidence", "purge", "descendant", "descendant_hide"} {
		for _, unresolved := range []bool{false, true} {
			t.Run(op+map[bool]string{false: "_missing_result", true: "_cleanup_error"}[unresolved], func(t *testing.T) {
				a, store := newTestAgent(t, MockProvider{})
				root := createTestRun(t, a)
				owner := root
				if op == "descendant" || op == "descendant_hide" {
					owner = createTestRun(t, a)
					if _, err := store.Update(owner, func(s *Snapshot) error { s.Run.ParentRunID = root; s.Run.ParentTaskID = "task"; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				if _, err := store.Update(owner, func(s *Snapshot) error {
					s.Run.Status = StatusPaused
					action := Action{RequestID: "r", CallID: "c", Adapter: "exec", Status: "uncertain", ResourceOwner: "retained-owner"}
					if unresolved {
						action.Result = &ToolResult{WorkerCleanupError: "owner still populated"}
					}
					s.Actions = []Action{action}
					s.Evidence = []Evidence{{ID: "guard-evidence"}}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				var err error
				switch op {
				case "hide", "descendant_hide":
					err = store.Delete(root)
				case "evidence":
					_, err = store.DeleteEvidence(root, "guard-evidence")
				default:
					_, err = store.PurgeRun(root)
				}
				if err == nil || !strings.Contains(err.Error(), "工具资源清理尚未确认") {
					t.Fatal("cleanup gate did not preserve owner ledger", err)
				}
				snap, err := store.Load(owner)
				if err != nil || snap.Run.Deleted || len(snap.Actions) != 1 || snap.Actions[0].ResourceOwner != "retained-owner" {
					t.Fatalf("owner lost: %+v %v", snap.Run, err)
				}
				if _, err = store.Update(owner, func(s *Snapshot) error { s.Actions[0].ResourceCleanupStatus = "completed"; return nil }); err != nil {
					t.Fatal(err)
				}
				if op == "hide" || op == "descendant_hide" {
					err = store.Delete(root)
				} else if op == "evidence" {
					_, err = store.DeleteEvidence(root, "guard-evidence")
				} else {
					_, err = store.PurgeRun(root)
				}
				if err != nil {
					t.Fatal("durable cleanup did not release deletion guard", err)
				}
			})
		}
	}
}
