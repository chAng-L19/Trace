package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

type jsonSpan struct{ start, end int }

// Used only while decoding immutable, internally owned snapshot bytes. Public
// readers still receive detached copies. Unlike RawMessage this does not copy
// every large JSON field or event before selecting the fields a view needs.
type snapshotJSON []byte

func (r *snapshotJSON) UnmarshalJSON(data []byte) error {
	*r = data
	return nil
}

// Only standard-library JSON decoding determines field boundaries. Retain the
// exact committed bytes so telemetry never re-encodes request/evidence history.
func (v *storedView) retainSnapshot(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	first, err := d.Token()
	if err != nil {
		return err
	}
	if first != json.Delim('{') {
		return errors.New("invalid run snapshot")
	}
	metadata := map[string]json.RawMessage{}
	v.fields = map[string]json.RawMessage{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return err
		}
		var raw snapshotJSON
		if err := d.Decode(&raw); err != nil {
			return err
		}
		span := jsonSpan{int(d.InputOffset()) - len(raw), int(d.InputOffset())}
		// Decoder may reuse its buffer on the next field; retain slices of the
		// original immutable snapshot, never slices of that decoder buffer.
		value := json.RawMessage(data[span.start:span.end])
		v.fields[key.(string)] = value
		switch key.(string) {
		case "run":
			v.runSpan = span
			metadata["run"] = value
		case "events":
			v.eventSpan = span
		case "plan", "workers", "worker_messages", "mailbox":
			metadata[key.(string)] = value
		}
	}
	if _, err := d.Token(); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("invalid trailing snapshot data")
	}
	v.observation, err = json.Marshal(metadata)
	v.snapshot = data
	v.workspaceBase = &workspaceBase{}
	return err
}

func (s *Store) appendCachedEventsLocked(id string, previous *storedView, events []Event) error {
	v, through, err := buildAppendedEvents(id, previous, events)
	if err != nil {
		return err
	}
	parts, err := v.snapshotParts()
	if err != nil {
		return err
	}
	staged, err := s.stageSnapshotLocked(id, parts...)
	if err != nil {
		return err
	}
	defer os.Remove(staged.path)
	return s.publishAppendedEventsLocked(id, v, staged, events, through)
}

func buildAppendedEvents(id string, previous *storedView, events []Event) (*storedView, int, error) {
	var observation Observation
	if err := json.Unmarshal(previous.observation, &observation); err != nil {
		return nil, 0, err
	}
	run := observation.Run
	if run.ID != id {
		return nil, 0, errors.New("snapshot identity mismatch")
	}
	if run.Deleted {
		return nil, 0, errors.New("run was deleted from archive")
	}
	previous.indexEvents()
	if previous.eventsErr != nil {
		return nil, 0, previous.eventsErr
	}
	v := &storedView{snapshot: previous.snapshot, runSpan: previous.runSpan, eventSpan: previous.eventSpan, eventsOrdered: previous.eventsOrdered, workspaceBase: previous.workspaceBase, info: previous.info}
	v.workspaceReady.Store(previous.workspaceReady.Load())
	v.events = make([]snapshotJSON, len(previous.events), len(previous.events)+len(events))
	copy(v.events, previous.events)
	v.sequences = make([]int, len(previous.sequences), len(previous.sequences)+len(events))
	copy(v.sequences, previous.sequences)
	tail := append([]byte(nil), previous.eventTail...)
	for _, event := range events {
		run.EventSeq++
		event.Sequence = run.EventSeq
		if event.CreatedAt.IsZero() {
			event.CreatedAt = time.Now().UTC()
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return nil, 0, err
		}
		if len(v.events) > 0 {
			tail = append(tail, ',')
		}
		tail = append(tail, raw...)
		if len(v.sequences) > 0 && v.sequences[len(v.sequences)-1] >= event.Sequence {
			v.eventsOrdered = false
		}
		v.events = append(v.events, snapshotJSON(raw))
		v.sequences = append(v.sequences, event.Sequence)
	}
	run.UpdatedAt = time.Now().UTC()
	rawRun, err := json.Marshal(run)
	if err != nil {
		return nil, 0, err
	}
	v.eventTail = tail
	v.fields = make(map[string]json.RawMessage, len(previous.fields))
	for key, value := range previous.fields {
		if key != "events" {
			v.fields[key] = value
		}
	}
	v.fields["run"] = rawRun
	v.eventsOnce.Do(func() {})
	observation.Run = run
	v.observation, err = json.Marshal(observation)
	if err != nil {
		return nil, 0, err
	}
	return v, run.EventSeq, nil
}

func (s *Store) appendEventsOptimistic(id string, previous *storedView, events []Event) (bool, error) {
	v, through, err := buildAppendedEvents(id, previous, events)
	if err != nil {
		return false, err
	}
	parts, err := v.snapshotParts()
	if err != nil {
		return false, err
	}
	staged, err := s.stageSnapshot(parts...)
	if err != nil {
		return false, err
	}
	defer os.Remove(staged.path)
	return s.commitAppendedEvents(id, previous, v, staged, events, through)
}

// Generation matching and publication share the semantic writer's locks.
// A stale candidate never exposes its sequence, cache, or notification.
func (s *Store) commitAppendedEvents(id string, previous, v *storedView, staged *stagedSnapshot, events []Event, through int) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return false, err
	}
	defer unlock()
	current, err := os.Stat(filepath.Join(s.root, id+".json"))
	if err != nil {
		return false, err
	}
	if !sameGeneration(previous.info, current) {
		return false, nil
	}
	if s.beforePersist != nil {
		s.beforePersist(id)
	}
	if err := s.publishAppendedEventsLocked(id, v, staged, events, through); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) publishAppendedEventsLocked(id string, v *storedView, staged *stagedSnapshot, events []Event, through int) error {
	if err := s.publishSnapshotViewsLocked(id, staged, v, v.observation); err != nil {
		return err
	}
	s.notifyEventCommit(id)
	s.notifySchedulingCommit(id, events, through)
	return nil
}

// Keep the committed immutable prefix once, as Harness/pi retain a primed log.
// Trace still writes one fsynced atomic JSON snapshot; only in-memory assembly
// is segmented. No journal, second source of truth, or weaker commit boundary.
func (v *storedView) snapshotParts() ([][]byte, error) {
	oldEvents := bytes.TrimSpace(v.snapshot[v.eventSpan.start:v.eventSpan.end])
	var prefix []byte
	if bytes.Equal(oldEvents, []byte("null")) {
		prefix = []byte{'['}
	} else {
		if len(oldEvents) < 2 || oldEvents[0] != '[' || oldEvents[len(oldEvents)-1] != ']' {
			return nil, errors.New("invalid snapshot events")
		}
		prefix = oldEvents[:len(oldEvents)-1]
	}
	event := [][]byte{prefix, v.eventTail, {']'}}
	parts := make([][]byte, 0, 7)
	if v.runSpan.start < v.eventSpan.start {
		parts = append(parts, v.snapshot[:v.runSpan.start], v.fields["run"], v.snapshot[v.runSpan.end:v.eventSpan.start])
		parts = append(parts, event...)
		return append(parts, v.snapshot[v.eventSpan.end:]), nil
	}
	parts = append(parts, v.snapshot[:v.eventSpan.start])
	parts = append(parts, event...)
	parts = append(parts, v.snapshot[v.eventSpan.end:v.runSpan.start], v.fields["run"], v.snapshot[v.runSpan.end:])
	return parts, nil
}
