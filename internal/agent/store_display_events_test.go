package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDisplayEventWatermarkSkipsFullHistoryAndKeepsReplay(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Create("watermark fixture", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AppendEvents(snap.Run.ID, []Event{{Type: "fixture", Summary: "first"}, {Type: "fixture", Summary: "second"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActivityWorkspace(snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	s.ReleaseEventView(snap.Run.ID)
	run, batch, err := s.EventBatch(snap.Run.ID, int(^uint(0)>>1))
	if err != nil || len(batch) != 0 {
		t.Fatalf("empty watermark read: run=%+v batch=%d err=%v", run, len(batch), err)
	}
	if _, retained := s.eventViews.Load(snap.Run.ID); retained {
		t.Fatal("empty watermark loaded the complete history")
	}
	_, batch, err = s.EventBatch(snap.Run.ID, 0)
	if err != nil || len(batch) < 2 {
		t.Fatalf("replay lost committed events: batch=%d err=%v", len(batch), err)
	}
	if err = s.AppendEvents(snap.Run.ID, []Event{{Type: "fixture", Summary: "third"}}); err != nil {
		t.Fatal(err)
	}
	current, incremental, err := s.EventBatch(snap.Run.ID, run.EventSeq)
	if err != nil || len(incremental) != 1 || current.EventSeq != run.EventSeq+1 {
		t.Fatalf("incremental replay lost: run=%+v count=%d err=%v", current, len(incremental), err)
	}
}

func TestDisplayEventWatermarkRejectsMalformedEventSequence(t *testing.T) {
	for _, event := range []string{`{"sequence":"invalid"}`, `{"sequence":1.5}`, `{"sequence":1e100}`} {
		s, err := NewStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		snap, err := s.Create("malformed sequence", "", "")
		if err != nil {
			s.Close()
			t.Fatal(err)
		}
		snap.Run.EventSeq = 100
		run, err := json.Marshal(snap.Run)
		if err != nil {
			s.Close()
			t.Fatal(err)
		}
		raw := `{"run":` + string(run) + `,"messages":[],"events":[` + event + `]}`
		if _, err = buildDisplayRecordIndex(strings.NewReader(raw), nil, nil); err == nil {
			s.Close()
			t.Fatal("malformed event sequence was certified for an empty cursor")
		}
		if err = os.WriteFile(filepath.Join(s.root, snap.Run.ID+".json"), []byte(raw), 0600); err != nil {
			s.Close()
			t.Fatal(err)
		}
		_, _, err = s.EventBatch(snap.Run.ID, 100)
		s.Close()
		if err == nil {
			t.Fatal("empty cursor bypassed malformed event validation")
		}
	}
}

func TestDisplayEventWatermarkRejectsStaleAndUnvalidatedIndex(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	snap, err := s.Create("stale watermark fixture", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActivityWorkspace(snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(s.root, snap.Run.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	generation, err := displayGeneration(f)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	index, err := readJSONDisplayIndex(filepath.Join(s.root, snap.Run.ID+".display"), generation)
	if err != nil {
		t.Fatal(err)
	}
	index.EventsValidated = false
	staged, err := s.stageJSONDisplayIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(staged, filepath.Join(s.root, snap.Run.ID+".display")); err != nil {
		t.Fatal(err)
	}
	if _, empty, err := s.indexedEventWatermark(snap.Run.ID, 1000); err != nil || empty {
		t.Fatalf("unvalidated index accepted: empty=%v err=%v", empty, err)
	}
	index.EventsValidated = true
	staged, err = s.stageJSONDisplayIndex(index)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(staged, filepath.Join(s.root, snap.Run.ID+".display")); err != nil {
		t.Fatal(err)
	}
	// A new authoritative generation must not borrow the old index's watermark.
	snap.Run.EventSeq++
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.root, snap.Run.ID+".json")
	if err = os.WriteFile(path+".replacement", raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(path+".replacement", path); err != nil {
		t.Fatal(err)
	}
	if _, empty, err := s.indexedEventWatermark(snap.Run.ID, 1000); err != nil || empty {
		t.Fatalf("stale index accepted: empty=%v err=%v", empty, err)
	}
}
