package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func sqliteTestStore(t *testing.T) *Store {
	t.Helper()
	root := t.TempDir()
	if err := MigrateStoreToSQLite(root); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSQLiteMigrationAuthorityAndExport(t *testing.T) {
	root := t.TempDir()
	old, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := old.Create("migration", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	if err = old.AppendEvents(snap.Run.ID, []Event{{Type: "model_stream", Data: map[string]any{"delta": "before"}}}); err != nil {
		t.Fatal(err)
	}
	before, err := old.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = MigrateStoreToSQLite(root); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Update(snap.Run.ID, func(s *Snapshot) error { s.Run.Goal = "stale writer"; return nil }); err == nil {
		t.Fatal("stale JSON writer accepted")
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	after, err := s.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("migration changed snapshot")
	}
	if _, err = os.Stat(filepath.Join(root, snap.Run.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("live JSON remains")
	}
	if err = s.AppendEvents(snap.Run.ID, []Event{{Type: "model_stream", Data: "after"}}); err != nil {
		t.Fatal(err)
	}
	exported := filepath.Join(t.TempDir(), "export")
	if err = s.ExportSQLiteSnapshots(exported); err != nil {
		t.Fatal(err)
	}
	restored, err := NewStore(exported)
	if err != nil {
		t.Fatal(err)
	}
	current, err := restored.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(current.Events) != 2 {
		t.Fatal("export lost post-migration commit")
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(root, sqliteStoreName), filepath.Join(root, "damaged-db")); err != nil {
		t.Fatal(err)
	}
	if _, err = NewStore(root); err == nil {
		t.Fatal("missing DB silently fell back to JSON")
	}
}

func TestSQLiteMigrationRecoversAfterDurableMarker(t *testing.T) {
	root := t.TempDir()
	old, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := old.Create("pending migration", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("stop after authority marker")
	if err = migrateStoreToSQLite(root, func() error { return injected }); !errors.Is(err, injected) {
		t.Fatalf("injection: %v", err)
	}
	if _, err = old.Create("stale", "", ""); err == nil {
		t.Fatal("old Store wrote during pending migration")
	}
	s, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.sqlite {
		t.Fatal("recovery fell back to JSON")
	}
	got, err := s.Load(snap.Run.ID)
	if err != nil || got.Run.Goal != "pending migration" {
		t.Fatalf("recovery: %v", err)
	}
}

func TestSQLiteAtomicEventsStateReceiptsAndNotification(t *testing.T) {
	s := sqliteTestStore(t)
	snap, err := s.Create("atomic", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	// Load may durably initialize/revoke derived Judge state. Settle that valid
	// commit before subscribing to the transaction whose rollback is under test.
	if _, err = s.Load(snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	changed := s.EventChanges(snap.Run.ID)
	_, err = s.Update(snap.Run.ID, func(s *Snapshot) error {
		s.Run.Goal = "must rollback"
		s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: "receipt"})
		addEvent(s, "worker_message", "receipt", nil)
		s.Events = append(s.Events, s.Events[len(s.Events)-1])
		return nil
	})
	if err == nil {
		t.Fatal("duplicate sequence accepted")
	}
	select {
	case <-changed:
		t.Fatal("failed transaction notified")
	default:
	}
	got, err := s.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Run.Goal != "atomic" || len(got.Events) != 0 || len(got.WorkerMessages) != 0 {
		t.Fatal("partial state/receipt/event commit")
	}
	_, err = s.Update(snap.Run.ID, func(s *Snapshot) error {
		s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: "receipt"})
		addEvent(s, "worker_message", "receipt", nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("commit did not notify")
	}
	reopened, err := NewStore(s.root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, err = reopened.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.WorkerMessages) != 1 || len(got.Events) != 1 {
		t.Fatal("receipt/event not durable")
	}
}

func TestSQLiteFiveWritersCursorAndEventDeletion(t *testing.T) {
	s := sqliteTestStore(t)
	snap, err := s.Create("concurrent", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 5)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			writer, e := NewStore(s.root)
			if e != nil {
				failures <- e
				return
			}
			defer writer.Close()
			for n := 0; n < 8; n++ {
				if e = writer.AppendEvents(snap.Run.ID, []Event{{Type: "model_stream", Data: fmt.Sprint(n)}}); e != nil {
					failures <- e
					return
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	for e := range failures {
		t.Fatal(e)
	}
	run, events, err := s.EventBatch(snap.Run.ID, 35)
	if err != nil {
		t.Fatal(err)
	}
	if run.EventSeq != 40 || len(events) != 5 {
		t.Fatalf("cursor=%d len=%d", run.EventSeq, len(events))
	}
	for i, raw := range events {
		var event Event
		if err = json.Unmarshal(raw, &event); err != nil {
			t.Fatal(err)
		}
		if event.Sequence != 36+i {
			t.Fatal("cursor sequence gap")
		}
	}
	_, err = s.Update(snap.Run.ID, func(s *Snapshot) error { s.Events = s.Events[1:]; s.Events[0].Summary = "redacted"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Events) != 39 || got.Events[0].Sequence != 2 || got.Events[0].Summary != "redacted" {
		t.Fatal("event edit/delete lost")
	}
	observation, err := s.Observe(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observation.Run, got.Run) {
		t.Fatal("observation disagrees with committed run")
	}
	if _, err = s.PurgeRun(snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(snap.Run.ID); !os.IsNotExist(err) {
		t.Fatalf("purge result: %v", err)
	}
}

func TestSQLiteWorkerOperationRetryAndBudget(t *testing.T) {
	a, parent, _ := schedulingInbox(t, true, 0, 0)
	if err := MigrateStoreToSQLite(a.store.root); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(a.store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a.store = store
	_, first, _, err := a.SendWorkerMessageOperation(parent, "parent", "worker", "payload", "help", "", "sqlite-lost-ack")
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	b, err := New(other, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	_, second, _, err := b.SendWorkerMessageOperation(parent, "parent", "worker", "payload", "help", "", "sqlite-lost-ack")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.RequestHash != second.RequestHash {
		t.Fatal("durable receipt was not reused")
	}
	if _, _, _, err = b.SendWorkerMessageOperation(parent, "parent", "worker", "changed", "help", "", "sqlite-lost-ack"); err == nil {
		t.Fatal("operation collision accepted")
	}
	root, err := a.Create("tree budget")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetTreeBudget(root.Run.ID, TreeBudget{MaxWorkers: 1, MaxDepth: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.SetPlan(root.Run.ID, []PlanTask{{ID: "one", Goal: "one"}, {ID: "two", Goal: "two"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = a.ClaimTask(root.Run.ID, "one", "worker-one", 30); err != nil {
		t.Fatal(err)
	}
	if _, err = b.ClaimTask(root.Run.ID, "two", "worker-two", 30); err == nil {
		t.Fatal("cross-Store tree budget exceeded")
	}
}

func TestSQLiteUncertainNeverReexecutes(t *testing.T) {
	p := manualProvider(ToolCall{ID: "one", Name: "external", Arguments: "{}"})
	a, old := newTestAgent(t, p)
	if err := MigrateStoreToSQLite(old.root); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(old.root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a.store = s
	id := createTestRun(t, a)
	waitTestRun(t, a, id)
	if _, err = s.Update(id, func(s *Snapshot) error {
		s.Actions = append(s.Actions, Action{RequestID: currentRequestID(s), CallID: "one", Name: "external", Status: "started", StartedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	snap, err := a.ExecuteTool(context.Background(), id, "one", []string{"nonexistent-command-must-not-run"})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != StatusCompleted || snap.Actions[0].Status != "uncertain" || len(snap.Evidence) != 1 || snap.Evidence[0].Verification == "verified" {
		t.Fatal("uncertain action replay boundary lost")
	}
}

func TestSQLiteCrashTransactionBoundary(t *testing.T) {
	if root := os.Getenv("TRACE_SQLITE_CRASH_ROOT"); root != "" {
		s, err := NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		if os.Getenv("TRACE_SQLITE_CRASH_PHASE") == "before" {
			s.beforeSQLiteCommit = func() { os.Exit(71) }
		}
		_, err = s.Update(os.Getenv("TRACE_SQLITE_CRASH_ID"), func(s *Snapshot) error {
			s.Run.Goal = "committed"
			s.WorkerMessages = append(s.WorkerMessages, WorkerMessage{ID: "crash-receipt"})
			addEvent(s, "worker_message", "crash", nil)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		os.Exit(72)
	}
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			s := sqliteTestStore(t)
			snap, err := s.Create("original", "prompt", "hash")
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestSQLiteCrashTransactionBoundary$")
			cmd.Env = append(os.Environ(), "TRACE_SQLITE_CRASH_ROOT="+s.root, "TRACE_SQLITE_CRASH_ID="+snap.Run.ID, "TRACE_SQLITE_CRASH_PHASE="+phase)
			output, err := cmd.CombinedOutput()
			var exited *exec.ExitError
			want := 71
			if phase == "after" {
				want = 72
			}
			if !errors.As(err, &exited) || exited.ExitCode() != want {
				t.Fatalf("child %v: %s", err, output)
			}
			got, err := s.Load(snap.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if phase == "before" {
				if got.Run.Goal != "original" || len(got.Events) != 0 || len(got.WorkerMessages) != 0 {
					t.Fatal("uncommitted crash published")
				}
			} else {
				if got.Run.Goal != "committed" || len(got.Events) != 1 || len(got.WorkerMessages) != 1 {
					t.Fatal("committed crash lost receipt/state/event")
				}
			}
		})
	}
}

func TestSQLiteEvidenceSourceRevocationAndPurge(t *testing.T) {
	_, old, snap, ref := catalogFixture(t)
	if err := MigrateStoreToSQLite(old.root); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(old.root)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Evidence[0].Verification != "verified" {
		t.Fatal("migration lost valid source")
	}
	if err = os.Remove(filepath.Join(s.root, "artifacts", ref.Hash)); err != nil {
		t.Fatal(err)
	}
	got, err = s.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Evidence[0].Verification == "verified" {
		t.Fatal("missing CAS remained verified")
	}
	if _, err = s.PurgeRun(snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Load(snap.Run.ID); !os.IsNotExist(err) {
		t.Fatalf("purged source: %v", err)
	}
}

func TestSQLiteMigrationRejectsNonMonotonicHistoryWithoutMutation(t *testing.T) {
	for _, sequences := range [][]int{{2, 1}, {1, 1}, {0}, {3}} {
		t.Run(fmt.Sprint(sequences), func(t *testing.T) {
			root := t.TempDir()
			s, err := NewStore(root)
			if err != nil {
				t.Fatal(err)
			}
			snap, err := s.Create("legacy order", "prompt", "hash")
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.Update(snap.Run.ID, func(s *Snapshot) error {
				s.Run.EventSeq = 2
				for _, sequence := range sequences {
					s.Events = append(s.Events, Event{Sequence: sequence, Type: "legacy"})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, snap.Run.ID+".json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err = MigrateStoreToSQLite(root); err == nil {
				t.Fatal("incompatible history accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(before) != string(after) {
				t.Fatal("rejected migration changed JSON")
			}
			for _, name := range []string{sqliteStoreMarker, sqliteStoreName, "sqlite-migration-json-backup"} {
				if _, err = os.Stat(filepath.Join(root, name)); !os.IsNotExist(err) {
					t.Fatalf("rejected migration created %s", name)
				}
			}
		})
	}
}
