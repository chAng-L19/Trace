package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestObservationAndEventFeedFollowAtomicCrossStoreWrites(t *testing.T) {
	root := t.TempDir()
	one, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := one.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	if _, err = one.Observe(id); err != nil {
		t.Fatal(err)
	}
	if _, _, err = one.EventBatch(id, 0); err != nil {
		t.Fatal(err)
	}
	if err = two.AppendEvents(id, []Event{{Type: "model_stream", Data: map[string]string{"value": "first"}}}); err != nil {
		t.Fatal(err)
	}
	view, err := one.Observe(id)
	if err != nil || view.Run.EventSeq != 1 {
		t.Fatalf("stale observation: %+v %v", view, err)
	}
	_, events, err := one.EventBatch(id, 0)
	if err != nil || len(events) != 1 {
		t.Fatalf("missing committed event: %v %v", events, err)
	}
	events[0][0] = '!'
	_, events, err = one.EventBatch(id, 0)
	if err != nil || !json.Valid(events[0]) {
		t.Fatal("caller mutated cached feed")
	}
	if err = two.AppendEvents(id, []Event{{Type: "tool_output"}}); err != nil {
		t.Fatal(err)
	}
	_, events, err = one.EventBatch(id, 1)
	if err != nil || len(events) != 1 {
		t.Fatalf("cursor duplicated or lost event: %d %v", len(events), err)
	}
	var event Event
	if err = json.Unmarshal(events[0], &event); err != nil || event.Sequence != 2 {
		t.Fatalf("sequence: %+v %v", event, err)
	}
	_, err = two.Update(id, func(s *Snapshot) error { s.Run.Deleted = true; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = one.EventBatch(id, 0); !os.IsNotExist(err) {
		t.Fatalf("deleted feed visible: %v", err)
	}
	if err = one.AppendEvents(id, []Event{{Type: "late"}}); err == nil {
		t.Fatal("deleted archive accepted write")
	}
	runs, err := one.List()
	if err != nil || len(runs) != 0 {
		t.Fatalf("deleted list visible: %+v %v", runs, err)
	}
}

func TestCommittedObservationCacheIsDetached(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	snap, err = store.Update(snap.Run.ID, func(s *Snapshot) error { s.Workers = []AgentWorker{{ID: "original", Status: "running"}}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	snap.Workers[0].ID = "caller mutation"
	v, err := store.Observe(snap.Run.ID)
	if err != nil || v.Workers[0].ID != "original" {
		t.Fatalf("save cache shared caller slices: %+v %v", v, err)
	}
	cached, _ := store.observations.Load(snap.Run.ID)
	if len(cached.(*storedView).observation) == 0 {
		t.Fatal("committed metadata not cached")
	}
}

func TestConcurrentEventReadersKeepDetachedIndexedBytes(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	events := make([]Event, 100)
	for i := range events {
		events[i] = Event{Type: "model_stream", Data: fmt.Sprint(i)}
	}
	if err = store.AppendEvents(snap.Run.ID, events); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	for i := 0; i < 8; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < 10; j++ {
				_, batch, err := store.EventBatch(snap.Run.ID, 98)
				if err != nil || len(batch) != 2 {
					t.Errorf("indexed suffix lost: %d %v", len(batch), err)
					return
				}
				if !json.Valid(batch[0]) {
					t.Error("shared caller mutation")
					return
				}
				batch[0][0] = '!'
			}
		}()
	}
	group.Wait()
}

func TestCommittedEventViewsReuseOnlyMatchingAppendGeneration(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	data := map[string]string{"value": "original"}
	if _, err := store.viewFields(id, true); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvents(id, []Event{{Type: "first", Data: data}}); err != nil {
		t.Fatal(err)
	}
	old, _ := store.eventViews.Load(id)
	data["value"] = "caller mutation"
	if err := store.AppendEvents(id, []Event{{Type: "second"}}); err != nil {
		t.Fatal(err)
	}
	current, _ := store.eventViews.Load(id)
	before, after := old.(*storedView), current.(*storedView)
	if len(before.events) != 1 || len(after.events) != 2 || &before.events[0][0] != &after.events[0][0] {
		t.Fatal("append did not reuse immutable committed history")
	}
	if !bytes.Contains(after.events[0], []byte("original")) {
		t.Fatal("caller mutated committed event")
	}
	other, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := other.Update(id, func(s *Snapshot) error { s.Events[0].Type = "rewritten"; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvents(id, []Event{{Type: "third"}}); err != nil {
		t.Fatal(err)
	}
	_, batch, err := store.EventBatch(id, 0)
	if err != nil || len(batch) != 3 || !bytes.Contains(batch[0], []byte("rewritten")) {
		t.Fatalf("cross-store event rewrite lost: %q %v", batch, err)
	}
	changed, err := store.Update(id, func(s *Snapshot) error { s.Events[0].Type = "same-store rewrite"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	changed.Events[0].Type = "caller changed returned snapshot"
	_, batch, err = store.EventBatch(id, 0)
	if err != nil || !bytes.Contains(batch[0], []byte("same-store rewrite")) {
		t.Fatalf("general update reused old history: %q %v", batch, err)
	}
	committed, _ := store.eventViews.Load(id)
	if err := store.AppendEvents(id, []Event{{Type: "unserializable", Data: make(chan int)}}); err == nil {
		t.Fatal("invalid event committed")
	}
	unchanged, _ := store.eventViews.Load(id)
	if committed != unchanged {
		t.Fatal("failed commit published cache")
	}
	run, batch, err := store.EventBatch(id, 2)
	if err != nil || run.EventSeq != 3 || len(batch) != 1 {
		t.Fatalf("failed commit advanced cursor: %+v %q %v", run, batch, err)
	}
}

func TestCommittedEventViewRetentionRequiresReader(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	if err := store.AppendEvents(id, []Event{{Type: "without-reader"}}); err != nil {
		t.Fatal(err)
	}
	if _, retained := store.eventViews.Load(id); retained {
		t.Fatal("unobserved run retains event history")
	}
	if _, _, err := store.EventBatch(id, 0); err != nil {
		t.Fatal(err)
	}
	store.ReleaseEventView(id)
	if err := store.AppendEvents(id, []Event{{Type: "after-release"}}); err != nil {
		t.Fatal(err)
	}
	if _, retained := store.eventViews.Load(id); retained {
		t.Fatal("commit resurrected released event history")
	}
	_, events, err := store.EventBatch(id, 0)
	if err != nil || len(events) != 2 {
		t.Fatalf("released reader lost fallback: %d %v", len(events), err)
	}
}

func TestCommittedEventViewsConcurrentReadersAndRelease(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	if _, _, err := store.EventBatch(id, 0); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 20; i++ {
			if err := store.AppendEvents(id, []Event{{Type: "frame", Data: i}}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	// Even if the writer fails, join it before TempDir cleanup.
	defer func() {
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for i := 0; i < 30; i++ {
		if i%3 == 0 {
			store.ReleaseEventView(id)
		}
		run, events, err := store.EventBatch(id, 0)
		if err != nil || len(events) != run.EventSeq {
			t.Fatalf("mixed committed generations: %d %+v %v", len(events), run, err)
		}
		for j, raw := range events {
			var event Event
			if err := json.Unmarshal(raw, &event); err != nil || event.Sequence != j+1 {
				t.Fatalf("event index corrupted: %+v %v", event, err)
			}
		}
		if len(events) > 0 {
			events[0][0] = '!'
		}
	}
}

func TestLegacyMessageJSONAndWireBytesRemainStable(t *testing.T) {
	// Existing source hashes cover compact JSON in this exact pre-request_id
	// field order. A missing new optional identity must not rewrite that source.
	legacy := []byte(`{"id":"m1","parent_id":"m0","thinking":"thought","role":"assistant","content":"body","tool_calls":[{"id":"c1","name":"exec","arguments":"{}"}],"created_at":"2026-10-19T00:00:00Z"}`)
	var message Message
	if err := json.Unmarshal(legacy, &message); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(message)
	if err != nil || !bytes.Equal(encoded, legacy) {
		t.Fatalf("legacy message hash input changed: %s %v", encoded, err)
	}
	legacyRequest := []byte(`{"context_tokens":123,"id":"r1","prompt_hash":"h","messages":[` + string(legacy) + `],"created_at":"2026-10-19T00:00:00Z"}`)
	var request ModelRequest
	if err = json.Unmarshal(legacyRequest, &request); err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(request)
	if err != nil || !bytes.Equal(encoded, legacyRequest) {
		t.Fatalf("legacy request hash input changed: %s %v", encoded, err)
	}
	before, _ := json.Marshal(wireMessages([]Message{message}))
	message.RequestID = "request-1"
	after, _ := json.Marshal(wireMessages([]Message{message}))
	if !bytes.Equal(before, after) {
		t.Fatal("request identity leaked into provider wire messages")
	}
}

func BenchmarkLargeSnapshotViews(b *testing.B) {
	store, err := NewStore(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	snap, err := store.Create("large view fixture", "prompt", "hash")
	if err != nil {
		b.Fatal(err)
	}
	snap.Requests = []ModelRequest{{ID: "large-request", Messages: []Message{{Role: "user", Content: strings.Repeat("x", 10<<20)}}}}
	for i := 0; i < 20000; i++ {
		addEvent(&snap, "model_stream", "fixture", map[string]string{"content": strings.Repeat("x", 128)})
	}
	if err = store.saveLocked(snap); err != nil {
		b.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(store.root, snap.Run.ID+".json"))
	if err != nil {
		b.Fatal(err)
	}
	for _, cold := range []bool{false, true} {
		b.Run(fmt.Sprintf("Observe/cold=%t", cold), func(b *testing.B) {
			b.ReportAllocs()
			b.ReportMetric(float64(info.Size()), "snapshot-bytes")
			for i := 0; i < b.N; i++ {
				if cold {
					store.observations.Delete(snap.Run.ID)
				}
				if _, err := store.Observe(snap.Run.ID); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("EventSuffix/cold=%t", cold), func(b *testing.B) {
			if _, _, err := store.EventBatch(snap.Run.ID, 19999); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			b.ReportMetric(float64(info.Size()), "snapshot-bytes")
			for i := 0; i < b.N; i++ {
				if cold {
					store.ReleaseEventView(snap.Run.ID)
				}
				_, events, err := store.EventBatch(snap.Run.ID, 19999)
				if err != nil || len(events) != 1 {
					b.Fatalf("suffix: %d %v", len(events), err)
				}
			}
		})
	}
	b.Run("AppendAndCommittedSuffix", func(b *testing.B) {
		view, err := store.Observe(snap.Run.ID)
		if err != nil {
			b.Fatal(err)
		}
		if _, _, err := store.EventBatch(snap.Run.ID, view.Run.EventSeq-1); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.ResetTimer()
		b.ReportMetric(float64(info.Size()), "snapshot-bytes")
		cursor := view.Run.EventSeq
		for i := 0; i < b.N; i++ {
			if err := store.AppendEvents(snap.Run.ID, []Event{{Type: "model_stream", Data: "new frame"}}); err != nil {
				b.Fatal(err)
			}
			run, events, err := store.EventBatch(snap.Run.ID, cursor)
			if err != nil || len(events) != 1 {
				b.Fatalf("committed suffix: %d %v", len(events), err)
			}
			cursor = run.EventSeq
		}
	})
}

func TestObservationAcceptsReorderedJSONAndRejectsMismatchedIdentity(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{"run": snap.Run, "workers": []AgentWorker{{ID: "late", Status: "running"}}, "plan": snap.Plan})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.root, snap.Run.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	v, err := store.Observe(snap.Run.ID)
	if err != nil || len(v.Workers) != 1 {
		t.Fatalf("lost reordered metadata: %+v %v", v, err)
	}
	v.Workers[0].Status = "mutated"
	v, err = store.Observe(snap.Run.ID)
	if err != nil || v.Workers[0].Status != "running" {
		t.Fatal("observation cache mutable")
	}
	if err = os.WriteFile(filepath.Join(store.root, "wrong.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Observe("wrong"); err == nil {
		t.Fatal("mismatched run accepted")
	}
}
