package agent

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func workerActivityHistory(t testing.TB) (*Store, Snapshot) {
	t.Helper()
	a, parentID, _ := schedulingInbox(t, true, 0, 0)
	parent, err := a.store.Load(parentID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		child, err := a.Create("completed history")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.store.Update(child.Run.ID, func(s *Snapshot) error {
			s.Run.ParentRunID, s.Run.ParentTaskID, s.Run.Status = parentID, fmt.Sprint(i), StatusCompleted
			s.Responses = []ResponseRecord{{RequestID: "completed", CreatedAt: time.Now().UTC()}}
			for j := 0; j < 1024; j++ {
				s.Events = append(s.Events, Event{Type: "model_stream", Sequence: j + 1, Data: strings.Repeat("x", 1024)})
			}
			s.Run.EventSeq = len(s.Events)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		parent.Workers = append(parent.Workers, AgentWorker{ID: fmt.Sprint(i), RunID: child.Run.ID, TaskID: fmt.Sprint(i), Status: "completed", CleanupStatus: "completed"})
	}
	return a.store, parent
}

func TestWorkerActivitiesCacheLargeHistoriesAndConcurrentReads(t *testing.T) {
	store, parent := workerActivityHistory(t)
	const readers = 8
	var wg sync.WaitGroup
	results := make(chan []*workerActivityCache, readers)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows := store.WorkerActivities(parent)
			if len(rows) != len(parent.Workers) {
				t.Error("missing worker rows")
			}
			entries := []*workerActivityCache{}
			for _, w := range parent.Workers {
				entry, err := store.workerActivity(w.RunID)
				if err != nil {
					t.Error(err)
					return
				}
				entries = append(entries, entry)
			}
			results <- entries
		}()
	}
	wg.Wait()
	close(results)
	var original []*workerActivityCache
	for entries := range results {
		if original == nil {
			original = entries
			continue
		}
		for i, entry := range entries {
			if original[i] != entry {
				t.Fatal("concurrent/cached board reread immutable history")
			}
		}
	}
	if len(original) != len(parent.Workers) {
		t.Fatal("missing cache results")
	}
	for i, w := range parent.Workers {
		if _, retained := store.eventViews.Load(w.RunID); retained || store.eventReaders[w.RunID] != 0 {
			t.Fatal("activity retained full event view without SSE reader")
		}
		entry, err := store.workerActivity(w.RunID)
		if err != nil || entry != original[i] {
			t.Fatal("unchanged generation did not reuse compact entry")
		}
	}
}

func TestWorkerActivityCacheInvalidationAndDetachedParentProjection(t *testing.T) {
	a, parentID, childID := schedulingInbox(t, true, 0, 0)
	if _, err := a.store.Update(childID, func(s *Snapshot) error {
		s.Run.Status = StatusWaitingTool
		s.Run.Budget.MaxSeconds = 60
		s.Run.BudgetActiveSince = time.Now().UTC().Add(-5 * time.Second)
		s.Actions = []Action{{Name: "tool", Status: "started"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	parent, _ := a.store.Load(parentID)
	first := a.store.WorkerActivities(parent)[0]
	entry, _ := a.store.workerActivity(childID)
	first.Tools[0] = "caller mutation"
	parent.Workers[0].CleanupStatus = "pending"
	parent.WorkerMessages = append(parent.WorkerMessages, WorkerMessage{ID: "help", Kind: "help", From: "parent", To: "worker"})
	second := a.store.WorkerActivities(parent)[0]
	if second.Tools[0] != "tool" || second.CleanupStatus != "pending" || second.PendingMessages != 1 || len(second.OpenHelpIDs) != 1 {
		t.Fatalf("cached parent fields/shared slices: %+v", second)
	}
	time.Sleep(1100 * time.Millisecond)
	third := a.store.WorkerActivities(parent)[0]
	if third.BudgetRemainingSeconds == nil || *third.BudgetRemainingSeconds >= *second.BudgetRemainingSeconds {
		t.Fatal("remaining budget froze with cache")
	}
	parent.Run.ID = "wrong-parent"
	if row := a.store.WorkerActivities(parent)[0]; row.Available {
		t.Fatal("cache bypassed current parent identity")
	}
	parent.Run.ID = parentID
	external, err := NewStore(a.store.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := external.Update(childID, func(s *Snapshot) error { s.Run.Status, s.Run.BudgetActiveSince = StatusPaused, time.Time{}; return nil }); err != nil {
		t.Fatal(err)
	}
	if row := a.store.WorkerActivities(parent)[0]; row.Phase != "paused" || len(row.Tools) != 0 {
		t.Fatalf("external generation not reflected: %+v", row)
	}
	current, _ := a.store.workerActivity(childID)
	if current == entry {
		t.Fatal("external atomic replacement did not invalidate summary")
	}
}

func TestWorkerActivityCacheIsBounded(t *testing.T) {
	a, _, childID := schedulingInbox(t, true, 0, 0)
	a.store.workerActivities = map[string]*workerActivityCache{}
	for i := 0; i < workerActivityCacheLimit; i++ {
		a.store.workerActivities[fmt.Sprint(i)] = &workerActivityCache{}
	}
	if _, err := a.store.workerActivity(childID); err != nil {
		t.Fatal(err)
	}
	if len(a.store.workerActivities) != workerActivityCacheLimit || a.store.workerActivities[childID] == nil {
		t.Fatal("activity summaries grew beyond bound or evicted newest")
	}
}

func BenchmarkWorkerActivitiesCachedLargeHistories(b *testing.B) {
	store, parent := workerActivityHistory(b)
	store.WorkerActivities(parent)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		store.WorkerActivities(parent)
	}
}
