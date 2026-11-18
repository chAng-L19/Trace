package agent

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func detachedPublicationView(t *testing.T, store *Store, id string) *storedView {
	t.Helper()
	file, err := os.Open(filepath.Join(store.root, id+".json"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(file)
	if err != nil {
		t.Fatal(err)
	}
	v := &storedView{info: info}
	if err := v.retainSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestSemanticPublicationRetainsDetachedSequenceHeaders(t *testing.T) {
	store, id := optimisticFixture(t)
	release := store.HoldEventView(id)
	defer release()
	snap, err := store.Update(id, func(s *Snapshot) error {
		s.Events = []Event{{Sequence: 9, Type: "newer"}, {Sequence: 3, Type: "older"}}
		s.Run.EventSeq = 9
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	cached, ok := store.eventViews.Load(id)
	if !ok {
		t.Fatal("semantic publication did not retain reader view")
	}
	v := cached.(*storedView)
	if len(v.sequences) != 2 || v.sequences[0] != 9 || v.sequences[1] != 3 {
		t.Fatalf("missing headers: %v", v.sequences)
	}
	snap.Events[0].Sequence = 100
	v.indexEvents()
	if v.eventsErr != nil || v.eventsOrdered || v.sequences[0] != 9 {
		t.Fatal("sequence index changed ordering or aliases caller")
	}
	_, batch, err := store.EventBatch(id, 5)
	if err != nil || len(batch) != 1 {
		t.Fatalf("warm cursor: %d %v", len(batch), err)
	}
	cold, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	_, again, err := cold.EventBatch(id, 5)
	if err != nil || len(again) != 1 || string(again[0]) != string(batch[0]) {
		t.Fatal("cold fallback disagrees with primed headers")
	}
}

func TestPublicationColdReaderCannotReplacePrimedGeneration(t *testing.T) {
	store, id := optimisticFixture(t)
	release := store.HoldEventView(id)
	defer release()
	if err := store.AppendEvents(id, []Event{{Type: "historical"}}); err != nil {
		t.Fatal(err)
	}
	// Hold an earlier cold read at its post-decode publication boundary.
	before := detachedPublicationView(t, store, id)
	epoch := store.eventViewEpoch
	if err := store.AppendEvents(id, []Event{{Type: "new"}}); err != nil {
		t.Fatal(err)
	}
	primed, _ := store.eventViews.Load(id)
	after := detachedPublicationView(t, store, id)
	for _, cold := range []*storedView{before, after} {
		for _, cacheResult := range []bool{false, true} {
			if got := store.finishReadView(id, true, cacheResult, epoch, cold); got != primed {
				t.Fatal("cold reader ignored current primed view")
			}
			if got, _ := store.eventViews.Load(id); got != primed {
				t.Fatal("cold reader replaced primed view")
			}
		}
	}
	view := primed.(*storedView)
	if len(view.events) != 2 || len(view.sequences) != 2 || view.fields["events"] != nil {
		t.Fatal("telemetry publication lost prebuilt event index")
	}
}

func TestPublicationReplacesChangedCachePointerAtCommit(t *testing.T) {
	for _, semantic := range []bool{false, true} {
		t.Run(map[bool]string{false: "telemetry", true: "semantic"}[semantic], func(t *testing.T) {
			store, id := optimisticFixture(t)
			if _, err := store.viewFields(id, true); err != nil {
				t.Fatal(err)
			}
			original, _ := store.eventViews.Load(id)
			var interleaved *storedView
			store.beforePersist = func(string) {
				// A cold reader of the same committed generation finishes after
				// the writer captured its base but before rename. Pointer CAS is
				// insufficient here even without the rename/publication window.
				interleaved = detachedPublicationView(t, store, id)
				store.eventViewMu.Lock()
				store.eventViews.Store(id, interleaved)
				store.eventViewMu.Unlock()
			}
			defer func() { store.beforePersist = nil }()
			if semantic {
				if _, err := store.Update(id, func(s *Snapshot) error { addEvent(s, "semantic", "", nil); return nil }); err != nil {
					t.Fatal(err)
				}
			} else if err := store.AppendEvents(id, []Event{{Type: "model_stream"}}); err != nil {
				t.Fatal(err)
			}
			store.beforePersist = nil
			published, _ := store.eventViews.Load(id)
			if published == original || published == interleaved {
				t.Fatal("commit lost view to reader pointer replacement")
			}
			info, err := os.Stat(filepath.Join(store.root, id+".json"))
			if err != nil || !sameGeneration(published.(*storedView).info, info) {
				t.Fatalf("published cache not current: %v", err)
			}
			v, err := store.readViewFields(id, true, true)
			if err != nil || v != published {
				t.Fatalf("reader cold-loaded just-published generation: %v", err)
			}
			if !semantic && (len(v.events) != 1 || v.fields["events"] != nil) {
				t.Fatal("primed index lost at publication")
			}
		})
	}
}

func TestPublicationReaderSelectionWaitsForRenameAndCache(t *testing.T) {
	store, id := optimisticFixture(t)
	previous, err := store.viewFields(id, true)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := buildAppendedEvents(id, previous, []Event{{Type: "new"}})
	if err != nil {
		t.Fatal(err)
	}
	parts, err := v.snapshotParts()
	if err != nil {
		t.Fatal(err)
	}
	staged, err := store.stageSnapshot(parts...)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(staged.path)
	// Reproduce the exact rename->cache window, inside the production lock.
	store.eventViewMu.Lock()
	locked := true
	defer func() {
		if locked {
			store.eventViewMu.Unlock()
		}
	}()
	info, err := store.publishSnapshotLocked(id, staged)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	type result struct {
		view *storedView
		err  error
	}
	read := make(chan result, 1)
	go func() {
		close(started)
		got, err := store.readViewFields(id, true, true)
		read <- result{got, err}
	}()
	<-started
	select {
	case got := <-read:
		t.Fatalf("reader passed a locked rename/cache boundary: %v", got.err)
	case <-time.After(20 * time.Millisecond):
	}
	v.info = info
	store.eventViews.Store(id, v)
	store.observations.Store(id, &storedView{info: info, observation: v.observation})
	store.eventViewMu.Unlock()
	locked = false
	got := <-read
	if got.err != nil || got.view != v {
		t.Fatalf("reader selected rename/cache gap: %v", got.err)
	}
}

func TestPublicationColdCompletionRespectsReleaseAndDiskGeneration(t *testing.T) {
	for _, change := range []string{"release", "other-store", "purge"} {
		t.Run(change, func(t *testing.T) {
			store, id := optimisticFixture(t)
			cold := detachedPublicationView(t, store, id)
			epoch := store.eventViewEpoch
			switch change {
			case "release":
				store.ReleaseEventView(id)
			case "other-store":
				other, err := NewStore(store.root)
				if err != nil {
					t.Fatal(err)
				}
				if err := other.AppendEvents(id, []Event{{Type: "other"}}); err != nil {
					t.Fatal(err)
				}
			case "purge":
				if _, err := store.PurgeRun(id); err != nil {
					t.Fatal(err)
				}
			}
			store.finishReadView(id, true, true, epoch, cold)
			if _, exists := store.eventViews.Load(id); exists {
				t.Fatal("late cold completion cached a released/stale/purged view")
			}
		})
	}
}

func TestPublicationSemanticReleaseDoesNotResurrectView(t *testing.T) {
	store, id := optimisticFixture(t)
	if _, err := store.viewFields(id, true); err != nil {
		t.Fatal(err)
	}
	store.beforePersist = func(string) { store.ReleaseEventView(id) }
	defer func() { store.beforePersist = nil }()
	if _, err := store.Update(id, func(s *Snapshot) error { s.Run.Goal = "committed"; return nil }); err != nil {
		t.Fatal(err)
	}
	store.beforePersist = nil
	if _, exists := store.eventViews.Load(id); exists {
		t.Fatal("semantic commit resurrected released view")
	}
	observation, err := store.Observe(id)
	if err != nil || observation.Run.Goal != "committed" {
		t.Fatalf("semantic observation not published: %+v %v", observation.Run, err)
	}
}

func TestPublicationHeldReaderSurvivesUnrelatedReleaseFence(t *testing.T) {
	store, id := optimisticFixture(t)
	other, err := store.Create("other", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	release := store.HoldEventView(id)
	defer release()
	cold := detachedPublicationView(t, store, id)
	epoch := store.eventViewEpoch
	store.ReleaseEventView(other.Run.ID)
	store.finishReadView(id, true, true, epoch, cold)
	if got, ok := store.eventViews.Load(id); !ok || got != cold {
		t.Fatal("unrelated release prevented held reader from caching")
	}
	release()
	store.finishReadView(id, true, true, epoch, cold)
	if _, ok := store.eventViews.Load(id); ok {
		t.Fatal("last reader release was undone")
	}
	resubscribe := store.HoldEventView(id)
	defer resubscribe()
	store.finishReadView(id, true, true, epoch, cold)
	if got, ok := store.eventViews.Load(id); !ok || got != cold {
		t.Fatal("new active reader could not reuse current immutable generation")
	}
}
