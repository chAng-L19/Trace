package agent

// Opt-in server check using an isolated copy of large persisted histories.
// It only reads/writes the copy; no model, worker command or tool is started.
import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"testing"
)

func TestControllerLargeSnapshotMemoryBudget(t *testing.T) {
	root := os.Getenv("TRACE_CONTROLLER_FIXTURE_DIR")
	if root == "" {
		t.Skip("server-only isolated large-state copy required")
	}
	if debug.SetMemoryLimit(-1) != 536870912 {
		t.Fatal("expected default 512MiB Go target inside the unchanged 1GiB controller")
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	files, err := filepath.Glob(filepath.Join(root, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{}
	for _, file := range files {
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		if info.Size() >= 30<<20 {
			ids = append(ids, strings.TrimSuffix(filepath.Base(file), ".json"))
		}
	}
	if len(ids) != 3 {
		t.Fatalf("expected three large source snapshots, got %d", len(ids))
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer store.ReleaseEventView(id)
			before, err := store.Load(id)
			if err != nil {
				t.Error(err)
				return
			}
			original := valueHash([]any{before.Actions, before.Evidence, before.Requests, before.Responses, before.Messages, before.Run.Status, before.Run.PauseReason})
			through := before.Run.EventSeq
			before = Snapshot{}
			for i := 0; i < 3; i++ {
				if _, _, err := store.EventBatch(id, through); err != nil {
					t.Error(err)
					return
				}
				if err := store.AppendEvents(id, []Event{{Type: "fixture_pressure", Summary: "isolated memory check"}}); err != nil {
					t.Error(err)
					return
				}
				after, err := store.Update(id, func(s *Snapshot) error { return nil })
				if err != nil {
					t.Error(err)
					return
				}
				if original != valueHash([]any{after.Actions, after.Evidence, after.Requests, after.Responses, after.Messages, after.Run.Status, after.Run.PauseReason}) {
					t.Error("memory check changed execution records or resumed the copy")
					return
				}
				through = after.Run.EventSeq
			}
		}(id)
	}
	wg.Wait()
	t.Log("three large histories: concurrent read, telemetry append, semantic save; execution records retained")
}
