package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestSnapshotStreamMatchesJSON(t *testing.T) {
	snap := activityRequestHistory(100, 16)
	snap.Events = []Event{{Sequence: 3, Data: map[string]any{"n": float64(42)}}}
	snap.Mailbox = []QueuedMessage{}
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err = encodeSnapshot(&encoded, snap); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, encoded.Bytes()) {
		t.Fatal("streaming writer changed JSON representation")
	}
	var got, standard Snapshot
	if err = decodeSnapshot(bytes.NewReader(raw), &got); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &standard); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, standard) {
		t.Fatal("streaming reader changed snapshot")
	}
	for _, raw := range []string{`{"run":{"id":"x"},"messages":[]}`, `{"messages":[{}],"messages":null}`, `{"unknown":{"array":[1,true,null,{}]},"RUN":{"id":"x"}}`, `{"messages":[{"id":"first","content":"retained"}],"messages":[{"id":"second"}]}`} {
		got, standard = Snapshot{}, Snapshot{}
		if err = decodeSnapshot(strings.NewReader(raw), &got); err != nil {
			t.Fatal(err)
		}
		if err = json.Unmarshal([]byte(raw), &standard); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, standard) {
			t.Fatalf("different decoding for %s", raw)
		}
	}
	for _, raw := range []string{`{"requests":{}}`, `{"unknown":[1,]}`, `{"run":{"id":42}}`, `{} {}`} {
		if err = decodeSnapshot(strings.NewReader(raw), &got); err == nil {
			t.Fatalf("accepted malformed JSON: %s", raw)
		}
	}
}

func TestActivityStreamMatchesProjection(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		snap := activityRequestHistory(100, 16)
		if legacy {
			snap.Messages[len(snap.Messages)-1].RequestID = ""
		}
		for i := range snap.Responses {
			snap.Responses[i].Response.Thinking = strings.Repeat("response ", 1024)
		}
		raw, err := json.Marshal(snap)
		if err != nil {
			t.Fatal(err)
		}
		full := &storedView{}
		if err = full.retainSnapshot(raw); err != nil {
			t.Fatal(err)
		}
		want, err := buildActivity(full.fields)
		if err != nil {
			t.Fatal(err)
		}
		stream, response, err := readActivityFields(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(stream.fields["requests"], []byte("context context")) || bytes.Contains(stream.fields["responses"], []byte("response response")) {
			t.Fatal("stream retained historical request/response body")
		}
		got, err := buildActivityRecords(stream.fields, response)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("stream changed projection (legacy=%v)", legacy)
		}
	}
}

func TestActivityStreamBoundsAndValidation(t *testing.T) {
	for _, raw := range []string{`{"run":{"id":"x"},"requests":[{"provider_model":42}]}`, `{"responses":[{"request_id":42}]}`, `{"events":[{"data":[1,]}]}`, `{"run":{"id":"x"}} {}`} {
		if _, _, err := readActivityFields(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted malformed history: %s", raw)
		}
	}
	raw := `{"messages":[{"content":"` + strings.Repeat("x", activityProjectionMaxBytes+1) + `"}]}`
	if _, _, err := readActivityFields(strings.NewReader(raw)); !errors.Is(err, errActivityTooLarge) {
		t.Fatalf("unbounded record: %v", err)
	}
	for _, field := range []string{"unknown", "events"} {
		raw := `{"` + field + `":` + strings.Repeat("[", 10001) + "0" + strings.Repeat("]", 10001) + "}"
		if _, _, err := readActivityFields(strings.NewReader(raw)); err == nil {
			t.Fatal("activity accepted deeply nested omitted history")
		}
		if field == "unknown" {
			var snap Snapshot
			if err := decodeSnapshot(strings.NewReader(raw), &snap); err == nil {
				t.Fatal("snapshot accepted deeply nested unknown history")
			}
		}
	}
}

func TestActivityStreamHistoricalManifestsStayOnDemand(t *testing.T) {
	snap := activityRequestHistory(96, 4096)
	raw, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) <= activityProjectionMaxBytes {
		t.Fatal("fixture must exceed aggregate projection budget")
	}
	v, records, err := readActivityFields(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if len(v.fields["requests"]) > 1<<20 {
		t.Fatal("historical manifests were retained before selection")
	}
	p, err := buildActivityRecords(v.fields, records)
	if err != nil {
		t.Fatal(err)
	}
	if p.Totals["requests"] != 96 || len(p.Snapshot.Requests) != 5 {
		t.Fatal("on-demand requests changed totals or selection")
	}
	for _, request := range p.Snapshot.Requests {
		if request.ContextManifest != nil {
			t.Fatal("selected display request retained detail-only manifest")
		}
	}
	// Even every in-flight identity remains compact; the canonical manifests
	// were typed-validated by the reader and remain intact in request details.
	v.fields["responses"] = json.RawMessage(`[]`)
	all, err := buildActivityRecords(v.fields, records)
	if err != nil || len(all.Snapshot.Requests) != 96 {
		t.Fatalf("detail manifests prevented compact in-flight display: %v", err)
	}
	for _, request := range snap.Requests {
		if request.ContextManifest == nil || len(request.ContextManifest.Parts) != 4096 {
			t.Fatal("display mutated original manifest")
		}
	}
}

func TestLargeSnapshotSavePreservesHistoryAndPublication(t *testing.T) {
	store := activityCacheStore(t, "json")
	snap, err := store.Create("large", "", "")
	if err != nil {
		t.Fatal(err)
	}
	snap.Messages = []Message{{ID: "m", Role: "user", Content: "original"}}
	for i := 0; i < 20; i++ {
		snap.Requests = append(snap.Requests, ModelRequest{ID: "r", Messages: []Message{{Content: strings.Repeat("x", 1<<20)}}})
	}
	var encoded bytes.Buffer
	if err = encodeSnapshot(&encoded, snap); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(store.root, snap.Run.ID+".json"), encoded.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	encoded.Reset()
	updated, err := store.Update(snap.Run.ID, func(v *Snapshot) error { v.Run.Status = StatusPaused; addEvent(v, "run_paused", "", nil); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(updated.Requests, snap.Requests) {
		t.Fatal("large save changed request archive")
	}
	other, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	loaded, err := other.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Requests, snap.Requests) || loaded.Run.Status != StatusPaused || loaded.Events[len(loaded.Events)-1].Type != "run_paused" {
		t.Fatal("large commit lost archive/status/event")
	}
	if obs, err := store.Observe(snap.Run.ID); err != nil || obs.Run.Status != StatusPaused {
		t.Fatal("large commit did not publish observation", err)
	}
	// Force the large-write rename to fail after fsync. The original file is
	// preserved beside a directory occupying the target path until restoration.
	if _, _, err = store.EventBatch(snap.Run.ID, 0); err != nil {
		t.Fatal(err)
	}
	viewBefore, _ := store.eventViews.Load(snap.Run.ID)
	observationBefore, _ := store.observations.Load(snap.Run.ID)
	changed := store.EventChanges(snap.Run.ID)
	path := filepath.Join(store.root, snap.Run.ID+".json")
	backup := path + ".saved"
	store.beforePersist = func(string) {
		if err := os.Rename(path, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	_, err = store.Update(snap.Run.ID, func(v *Snapshot) error { v.Run.Title = "failed publication"; return nil })
	store.beforePersist = nil
	if restoreErr := os.Remove(path); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if restoreErr := os.Rename(backup, path); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("large snapshot rename unexpectedly succeeded")
	}
	if after, _ := store.eventViews.Load(snap.Run.ID); after != viewBefore {
		t.Fatal("failed large save published event view")
	}
	if after, _ := store.observations.Load(snap.Run.ID); after != observationBefore {
		t.Fatal("failed large save published observation")
	}
	select {
	case <-changed:
		t.Fatal("failed large save notified a commit")
	default:
	}
	if leftovers, err := filepath.Glob(filepath.Join(store.root, ".trace-*.tmp")); err != nil || len(leftovers) != 0 {
		t.Fatal("failed large save leaked staged file", err)
	}
}

func TestSQLiteStreamStateExactHistoryAndRollback(t *testing.T) {
	store := sqliteTestStore(t)
	snap, err := store.Create("sqlite stream", "", "")
	if err != nil {
		t.Fatal(err)
	}
	history := activityRequestHistory(16, 16)
	for i := range history.Requests {
		history.Requests[i].Messages[0].Content = strings.Repeat("archived history ", 16384)
	}
	snap, err = store.Update(snap.Run.ID, func(v *Snapshot) error {
		v.Requests, v.Responses, v.Messages = history.Requests, history.Responses, history.Messages
		addEvent(v, "saved", "", nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.sqliteDB()
	if err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := db.QueryRow("SELECT state FROM runs WHERE id=?", snap.Run.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	state := snap
	state.Run, state.Events = Run{}, nil
	want, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, want) {
		t.Fatal("streamed SQL state differs from canonical JSON bytes")
	}
	loaded, err := store.Load(snap.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Requests, snap.Requests) || !reflect.DeepEqual(loaded.Responses, snap.Responses) || loaded.Run.ID != snap.Run.ID || len(loaded.Events) != 1 {
		t.Fatal("borrowed SQL row lost full history, Run override, or events")
	}
	changed := store.EventChanges(snap.Run.ID)
	_, err = store.Update(snap.Run.ID, func(v *Snapshot) error {
		v.Run.Goal = "rollback"
		v.Requests[0].Messages[0].Content = "must not persist"
		v.Events = append(v.Events, v.Events[0])
		return nil
	})
	if err == nil {
		t.Fatal("invalid event sequence committed SQL state")
	}
	var after []byte
	if err := db.QueryRow("SELECT state FROM runs WHERE id=?", snap.Run.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("failed SQL event transaction changed historical state")
	}
	select {
	case <-changed:
		t.Fatal("rolled back SQL state notified a commit")
	default:
	}
}
