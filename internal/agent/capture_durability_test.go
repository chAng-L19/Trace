package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestActionRetainsCapturesUntilResultCommitAndNeverReplaysLostResult(t *testing.T) {
	for _, failure := range []string{"cas", "snapshot", "none"} {
		t.Run(failure, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			_, err := store.Update(id, func(s *Snapshot) error {
				s.Run.Status = StatusWaitingTool
				s.Requests = []ModelRequest{{ID: "request"}}
				s.Messages = []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "call", Name: "exec", Arguments: `{"command":"fixture"}`}}}}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			capture := filepath.Join(t.TempDir(), "capture")
			output := strings.Repeat("observed output\n", 2000)
			if err := os.WriteFile(capture, []byte(output), 0600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(store.root, id+".json")
			_, err = a.executeAction(context.Background(), id, "call", "exec", nil, func() ToolResult {
				if failure == "cas" {
					if err := os.WriteFile(filepath.Join(store.root, "artifacts"), []byte("block CAS directory"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				if failure == "snapshot" {
					if err := os.Rename(path, path+".saved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
				return ToolResult{StdoutPath: capture, ExitCode: 0}
			})
			if failure == "none" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(capture); !os.IsNotExist(err) {
					t.Fatal("committed capture not cleaned")
				}
				snap, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				got, err := artifactOutput(store.root, *snap.Actions[0].Result, "stdout")
				if err != nil || got != output {
					t.Fatal("committed capture bytes lost")
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), capture) {
				t.Fatalf("recovery path missing: %v", err)
			}
			got, err := os.ReadFile(capture)
			if err != nil || string(got) != output {
				t.Fatal("uncommitted output destroyed")
			}
			if failure == "snapshot" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(path+".saved", path); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			snap, err := a.executeAction(context.Background(), id, "call", "exec", nil, func() ToolResult { called = true; return ToolResult{} })
			if err != nil || called || snap.Run.Status != StatusCreated || len(snap.Actions) != 1 || snap.Actions[0].Status != "uncertain" || len(snap.Evidence) != 1 || snap.Evidence[0].Type != "runtime_observation" {
				t.Fatalf("lost result replayed: %+v %v", snap.Run, err)
			}
			if snap.Actions[0].Result != nil || toolCleanupError(snap) == nil {
				t.Fatal("lost durable result forged process cleanup proof")
			}
		})
	}
}
