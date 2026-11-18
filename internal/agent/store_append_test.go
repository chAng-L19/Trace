package agent

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestTelemetrySplicePreservesExactHistoryAndFieldOrder(t *testing.T) {
	for _, cold := range []bool{false, true} {
		for _, eventsFirst := range []bool{false, true} {
			for _, initial := range []string{"null", "[ \n ]", `[{"sequence":1,"type":"historical","data":{"precise":9007199254740993,"text":"} ] \\\""}}]`} {
				store, err := NewStore(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				snap, err := store.Create("fixture", "prompt", "hash")
				if err != nil {
					t.Fatal(err)
				}
				if len(initial) > 10 {
					snap.Run.EventSeq = 1
				}
				run, _ := json.Marshal(snap.Run)
				before := []byte("{\n\"run\" : " + string(run) + ",\n\"events\" : " + initial)
				if eventsFirst {
					before = []byte("{\n\"events\" : " + initial + ",\n\"run\" : " + string(run))
				}
				before = append(before, []byte(",\n\"requests\" : [ {\"id\":\"r1\", \"context_tokens\":123, \"messages\": [{\"role\":\"user\",\"content\":\"\\u4e2d\\u6587 \\\" [ ] { }\"}]} ],\n\"unknown\" : {\"precise\":9007199254740993, \"escaped\":\"\\u0061\"},\n\"plan\":{\"capacity\":5}}\n")...)
				path := filepath.Join(store.root, snap.Run.ID+".json")
				if err := os.WriteFile(path, before, 0600); err != nil {
					t.Fatal(err)
				}
				if _, _, err := store.EventBatch(snap.Run.ID, 0); err != nil {
					t.Fatal(err)
				}
				var original map[string]json.RawMessage
				if err := json.Unmarshal(before, &original); err != nil {
					t.Fatal(err)
				}
				for i := 0; i < 3; i++ {
					if cold {
						store.ReleaseEventView(snap.Run.ID)
					}
					if err := store.AppendEvents(snap.Run.ID, []Event{{Type: "model_stream", Data: map[string]string{"text": "中文\n\" } ]"}}}); err != nil {
						t.Fatal(err)
					}
					after, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(after, &fields); err != nil {
						t.Fatalf("invalid spliced JSON: %s %v", after, err)
					}
					for _, name := range []string{"requests", "unknown", "plan"} {
						if !bytes.Equal(original[name], fields[name]) {
							t.Fatalf("%s history re-encoded: %s -> %s", name, original[name], fields[name])
						}
					}
					run, batch, err := store.EventBatch(snap.Run.ID, snap.Run.EventSeq+i)
					if err != nil || len(batch) != 1 || run.EventSeq != snap.Run.EventSeq+i+1 {
						t.Fatalf("spliced cursor: %+v %d %v", run, len(batch), err)
					}
					observation, err := store.Observe(snap.Run.ID)
					if err != nil || observation.Run.EventSeq != run.EventSeq || observation.Plan.Capacity != 5 {
						t.Fatalf("spliced observation: %+v %v", observation, err)
					}
				}
			}
		}
	}
}

func TestTelemetrySegmentsRemainDetachedAndWorkspaceCurrent(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	if _, err := store.viewFields(id, true); err != nil {
		t.Fatal(err)
	}
	old, _ := store.eventViews.Load(id)
	for i := 0; i < 32; i++ {
		if err := store.AppendEvents(id, []Event{{Type: "frame", Data: i}}); err != nil {
			t.Fatal(err)
		}
	}
	current, _ := store.eventViews.Load(id)
	before, after := old.(*storedView), current.(*storedView)
	if &before.snapshot[0] != &after.snapshot[0] || len(before.eventTail) != 0 {
		t.Fatal("append copied or mutated immutable prefix")
	}
	disk, err := os.ReadFile(filepath.Join(store.root, id+".json"))
	if err != nil || !bytes.Equal(disk, after.snapshotBytes()) {
		t.Fatalf("segmented snapshot differs from commit: %v", err)
	}
	view, err := store.Workspace(id)
	if err != nil || view.Run.EventSeq != 32 || len(view.Events) != 32 {
		t.Fatalf("workspace stale: %d %d %v", view.Run.EventSeq, len(view.Events), err)
	}
}

func TestEventWakeupFollowsCommitAndCoalesces(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	release := store.HoldEventView(snap.Run.ID)
	defer release()
	changed := store.EventChanges(snap.Run.ID)
	if err := store.AppendEvents(snap.Run.ID, []Event{{Type: "bad", Data: make(chan int)}}); err == nil {
		t.Fatal("invalid event committed")
	}
	select {
	case <-changed:
		t.Fatal("failed commit woke reader")
	default:
	}
	for i := 0; i < 2; i++ {
		if err := store.AppendEvents(snap.Run.ID, []Event{{Type: "frame"}}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-changed:
	default:
		t.Fatal("commit did not wake reader")
	}
	next := store.EventChanges(snap.Run.ID)
	select {
	case <-next:
		t.Fatal("new generation already closed")
	default:
	}
	run, batch, err := store.EventBatch(snap.Run.ID, 0)
	if err != nil || run.EventSeq != 2 || len(batch) != 2 {
		t.Fatalf("coalesced wake lost frames: %d %d %v", run.EventSeq, len(batch), err)
	}
}

func TestEventWakeupDoesNotPublishFailedRename(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	snap, err := store.Create("fixture", "prompt", "hash")
	if err != nil {
		t.Fatal(err)
	}
	id := snap.Run.ID
	if _, err = store.viewFields(id, true); err != nil {
		t.Fatal(err)
	}
	old, _ := store.eventViews.Load(id)
	release := store.HoldEventView(id)
	defer release()
	changed := store.EventChanges(id)
	path := filepath.Join(store.root, id+".json")
	saved := path + ".saved"
	if err = os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err = store.appendCachedEventsLocked(id, old.(*storedView), []Event{{Type: "failed"}}); err == nil {
		t.Fatal("directory replaced by snapshot")
	}
	select {
	case <-changed:
		t.Fatal("failed persistence notified reader")
	default:
	}
	current, _ := store.eventViews.Load(id)
	if current != old {
		t.Fatal("failed persistence published cache")
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	run, batch, err := store.EventBatch(id, 0)
	if err != nil || run.EventSeq != 0 || len(batch) != 0 {
		t.Fatalf("failed persistence advanced cursor: %d %d %v", run.EventSeq, len(batch), err)
	}
}

func (v *storedView) snapshotBytes() []byte {
	if len(v.eventTail) == 0 {
		return v.snapshot
	}
	parts, _ := v.snapshotParts() // constructed and validated before publication
	return bytes.Join(parts, nil)
}
