package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCapacityProjectionMatchesAdmission(t *testing.T) {
	cases := []struct {
		name string
		edit func(*Snapshot, *Snapshot)
		want int
	}{
		{"retained_local", func(_, _ *Snapshot) {}, 1},
		{"live_local", func(p, c *Snapshot) {
			p.Workers[0].Status, p.Workers[0].CleanupStatus, p.Plan.Tasks[0].Status = "running", "pending", "running"
			c.Run.Status = StatusRunning
		}, 1},
		{"handoff", func(p, _ *Snapshot) {
			p.Workers[0].Status, p.Workers[0].CleanupStatus, p.Plan.Tasks[0].Status = "running", "pending", "running"
			p.Workers[0].Visit = 2
		}, 1},
		{"uncertain_action", func(_, c *Snapshot) { c.Actions = []Action{{Status: "uncertain"}} }, 2},
		{"started_action", func(_, c *Snapshot) { c.Actions = []Action{{Status: "started"}} }, 2},
		{"exec_without_result", func(_, c *Snapshot) { c.Actions = []Action{{Adapter: "exec", Status: "completed"}} }, 2},
		{"exec_empty_result", func(_, c *Snapshot) {
			c.Actions = []Action{{Adapter: "exec", Status: "completed", Result: &ToolResult{}}}
		}, 1},
		{"local_without_result", func(_, c *Snapshot) { c.Actions = []Action{{Adapter: "local", Status: "completed"}} }, 2},
		{"tool_cleanup", func(_, c *Snapshot) { c.Actions = []Action{{Result: &ToolResult{WorkerCleanupError: "process live"}}} }, 2},
		{"resource_cleanup", func(_, c *Snapshot) { c.Actions = []Action{{ResourceCleanupError: "identity changed"}} }, 2},
		{"observed_cleanup", func(_, c *Snapshot) { c.Actions = []Action{{Adapter: "exec", ResourceCleanupStatus: "completed"}} }, 1},
		{"unknown_worker_cleanup", func(p, _ *Snapshot) { p.Workers[0].CleanupStatus = "unknown" }, 2},
		{"stale_visit", func(_, c *Snapshot) { c.Plan.Tasks[0].LocalWorkerVisit++ }, 2},
		{"wrong_manifest", func(_, c *Snapshot) { c.WorkerContext.WorkerID = "other" }, 2},
		{"stale_settlement", func(_, c *Snapshot) { c.WorkerSettlement.Visit++ }, 2},
		{"unobserved_settlement", func(_, c *Snapshot) { c.WorkerSettlement.ObservedAt = time.Time{} }, 2},
		{"uncertain_local", func(_, c *Snapshot) { c.Plan.Tasks[0].Status = "uncertain" }, 2},
		{"disposed_parent", func(p, _ *Snapshot) { p.Plan.Tasks[0].Status, p.Plan.Tasks[0].DispositionReason = "blocked", "retired" }, 1},
		{"unproven_disposition", func(p, _ *Snapshot) { p.Plan.Tasks[0].Status = "blocked" }, 1},
		{"independent_manual", func(p, _ *Snapshot) { p.Plan.Tasks = append(p.Plan.Tasks, PlanTask{ID: "manual", Status: "uncertain"}) }, 2},
	}
	for _, backend := range []string{"json", "sqlite"} {
		for _, tc := range cases {
			t.Run(backend+"/"+tc.name, func(t *testing.T) {
				a, store := treeCapacityAgent(t, backend, MockProvider{})
				parent, err := store.Create("root", "", "")
				if err != nil {
					t.Fatal(err)
				}
				child, err := store.Create("child", "", "")
				if err != nil {
					t.Fatal(err)
				}
				visit := uint64(1)
				parent.Run.Status = StatusRunning
				parent.Plan.Capacity = 5
				parent.Plan.Tasks = []PlanTask{{ID: "task", WorkerID: "worker", ChildRunID: child.Run.ID, Status: "uncertain", LeaseUntil: time.Now().Add(time.Hour)}}
				parent.Workers = []AgentWorker{{ID: "worker", TaskID: "task", RunID: child.Run.ID, Status: "uncertain", CleanupStatus: "completed", Visit: visit, SettledVisit: &visit}}
				child.Run.ParentRunID, child.Run.ParentTaskID, child.Run.Status = parent.Run.ID, "task", StatusPaused
				child.Plan.Tasks = []PlanTask{{ID: "local", WorkerID: "worker", Status: "running", LocalExecution: true, LocalWorkerVisit: visit}}
				child.WorkerContext = &WorkerContext{WorkerID: "worker", TaskID: "task", RunID: child.Run.ID, ParentRunID: parent.Run.ID}
				child.WorkerSettlement = &WorkerSettlement{WorkerID: "worker", TaskID: "task", RunID: child.Run.ID, ParentRunID: parent.Run.ID, Visit: visit, CleanupStatus: "completed", ObservedAt: time.Now().UTC()}
				tc.edit(&parent, &child)
				for _, snap := range []Snapshot{parent, child} {
					if _, err := store.Update(snap.Run.ID, func(s *Snapshot) error { *s = snap; return nil }); err != nil {
						t.Fatal(err)
					}
				}
				unlock, err := store.acquireStoreLock()
				if err != nil {
					t.Fatal(err)
				}
				full, err := store.loadRawLocked(parent.Run.ID)
				if err != nil {
					unlock()
					t.Fatal(err)
				}
				before := valueHash(full)
				want, err := a.workerCapacityLocked(&full)
				unlock()
				if err != nil || want.Active != tc.want {
					t.Fatalf("fixture admission: %+v %v", want, err)
				}
				for _, id := range []string{parent.Run.ID, child.Run.ID} {
					got, err := a.WorkerCapacityContext(context.Background(), id)
					if err != nil || got != want {
						t.Fatalf("projection %s: %+v, want %+v: %v", id, got, want, err)
					}
				}
				unlock, err = store.acquireStoreLock()
				if err != nil {
					t.Fatal(err)
				}
				after, err := store.loadRawLocked(parent.Run.ID)
				unlock()
				if err != nil || valueHash(after) != before {
					t.Fatal("capacity read changed authoritative snapshot", err)
				}
			})
		}
	}
}

type capacityLockContext struct {
	context.Context
	calls   atomic.Int32
	reached chan struct{}
}

func (c *capacityLockContext) Err() error {
	// First check is display admission, second is the first Store attempt;
	// the third proves that at least one contended Store attempt has finished.
	if c.calls.Add(1) == 3 {
		close(c.reached)
	}
	return c.Context.Err()
}

func TestCapacityProjectionCancelledStoreWaitAndBackendFence(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			run := createTestRun(t, a)
			unlock, err := store.acquireStoreLock()
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			waiting := &capacityLockContext{Context: ctx, reached: make(chan struct{})}
			done := make(chan error, 1)
			go func() { _, err := a.WorkerCapacityContext(waiting, run); done <- err }()
			select {
			case <-waiting.reached:
			case <-time.After(time.Second):
				t.Fatal("capacity did not enter Store lock wait")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled read remained behind the writer lock")
			}
			unlock()
			if _, err := a.WorkerCapacityContext(context.Background(), run); err != nil {
				t.Fatal("cancel leaked Store lock", err)
			}
			if backend == "json" {
				if err := os.WriteFile(filepath.Join(store.root, sqliteStoreMarker), []byte("authority"), 0600); err != nil {
					t.Fatal(err)
				}
				if _, err := a.WorkerCapacity(run); err == nil {
					t.Fatal("read crossed SQLite authority marker")
				}
			}
		})
	}
}

type capacityCancelReader struct {
	reader io.Reader
	cancel context.CancelFunc
	reads  int
}

func (r *capacityCancelReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.reader.Read(p[:min(len(p), 64)])
	if r.reads == 2 {
		r.cancel()
	}
	return n, err
}

func TestCapacityProjectionStreamsAndCancelsWithoutRetainingHistory(t *testing.T) {
	payload := strings.Repeat("x", 1<<20)
	var raw bytes.Buffer
	raw.WriteString(`{"run":{"id":"root"},"messages":[`)
	for i := 0; i < 40; i++ {
		if i > 0 {
			raw.WriteByte(',')
		}
		encoded, _ := json.Marshal(Message{ID: "m", Content: payload})
		raw.Write(encoded)
	}
	raw.WriteString(`],"plan":{"capacity":5,"tasks":[{"id":"manual","status":"uncertain"}]},"actions":[{"status":"completed","result":{"stdout":"discarded"}}]}`)
	budget := activityProjectionMaxBytes
	var snap Snapshot
	if err := readCapacityProjection(context.Background(), bytes.NewReader(raw.Bytes()), &snap, &budget); err != nil {
		t.Fatal(err)
	}
	if len(snap.Messages) != 0 || len(snap.Requests) != 0 || snap.Actions[0].Result == nil || snap.Actions[0].Result.Stdout != "" || occupiedWorkerSlots(&snap) != 1 || budget < activityProjectionMaxBytes-(1<<20) {
		t.Fatal("capacity retained historical payload or lost scheduling inputs")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := &capacityCancelReader{reader: bytes.NewReader(raw.Bytes()), cancel: cancel}
	budget = activityProjectionMaxBytes
	if err := readCapacityProjection(ctx, r, &Snapshot{}, &budget); !errors.Is(err, context.Canceled) || r.reads != 2 {
		t.Fatalf("scan cancel: reads=%d err=%v", r.reads, err)
	}
	budget = 20
	if err := readCapacityProjection(context.Background(), strings.NewReader(`{"workers":[{"id":"w"}]}`), &Snapshot{}, &budget); !errors.Is(err, errActivityTooLarge) {
		t.Fatal("retained cap silently accepted partial state", err)
	}
	budget = 64 * int(reflect.TypeOf(Action{}).Size())
	many := `{"actions":[` + strings.TrimSuffix(strings.Repeat(`{},`, 128), ",") + `]}`
	if err := readCapacityProjection(context.Background(), strings.NewReader(many), &Snapshot{}, &budget); !errors.Is(err, errActivityTooLarge) {
		t.Fatal("slice spare capacity escaped the retained memory budget", err)
	}
}

func TestCapacityProjectionDuplicateKeysAndResultPresence(t *testing.T) {
	for _, raw := range []string{
		`{"run":{"id":"r"},"plan":{"tasks":[{"id":"a","status":"uncertain"}]},"PLAN":{"TASKS":[{"worker_id":"w"}]} }`,
		`{"actions":[{"adapter":"exec","result":{}}],"actions":[{"status":"completed"}]}`,
		`{"actions":[{"adapter":"exec","result":{}}],"actions":[{"result":null}]}`,
		`{"workers":[{"id":"w","status":"uncertain"}],"workers":[],"workers":[{"cleanup_status":"completed"}]}`,
	} {
		var want, got Snapshot
		if err := json.Unmarshal([]byte(raw), &want); err != nil {
			t.Fatal(err)
		}
		budget := activityProjectionMaxBytes
		if err := readCapacityProjection(context.Background(), strings.NewReader(raw), &got, &budget); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("duplicate/null changed semantics:\nwant=%+v\ngot=%+v", want, got)
		}
	}
}

func TestCapacityProjectionTreeIdentityAndIndependentBranch(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store := treeCapacityAgent(t, backend, MockProvider{})
			root := createTestRun(t, a)
			branch := createTestRun(t, a)
			if _, err := store.Update(branch, func(s *Snapshot) error { s.Run.ParentRunID = root; s.Plan.Capacity = 2; return nil }); err != nil {
				t.Fatal(err)
			}
			if got, err := a.WorkerCapacity(branch); err != nil || got.RootRunID != branch || got.Capacity != 2 {
				t.Fatalf("ordinary branch joined worker pool: %+v %v", got, err)
			}
			if _, err := store.Update(branch, func(s *Snapshot) error { s.Run.ParentTaskID = "task"; return nil }); err != nil {
				t.Fatal(err)
			}
			if _, err := a.WorkerCapacity(branch); err == nil || !strings.Contains(err.Error(), "unregistered") {
				t.Fatal("unregistered ancestry accepted", err)
			}
			if _, err := store.Update(root, func(s *Snapshot) error {
				s.Workers = []AgentWorker{{ID: "w", RunID: branch, TaskID: "other"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := a.WorkerCapacity(root); err == nil || !strings.Contains(err.Error(), "identity mismatch") {
				t.Fatal("mismatched child accepted", err)
			}
		})
	}
}
