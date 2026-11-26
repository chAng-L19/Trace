package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func cancelVisitFixture(t *testing.T, backend string, settled bool) (*Agent, *Store, string, AgentWorker) {
	t.Helper()
	a, store := treeCapacityAgent(t, backend, MockProvider{})
	id := createTestRun(t, a)
	if _, err := a.SetPlan(id, []PlanTask{{ID: "work", Goal: "preserve worker identity"}}); err != nil {
		t.Fatal(err)
	}
	_, w := capacityFixtureChild(t, a, id, "work")
	if settled {
		if _, err := store.Update(w.RunID, func(s *Snapshot) error {
			s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Update(id, func(s *Snapshot) error {
			s.Workers[0].Status, s.Workers[0].CleanupStatus = "paused", "completed"
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		w.Status, w.CleanupStatus = "paused", "completed"
	}
	return a, store, id, w
}

func TestWorkerCancelVisitFencesAndRetainsCleanup(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, test := range []struct {
			name                   string
			visit                  uint64
			compat, settled, stale bool
		}{
			{name: "matching", visit: 1},
			{name: "stale", visit: 2, stale: true},
			{name: "explicit-zero", visit: 0, stale: true},
			{name: "compatibility", compat: true},
			{name: "settled", visit: 1, settled: true},
		} {
			t.Run(backend+"/"+test.name, func(t *testing.T) {
				a, store, id, w := cancelVisitFixture(t, backend, test.settled)
				before, err := store.Update(w.RunID, func(s *Snapshot) error {
					s.Mailbox = append(s.Mailbox, QueuedMessage{ID: "retained-input", Kind: "steer", Content: "durable input", Revision: 1})
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				var expected *uint64
				if !test.compat {
					expected = &test.visit
				}
				parent, err := a.CancelWorkerVisit(id, w.ID, expected)
				if test.stale {
					if !errors.Is(err, ErrStaleWorkerVisit) {
						t.Fatalf("stale cancellation accepted: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				child, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				if valueHash(child.Mailbox) != valueHash(before.Mailbox) {
					t.Fatal("cancellation lost the durable mailbox")
				}
				if test.stale {
					if child.Run.Status != before.Run.Status || child.Run.EventSeq != before.Run.EventSeq {
						t.Fatal("stale cancellation mutated the child")
					}
					return
				}
				if child.Run.Status != StatusCancelled {
					t.Fatalf("child was not cancelled: %s", child.Run.Status)
				}
				if test.settled {
					if parent.Workers[0].Status != "cancelled" || parent.Plan.Tasks[0].Status != "cancelled" {
						t.Fatal("settled worker did not retire its task")
					}
				} else if parent.Workers[0].Status != "running" || parent.Workers[0].CleanupStatus != "pending" {
					t.Fatal("cancellation released capacity before cleanup")
				}
				if _, err := a.CancelWorkerVisit(id, w.ID, expected); err != nil {
					t.Fatal(err)
				}
				repeated, err := store.Load(w.RunID)
				if err != nil || repeated.Run.EventSeq != child.Run.EventSeq {
					t.Fatalf("retry duplicated child cancellation: %v", err)
				}
			})
		}
	}
}

type resumeCommitGate struct {
	context.Context
	entered, proceed chan struct{}
	once             sync.Once
}

func (g *resumeCommitGate) Done() <-chan struct{} {
	g.once.Do(func() { close(g.entered); <-g.proceed })
	return g.Context.Done()
}

func TestWorkerResumeRejectsTerminalChildAtCommit(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, terminal := range []Status{StatusCancelled, StatusCompleted} {
			t.Run(backend+"/"+string(terminal), func(t *testing.T) {
				a, store, id, w := cancelVisitFixture(t, backend, true)
				unlock, err := store.AcquireRunLock(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
				child, err := store.Load(w.RunID)
				if err != nil {
					t.Fatal(err)
				}
				gate := &resumeCommitGate{Context: context.Background(), entered: make(chan struct{}), proceed: make(chan struct{})}
				var proceed sync.Once
				releaseGate := func() { proceed.Do(func() { close(gate.proceed) }) }
				defer releaseGate()
				done := make(chan error, 1)
				go func() {
					_, release, _, err := a.prepareWorkerResume(gate, child)
					if release != nil {
						release()
					}
					done <- err
				}()
				select {
				case <-gate.entered:
				case err := <-done:
					t.Fatalf("resume did not reach commit gate: %v", err)
				case <-time.After(5 * time.Second):
					t.Fatal("resume did not reach commit gate")
				}
				if terminal == StatusCancelled {
					_, err = a.Cancel(w.RunID)
				} else {
					_, err = store.Update(w.RunID, func(s *Snapshot) error { s.Run.Status = terminal; return nil })
				}
				if err != nil {
					t.Fatal(err)
				}
				releaseGate()
				select {
				case err := <-done:
					if err == nil {
						t.Fatal("terminal child admitted for another visit")
					}
				case <-time.After(5 * time.Second):
					t.Fatal("resume did not settle")
				}
				parent, err := store.Load(id)
				if err != nil {
					t.Fatal(err)
				}
				if parent.Workers[0].Visit != w.Visit {
					t.Fatal("terminal child advanced the visit")
				}
				for _, event := range parent.Events {
					if event.Type == "worker_resumed" {
						t.Fatal("terminal child produced a resume receipt")
					}
				}
			})
		}
	}
}

func TestWorkerCancelOldVisitAfterResume(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			a, store, id, w := cancelVisitFixture(t, backend, true)
			unlock, err := store.AcquireRunLock(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			defer unlock()
			child, err := store.Load(w.RunID)
			if err != nil {
				t.Fatal(err)
			}
			_, release, resumed, err := a.prepareWorkerResume(context.Background(), child)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if resumed.Visit != w.Visit+1 {
				t.Fatal("fixture did not resume its original worker")
			}
			if _, err := a.CancelWorkerVisit(id, w.ID, &w.Visit); !errors.Is(err, ErrStaleWorkerVisit) {
				t.Fatalf("old visit cancelled continuation: %v", err)
			}
			child, err = store.Load(w.RunID)
			if err != nil || child.Run.Status == StatusCancelled {
				t.Fatalf("continuation was cancelled: %v", err)
			}
			for _, arguments := range []string{
				fmt.Sprintf("{\"worker_id\":%q}", w.ID),
				fmt.Sprintf("{\"worker_id\":%q,\"expected_visit\":null}", w.ID),
			} {
				if _, err := a.builtin(context.Background(), id, ToolCall{Name: "worker_cancel", Arguments: arguments}); err == nil || !strings.Contains(err.Error(), "expected_visit") {
					t.Fatalf("tool omitted observed visit: %v", err)
				}
			}
		})
	}
}
