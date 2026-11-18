package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func assertSchedulingQuiet(t *testing.T, changed <-chan struct{}) {
	t.Helper()
	select {
	case <-changed:
		t.Fatal("unrelated or uncommitted event woke scheduler")
	default:
	}
}

func TestSchedulingNotificationSelectiveCommitAndUnsubscribe(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	if err := store.AppendEvents(id, []Event{{Type: "worker_started", Summary: "historical worker"}}); err != nil {
		t.Fatal(err)
	}
	changed, stop := store.SubscribeScheduling(id)
	defer stop()
	second, stopSecond := store.SubscribeScheduling(id)
	defer stopSecond()
	assertSchedulingQuiet(t, changed)
	assertSchedulingQuiet(t, second)
	if err := store.AppendEvents(id, []Event{{Type: "model_stream", Data: "tokens"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.UpdatedAt = time.Now().UTC(); return nil }); err != nil {
		t.Fatal(err)
	}
	assertSchedulingQuiet(t, changed)
	assertSchedulingQuiet(t, second)
	if err := store.AppendEvents(id, []Event{{Type: "worker_message", Data: "committed message"}}); err != nil {
		t.Fatal(err)
	}
	for _, ch := range []<-chan struct{}{changed, second} {
		select {
		case <-ch:
		case <-time.After(time.Second):
			t.Fatal("committed scheduling event did not wake")
		}
	}
	stop()
	stop()
	if err := store.AppendEvents(id, []Event{{Type: "run_paused"}}); err != nil {
		t.Fatal(err)
	}
	assertSchedulingQuiet(t, changed)
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("second subscriber lost after first unsubscribe")
	}
}

func TestSchedulingNotificationFailedRenameNeverWakes(t *testing.T) {
	_, store, id, _ := dispatchFailureFixture(t)
	changed, stop := store.SubscribeScheduling(id)
	defer stop()
	var restore func()
	store.beforePersist = func(got string) {
		if got == id && restore == nil {
			restore = dispatchBlockRename(t, store, id)
		}
	}
	err := store.AppendEvents(id, []Event{{Type: "worker_message", Summary: "must not publish"}})
	store.beforePersist = nil
	if restore != nil {
		restore()
	}
	if err == nil {
		t.Fatal("fixture did not fail commit")
	}
	assertSchedulingQuiet(t, changed)
}

func TestSchedulingNotificationCoalescesAndDoesNotWakeOtherRun(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	other := createTestRun(t, a)
	changed, stop := store.SubscribeScheduling(id)
	defer stop()
	unrelated, stopOther := store.SubscribeScheduling(other)
	defer stopOther()
	for i := 0; i < 20; i++ {
		if err := store.AppendEvents(id, []Event{{Type: "worker_finished", Data: fmt.Sprint(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-changed:
	default:
		t.Fatal("coalesced notification missing")
	}
	assertSchedulingQuiet(t, changed)
	assertSchedulingQuiet(t, unrelated)
}

func TestSchedulingSecondSubscriberCannotConsumeFirstSubscriberCommit(t *testing.T) {
	a, store := newTestAgent(t, MockProvider{})
	id := createTestRun(t, a)
	first, stop := store.SubscribeScheduling(id)
	defer stop()
	s, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	addEvent(&s, "worker_finished", "committed before notification", nil)
	// saveLocked publishes the observation before notifySchedulingCommit. Hold
	// that exact boundary using the same cache operation, without fake timers.
	info, err := os.Stat(filepath.Join(store.root, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	store.cacheObservation(s, info)
	second, stopSecond := store.SubscribeScheduling(id)
	defer stopSecond()
	store.notifySchedulingCommit(id, s.Events, s.Run.EventSeq)
	select {
	case <-first:
	default:
		t.Fatal("second subscriber advanced shared cursor past an unnotified commit")
	}
	select {
	case <-second:
	default:
	}
}

// Measures source-local notification overhead without fsync noise. Reports
// notification operations, not CPU or end-to-end SSE p95 evidence.
func BenchmarkSchedulingSelectiveFiveWorkers(b *testing.B) {
	for _, kind := range []string{"model_stream", "worker_message"} {
		b.Run(kind, func(b *testing.B) {
			store, err := NewStore(b.TempDir())
			if err != nil {
				b.Fatal(err)
			}
			a, err := New(store, MockProvider{}, "")
			if err != nil {
				b.Fatal(err)
			}
			s, err := a.Create("five waiting workers")
			if err != nil {
				b.Fatal(err)
			}
			channels := []<-chan struct{}{}
			for i := 0; i < 5; i++ {
				ch, stop := store.SubscribeScheduling(s.Run.ID)
				defer stop()
				channels = append(channels, ch)
			}
			wakes := 0
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				store.notifySchedulingCommit(s.Run.ID, []Event{{Sequence: s.Run.EventSeq + i + 1, Type: kind}}, s.Run.EventSeq+i+1)
				for _, ch := range channels {
					select {
					case <-ch:
						wakes++
					default:
					}
				}
			}
			b.ReportMetric(float64(wakes)/float64(b.N), "wake/op")
		})
	}
}

// Publish only after the atomic rename succeeds. The file identity comes from
// the temporary file itself, not a later Stat that could observe another writer.
// A different process's generation still takes the complete JSON fallback.
func (s *Store) cacheObservation(snap Snapshot, info os.FileInfo) {
	data, err := json.Marshal(Observation{Run: snap.Run, Plan: snap.Plan, Workers: snap.Workers, WorkerMessages: snap.WorkerMessages, Mailbox: snap.Mailbox})
	if err == nil {
		s.eventViewMu.Lock()
		s.observations.Store(snap.Run.ID, &storedView{info: info, observation: data})
		s.eventViewMu.Unlock()
	}
}
