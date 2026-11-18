package agent

import (
	"fmt"
	"testing"
	"time"
)

func TestToolActivityBoundedAndObservational(t *testing.T) {
	now := time.Now()
	actions := []Action{{CallID: "older", Status: "completed", FinishedAt: now}, {CallID: "latest-terminal", Status: "interrupted", FinishedAt: now, Result: &ToolResult{WorkerOutcomeUncertain: true}}}
	for i := 0; i < 25; i++ {
		actions = append(actions, Action{CallID: fmt.Sprint(i), Name: "exec", Status: "started", StartedAt: now, TimeoutSeconds: 300})
	}
	rows, total := projectToolActivity(actions)
	if len(rows) != 20 || total != 26 || rows[0].TimeoutSeconds != 300 || rows[0].Status != "started" {
		t.Fatalf("projection %d %d %+v", len(rows), total, rows)
	}
	if actions[2].Status != "started" || !actions[2].FinishedAt.IsZero() {
		t.Fatal("projection mutated execution")
	}
	rows, total = projectToolActivity(actions[:2])
	if total != 1 || len(rows) != 1 || rows[0].CallID != "latest-terminal" || !rows[0].OutcomeUncertain {
		t.Fatalf("terminal diagnostic %+v", rows)
	}
}
