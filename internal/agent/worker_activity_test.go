package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestWorkerActivitiesReadOnlyLifecycleProjection(t *testing.T) {
	a, parentID, childID := schedulingInbox(t, true, 0, 0)
	now := time.Now().UTC().Truncate(time.Second)
	parent, err := a.store.Update(parentID, func(s *Snapshot) error {
		s.WorkerMessages = []WorkerMessage{
			{ID: "open", From: "worker", To: "parent", Kind: "help", Delivered: true},
			{ID: "reply", From: "parent", To: "worker", Kind: "reply", ReplyTo: "open"},
			{ID: "closed", From: "parent", To: "worker", Kind: "help", Delivered: true},
			{ID: "resolved", From: "worker", To: "parent", Kind: "resolved", ReplyTo: "closed"},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		status   Status
		action   string
		response bool
		budget   int64
		phase    string
		tools    int
	}{
		{"unanswered", StatusRunning, "", false, 0, "model", 0},
		{"long-tool", StatusWaitingTool, "started", true, 100, "tool", 1},
		{"result-pending", StatusRunning, "result_ready", true, 0, "tool", 1},
		{"settled-tool", StatusRunning, "completed", true, 100, "running", 0},
		{"paused-request", StatusPaused, "", false, 0, "paused", 0},
		{"paused-tool", StatusPaused, "started", true, 100, "paused", 0},
		{"completed", StatusCompleted, "completed", true, 0, "completed", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.store.Update(childID, func(s *Snapshot) error {
				s.Run.Status = tc.status
				s.Run.Budget.MaxSeconds, s.Run.ElapsedMillis, s.Run.BudgetActiveSince = tc.budget, 7000, time.Time{}
				s.Requests = []ModelRequest{{ID: "old", CreatedAt: now.Add(-2 * time.Hour)}, {ID: "request", CreatedAt: now.Add(-time.Hour)}}
				s.Responses = []ResponseRecord{{RequestID: "old", CreatedAt: now.Add(-90 * time.Minute)}}
				if tc.response {
					s.Responses = append(s.Responses, ResponseRecord{RequestID: "request", CreatedAt: now.Add(-time.Hour)})
				}
				s.Actions = []Action{{Name: "previous", Status: "completed", FinishedAt: now.Add(-90 * time.Minute)}}
				if tc.action != "" {
					action := Action{Name: "long_tool", RequestID: "request", Status: tc.action, StartedAt: now.Add(-time.Hour)}
					if tc.action == "completed" {
						action.FinishedAt = now
					}
					s.Actions = append(s.Actions, action)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			before := activityFiles(t, a.store.root)
			rows := a.store.WorkerActivities(parent)
			if len(rows) != 1 {
				t.Fatalf("rows: %+v", rows)
			}
			row := rows[0]
			if !row.Available || row.RunStatus != tc.status || row.Phase != tc.phase || len(row.Tools) != tc.tools || row.CleanupStatus != "completed" || row.PendingMessages != 1 || !reflect.DeepEqual(row.OpenHelpIDs, []string{"open"}) {
				t.Fatalf("incorrect activity: %+v", row)
			}
			if tc.phase == "model" && row.RequestID != "request" {
				t.Fatal("missing in-flight request identity")
			}
			if tc.budget == 0 && row.BudgetRemainingSeconds != nil || tc.budget > 0 && (row.BudgetRemainingSeconds == nil || *row.BudgetRemainingSeconds != 93) {
				t.Fatalf("wrong explicit budget: %+v", row)
			}
			wantModel, wantTool := now.Add(-90*time.Minute), now.Add(-90*time.Minute)
			if tc.response {
				wantModel = now.Add(-time.Hour)
			}
			if tc.action == "completed" {
				wantTool = now
			}
			if !row.LastModelResponse.Equal(wantModel) || !row.LastToolResult.Equal(wantTool) {
				t.Fatalf("invented/stale settlement timestamps: %+v", row)
			}
			_ = a.store.WorkerActivities(parent)
			after := activityFiles(t, a.store.root)
			if !reflect.DeepEqual(before, after) {
				t.Fatal("read-only activity changed persisted files")
			}
		})
	}
}

func TestWorkerActivitiesUnavailableAndParentIdentity(t *testing.T) {
	a, parentID, childID := schedulingInbox(t, true, 0, 0)
	parent, _ := a.store.Load(parentID)
	parent.Workers = append(parent.Workers, AgentWorker{ID: "missing", RunID: "missing", TaskID: "task"})
	for _, tc := range []struct {
		parent, task string
		deleted      bool
	}{
		{"wrong-parent", "task", false}, {parentID, "wrong-task", false}, {parentID, "task", true},
	} {
		if _, err := a.store.Update(childID, func(s *Snapshot) error {
			s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Deleted = tc.parent, tc.task, tc.deleted
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, row := range a.store.WorkerActivities(parent) {
			if row.Available || row.Phase != "unavailable" || row.BudgetRemainingSeconds != nil {
				t.Fatalf("invalid child treated as available: %+v", row)
			}
		}
	}
}

// Check every file, not just the JSON snapshot: a board read must not append
// heartbeat/event ledgers or materialize a second help-state file.
func activityFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[path] = string(bytes.Clone(data))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
