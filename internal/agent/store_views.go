package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Observation is scheduling metadata, never proof of evidence validity. Reads
// follow the atomic snapshot generation, including writes from another CLI.
type Observation struct {
	Run            Run             `json:"run"`
	Plan           PlanState       `json:"plan"`
	Workers        []AgentWorker   `json:"workers,omitempty"`
	WorkerMessages []WorkerMessage `json:"worker_messages,omitempty"`
	Mailbox        []QueuedMessage `json:"mailbox,omitempty"`
}

type storedView struct {
	workspaceBase   *workspaceBase
	workspaceOnce   sync.Once
	workspaceEvents []Event
	workspaceErr    error
	workspaceReady  atomic.Pointer[workspaceProjection]
	snapshot        []byte
	eventTail       []byte
	runSpan         jsonSpan
	eventSpan       jsonSpan
	info            os.FileInfo
	fields          map[string]json.RawMessage
	observation     json.RawMessage
	eventsOnce      sync.Once
	events          []snapshotJSON
	sequences       []int
	eventsOrdered   bool
	eventsErr       error
}

func sameGeneration(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime())
}

func (s *Store) viewFields(id string, events bool) (*storedView, error) {
	return s.readViewFields(id, events, true)
}

func (s *Store) readViewFields(id string, events, cacheResult bool) (*storedView, error) {
	if s.sqlite {
		return s.sqliteView(id, events)
	}
	if id == "" || strings.ContainsAny(id, `/\\`) || id == "." || id == ".." {
		return nil, os.ErrNotExist
	}
	// ponytail: serialize cold views in this Store; the cache is rechecked
	// after admission, so overlapping readers share the selected generation.
	s.viewReadMu.Lock()
	defer s.viewReadMu.Unlock()
	path := filepath.Join(s.root, id+".json")
	s.eventViewMu.Lock()
	info, err := os.Stat(path)
	if err != nil {
		s.eventViewMu.Unlock()
		return nil, err
	}
	cache := &s.observations
	if events {
		cache = &s.eventViews
	}
	previous, existed := cache.Load(id)
	if existed {
		cached := previous
		v := cached.(*storedView)
		if sameGeneration(v.info, info) {
			s.eventViewMu.Unlock()
			return v, nil
		}
	}
	epoch := s.eventViewEpoch
	// Open the selected immutable generation before allowing a local rename;
	// decoding its bytes never holds the publication lock.
	f, err := os.Open(path)
	s.eventViewMu.Unlock()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil {
		return nil, err
	}
	if !events {
		fields := map[string]json.RawMessage{}
		d := json.NewDecoder(f)
		if err := readJSONObject(d, func(name string) error {
			switch name {
			case "run", "plan", "workers", "worker_messages", "mailbox":
				var raw json.RawMessage
				if err := d.Decode(&raw); err != nil {
					return err
				}
				fields[name] = raw
				return nil
			default:
				return discardJSON(d)
			}
		}); err != nil {
			return nil, err
		}
		var run Run
		if err := json.Unmarshal(fields["run"], &run); err != nil {
			return nil, err
		}
		if run.ID == "" {
			return nil, os.ErrNotExist
		}
		if run.ID != id {
			return nil, fmt.Errorf("snapshot identity mismatch: %s", id)
		}
		metadata, err := json.Marshal(fields)
		if err != nil {
			return nil, err
		}
		return s.finishReadView(id, false, cacheResult, epoch, &storedView{info: info, observation: metadata}), nil
	}
	var source io.Reader = f
	var snapshot []byte
	if events {
		snapshot = make([]byte, info.Size())
		_, err = io.ReadFull(f, snapshot)
		if err != nil {
			return nil, err
		}
		var extra [1]byte
		if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
			return nil, errors.New("snapshot size changed during read")
		}
		source = bytes.NewReader(snapshot)
	}
	d := json.NewDecoder(source)
	first, err := d.Token()
	if err != nil {
		return nil, err
	}
	if first != json.Delim('{') {
		return nil, errors.New("invalid run snapshot")
	}
	fields := map[string]json.RawMessage{}
	var runSpan, eventSpan jsonSpan
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, err
		}
		var value snapshotJSON
		if err = d.Decode(&value); err != nil {
			return nil, err
		}
		name := key.(string)
		span := jsonSpan{int(d.InputOffset()) - len(value), int(d.InputOffset())}
		if name == "run" {
			runSpan = span
		}
		if name == "events" {
			eventSpan = span
		}
		switch name {
		case "run", "plan", "workers", "worker_messages", "mailbox":
			if events {
				fields[name] = snapshot[span.start:span.end]
			} else {
				fields[name] = append(json.RawMessage(nil), value...)
			}
		case "events":
			if events {
				fields[name] = snapshot[span.start:span.end]
			}
		default:
			if events {
				fields[name] = snapshot[span.start:span.end]
			}
		}
	}
	if _, err = d.Token(); err != nil {
		return nil, err
	}
	if err = d.Decode(new(any)); err != io.EOF {
		return nil, errors.New("invalid trailing snapshot data")
	}
	if fields["run"] == nil {
		return nil, os.ErrNotExist
	}
	var run Run
	if err = json.Unmarshal(fields["run"], &run); err != nil {
		return nil, err
	}
	if run.ID == "" {
		return nil, os.ErrNotExist
	}
	if run.ID != id {
		return nil, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	v := &storedView{info: info, fields: fields, snapshot: snapshot, runSpan: runSpan, eventSpan: eventSpan, workspaceBase: &workspaceBase{}}
	metadata := make(map[string]json.RawMessage, len(fields))
	for key, value := range fields {
		if key == "run" || key == "plan" || key == "workers" || key == "worker_messages" || key == "mailbox" {
			metadata[key] = value
		}
	}
	v.observation, err = json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	if !events {
		v.fields = nil
	}
	return s.finishReadView(id, events, cacheResult, epoch, v), nil
}

func (s *Store) finishReadView(id string, events, cacheResult bool, epoch uint64, v *storedView) *storedView {
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	info, err := os.Stat(filepath.Join(s.root, id+".json"))
	if err != nil {
		return v
	}
	cache := &s.observations
	if events {
		cache = &s.eventViews
	}
	// Prefer a writer's primed view (or another cold reader's shared once),
	// even when this reader opened the preceding committed generation.
	if current, ok := cache.Load(id); ok && sameGeneration(current.(*storedView).info, info) {
		return current.(*storedView)
	}
	if cacheResult && sameGeneration(v.info, info) && (!events || epoch == s.eventViewEpoch || s.eventReaders[id] > 0) {
		cache.Store(id, v)
	}
	return v
}

func (s *Store) Observe(id string) (Observation, error) {
	if s.sqlite {
		return s.sqliteObserve(id)
	}
	v, err := s.viewFields(id, false)
	if err != nil {
		return Observation{}, err
	}
	// Decode a detached projection: callers cannot mutate cached maps/slices.
	var out Observation
	err = json.Unmarshal(v.observation, &out)
	return out, err
}

// The session rail needs only Run, not request bodies or event history. Canonical
// snapshots put Run before those large fields. This is a metadata projection,
// not validation of the unread history; Load remains the full snapshot reader.
func (s *Store) listRun(id string) (Run, error) {
	if id == "" || strings.ContainsAny(id, `/\\`) || id == "." || id == ".." {
		return Run{}, os.ErrNotExist
	}
	if s.sqlite {
		observation, err := s.sqliteObserve(id)
		return observation.Run, err
	}
	path := filepath.Join(s.root, id+".json")
	f, err := os.Open(path)
	if err != nil {
		return Run{}, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return Run{}, err
	}
	var run Run
	if cached, ok := s.observations.Load(id); ok && sameGeneration(cached.(*storedView).info, info) {
		var header struct{ Run Run }
		err = json.Unmarshal(cached.(*storedView).observation, &header)
		run = header.Run
	} else {
		d := json.NewDecoder(f)
		first, err := d.Token()
		if err != nil {
			return Run{}, err
		}
		if first != json.Delim('{') {
			return Run{}, errors.New("invalid run snapshot")
		}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return Run{}, err
			}
			if key == "run" {
				if err := d.Decode(&run); err != nil {
					return Run{}, err
				}
				break
			}
			var skipped json.RawMessage
			if err := d.Decode(&skipped); err != nil {
				return Run{}, err
			}
		}
	}
	if err != nil {
		return Run{}, err
	}
	if run.ID == "" {
		return Run{}, os.ErrNotExist
	}
	if run.ID != id {
		return Run{}, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	return run, nil
}

// Cache detached event bytes and the sequence index once per file generation.
// Reference: pi 8a7b0c0 session/jsonl/storage.ts applyCommit and DeepSeek Harness
// 5719020 session-persistence-jsonl/storage.ts materialized committed views.
// Trace keeps its JSON file format; unknown/reordered fields still scan fully.
func (v *storedView) indexEvents() {
	v.eventsOnce.Do(func() {
		if raw := v.fields["events"]; len(raw) > 0 {
			if v.eventsErr = json.Unmarshal(raw, &v.events); v.eventsErr != nil {
				return
			}
		}
		v.eventsOrdered = true
		known := v.sequences != nil && len(v.sequences) == len(v.events)
		if !known {
			v.sequences = make([]int, len(v.events))
		}
		for i, raw := range v.events {
			if !known {
				var header struct {
					Sequence int `json:"sequence"`
				}
				if v.eventsErr = json.Unmarshal(raw, &header); v.eventsErr != nil {
					return
				}
				v.sequences[i] = header.Sequence
			}
			if i > 0 && v.sequences[i-1] >= v.sequences[i] {
				v.eventsOrdered = false
			}
		}
	})
}

// EventBatch returns only committed event bytes. Its cursor is never advanced
// by a preview still waiting to be persisted. No execution provenance is inferred.
func (s *Store) EventBatch(id string, after int) (Run, []json.RawMessage, error) {
	if s.sqlite {
		return s.sqliteEventBatch(id, after)
	}
	if run, empty, err := s.indexedEventWatermark(id, after); err != nil || empty {
		return run, nil, err
	}
	v, err := s.viewFields(id, true)
	if err != nil {
		return Run{}, nil, err
	}
	var run Run
	if err = json.Unmarshal(v.fields["run"], &run); err != nil {
		return run, nil, err
	}
	if run.Deleted {
		return run, nil, os.ErrNotExist
	}
	v.indexEvents()
	if v.eventsErr != nil {
		return run, nil, v.eventsErr
	}
	if after >= run.EventSeq {
		return run, nil, nil
	}
	start := 0
	if v.eventsOrdered {
		start = sort.Search(len(v.sequences), func(i int) bool { return v.sequences[i] > after })
	}
	var batch []json.RawMessage
	for i := start; i < len(v.events); i++ {
		if v.sequences[i] > after {
			batch = append(batch, append(json.RawMessage(nil), v.events[i]...))
		}
	}
	return run, batch, nil
}

// AppendEvents is a telemetry-only transaction. It never runs actions or
// promotes derived evidence. Execution and evidence reads still use Load/Update.
func (s *Store) AppendEvents(id string, events []Event) error {
	if s.sqlite {
		return s.sqliteAppend(id, events)
	}
	if len(events) == 0 {
		return nil
	}
	// One retry absorbs a semantic or cross-process interleave. Busy runs then
	// use the original locked transaction instead of an unbounded retry loop.
	for attempt := 0; attempt < 2; attempt++ {
		// The writer may reuse a committed view but never installs a cold one:
		// a concurrent reader release must not be undone by this candidate.
		previous, err := s.readViewFields(id, true, false)
		if err != nil {
			// Locked fallback also recovers an interrupted purge before reading.
			break
		}
		if len(previous.snapshot) == 0 || previous.runSpan.end == 0 || previous.eventSpan.end == 0 {
			break
		}
		committed, err := s.appendEventsOptimistic(id, previous, events)
		if err != nil || committed {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return err
	}
	defer unlock()
	// Observe identity while holding the same cross-process lock as the load.
	// A stale view is never treated as the prefix of another CLI's generation.
	var previous *storedView
	if cached, ok := s.eventViews.Load(id); ok {
		if info, err := os.Stat(filepath.Join(s.root, id+".json")); err == nil && sameGeneration(cached.(*storedView).info, info) {
			previous = cached.(*storedView)
		}
	}
	if previous != nil && len(previous.snapshot) > 0 && previous.runSpan.end > 0 && previous.eventSpan.end > 0 {
		return s.appendCachedEventsLocked(id, previous, events)
	}
	// Cold telemetry uses the same exact-byte splice as an active feed, rather
	// than decoding and re-encoding every historical body under the store lock.
	// A temporary cold view is released unless an actual SSE reader holds it.
	previous, err = s.viewFields(id, true)
	if err != nil {
		return err
	}
	defer s.ReleaseEventView(id)
	if len(previous.snapshot) > 0 && previous.runSpan.end > 0 && previous.eventSpan.end > 0 {
		return s.appendCachedEventsLocked(id, previous, events)
	}
	snap, err := s.loadRawLocked(id)
	if err != nil {
		return err
	}
	if snap.Run.ID != id {
		return errors.New("snapshot identity mismatch")
	}
	if snap.Run.Deleted {
		return errors.New("run was deleted from archive")
	}
	for _, event := range events {
		snap.Run.EventSeq++
		event.Sequence = snap.Run.EventSeq
		if event.CreatedAt.IsZero() {
			event.CreatedAt = time.Now().UTC()
		}
		snap.Events = append(snap.Events, event)
	}
	snap.Run.UpdatedAt = time.Now().UTC()
	return s.saveLocked(snap)
}

// HoldEventView prevents another stream's disconnect or history read from
// discarding a live stream's committed prefix. The same short lock fences
// rename/cache publication; encoding, cold reads and fsync remain outside it.
func (s *Store) HoldEventView(id string) func() {
	s.eventViewMu.Lock()
	if s.eventReaders == nil {
		s.eventReaders = map[string]int{}
	}
	s.eventReaders[id]++
	s.eventViewMu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			s.eventViewMu.Lock()
			defer s.eventViewMu.Unlock()
			s.eventReaders[id]--
			if s.eventReaders[id] == 0 {
				s.eventViewEpoch++
				delete(s.eventReaders, id)
				delete(s.eventChanges, id)
				s.eventViews.Delete(id)
			}
		})
	}
}

func (s *Store) ReleaseEventView(id string) {
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	if s.eventReaders[id] == 0 {
		s.eventViewEpoch++
		s.eventViews.Delete(id)
	}
}

// Muteki core/event_bus.py publishes only after its required sink commits.
// This is a wakeup, not an event queue: readers always fetch the durable cursor.
// Cross-process changes remain covered by SSE's existing polling fallback.
func (s *Store) EventChanges(id string) <-chan struct{} {
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	if s.eventChanges == nil {
		s.eventChanges = map[string]chan struct{}{}
	}
	if s.eventChanges[id] == nil {
		s.eventChanges[id] = make(chan struct{})
	}
	return s.eventChanges[id]
}

func (s *Store) notifyEventCommit(id string) {
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	if changed := s.eventChanges[id]; changed != nil {
		close(changed)
		s.eventChanges[id] = make(chan struct{})
	}
}
