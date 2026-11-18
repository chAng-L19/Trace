package agent

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

const sqliteStoreName = "store.sqlite"
const sqliteStoreMarker = ".sqlite-authority"

// Every pooled connection enforces FULL, including replacement connections.
// Retain an idle connection to avoid last-close WAL checkpoints per event batch.
func openSQLite(path string, create bool) (*sql.DB, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path = absolute
	mode := "rw"
	if create {
		mode = "rwc"
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("mode", mode)
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "foreign_keys(ON)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	if err = db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (s *Store) sqliteDB() (*sql.DB, error) {
	if s.sqlitePool == nil {
		return nil, fmt.Errorf("SQLite Store is not open")
	}
	return s.sqlitePool, nil
}

// Close releases the database after all Store consumers stop. JSON stores have
// no retained handles. Close never changes persisted backend authority.
func (s *Store) Close() error {
	if s.sqliteDisplayConn != nil {
		s.sqliteDisplayConn.Close()
	}
	if s.sqlitePool != nil {
		return s.sqlitePool.Close()
	}
	return nil
}

func (s *Store) sqliteObserve(id string) (Observation, error) {
	db, err := s.sqliteDB()
	if err != nil {
		return Observation{}, err
	}
	var raw []byte
	err = db.QueryRow("SELECT observation FROM run_headers WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = os.ErrNotExist
	}
	if err != nil {
		return Observation{}, err
	}
	var out Observation
	if err = json.Unmarshal(raw, &out); err != nil {
		return out, err
	}
	if out.Run.ID != id {
		return out, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	return out, nil
}

func sqliteReadState(tx *sql.Tx, id string) (Snapshot, error) {
	var snap Snapshot
	var run, result []byte
	err := tx.QueryRow("SELECT run,result_override FROM run_headers WHERE id=?", id).Scan(&run, &result)
	if errors.Is(err, sql.ErrNoRows) {
		return snap, os.ErrNotExist
	}
	if err != nil {
		return snap, err
	}
	// Reuse the bounded authoritative reader in this same transaction. Full
	// Load still retains the typed history, without a second complete state BLOB.
	source := newSQLiteStateReader(context.Background(), tx, id, 0)
	defer source.Close()
	reader := bufio.NewReader(source)
	for {
		first, e := reader.ReadByte()
		if e != nil {
			return snap, e
		}
		if first == ' ' || first == '\t' || first == '\r' || first == '\n' {
			continue
		}
		if err = reader.UnreadByte(); err != nil {
			return snap, err
		}
		// Unlike the JSON-file reader, the original SQLite Unmarshal accepts
		// top-level null; the separate run column is still applied afterwards.
		if first == 'n' {
			d := json.NewDecoder(reader)
			err = d.Decode(&snap)
			if err == nil {
				if _, end := d.Token(); end != io.EOF {
					err = errors.New("invalid trailing snapshot data")
				}
			}
		} else {
			err = decodeSnapshot(reader, &snap)
		}
		break
	}
	if err != nil {
		return snap, err
	}
	if err = source.Close(); err != nil {
		return snap, err
	}
	if result != nil {
		// A control commit has moved these fields to their authoritative columns
		// and event rows, including when the imported state embedded legacy values.
		snap.Run, snap.Events, snap.Result = Run{}, nil, Result{}
		if err = json.Unmarshal(result, &snap.Result); err != nil {
			return snap, err
		}
	}
	if err = json.Unmarshal(run, &snap.Run); err != nil {
		return snap, err
	}
	if snap.Run.ID != id {
		return snap, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	return snap, nil
}

func sqliteReadEvents(tx *sql.Tx, id string, after int) ([]json.RawMessage, error) {
	rows, err := tx.Query("SELECT body FROM events WHERE run_id=? AND sequence>? ORDER BY sequence", id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []json.RawMessage
	for rows.Next() {
		var raw []byte
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		events = append(events, json.RawMessage(raw))
	}
	return events, rows.Err()
}

func (s *Store) sqliteSnapshot(id string, events bool) (Snapshot, error) {
	db, err := s.sqliteDB()
	if err != nil {
		return Snapshot{}, err
	}

	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()
	snap, err := sqliteReadState(tx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if events {
		rows, e := tx.Query("SELECT body FROM events WHERE run_id=? AND sequence>? ORDER BY sequence", id, -1)
		if e != nil {
			return Snapshot{}, e
		}
		defer rows.Close()
		for rows.Next() {
			// Decode before advancing the row; only the final typed events are
			// retained. EventBatch keeps its separate raw-message contract.
			var raw sql.RawBytes
			if e = rows.Scan(&raw); e != nil {
				return Snapshot{}, e
			}
			var event Event
			if e = json.Unmarshal(raw, &event); e != nil {
				return Snapshot{}, e
			}
			snap.Events = append(snap.Events, event)
		}
		if e = rows.Err(); e != nil {
			return Snapshot{}, e
		}
		if e = rows.Close(); e != nil {
			return Snapshot{}, e
		}
	}
	snap.artifactRoot = s.root
	return snap, tx.Commit()
}

func (s *Store) sqliteLoad(id string) (Snapshot, error) { return s.sqliteSnapshot(id, true) }

// Receipts, evidence, budgets and event edits share one database transaction.
// Existing events are compared, not rewritten on every semantic commit.
func sqliteWriteSnapshot(tx *sql.Tx, snap Snapshot) error {
	if snap.Run.ID == "" {
		return fmt.Errorf("empty snapshot identity")
	}
	events := snap.Events
	snap.Events = nil
	identity := snap.Run
	run, err := json.Marshal(snap.Run)
	if err != nil {
		return err
	}
	observation, err := json.Marshal(Observation{Run: snap.Run, Plan: snap.Plan, Workers: snap.Workers, WorkerMessages: snap.WorkerMessages, Mailbox: snap.Mailbox})
	if err != nil {
		return err
	}
	snap.Run = Run{}
	// Keep one owned SQL parameter buffer, without json.Marshal's complete
	// encoder buffer plus a second copy of the complete state value.
	// Count with the same record encoder before allocating: growing a 229 MiB
	// buffer retains its old and new allocations together at the peak.
	var size snapshotByteCount
	if err = encodeSnapshot(&size, snap); err != nil {
		return err
	}
	state := bytes.NewBuffer(make([]byte, 0, int(size)))
	if err = encodeSnapshot(state, snap); err != nil {
		return err
	}
	if state.Len() != int(size) {
		return fmt.Errorf("snapshot changed during SQL state encoding")
	}
	if _, err = tx.Exec("INSERT INTO runs(id,run,state,observation) VALUES(?,?,?,?) ON CONFLICT(id) DO UPDATE SET run=excluded.run,state=excluded.state,observation=excluded.observation,result_override=NULL", identity.ID, run, state.Bytes(), observation); err != nil {
		return err
	}
	if err = sqliteStageDisplayIndex(tx, identity.ID, state.Bytes()); err != nil {
		return err
	}
	return sqliteWriteEvents(tx, identity, events)
}

func sqliteWriteEvents(tx *sql.Tx, identity Run, events []Event) error {
	rows, err := tx.Query("SELECT sequence,body FROM events WHERE run_id=?", identity.ID)
	if err != nil {
		return err
	}
	previous := map[int][]byte{}
	for rows.Next() {
		var sequence int
		var body []byte
		if err = rows.Scan(&sequence, &body); err != nil {
			rows.Close()
			return err
		}
		previous[sequence] = body
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	seen := map[int]bool{}
	for _, event := range events {
		if event.Sequence <= 0 || event.Sequence > identity.EventSeq || seen[event.Sequence] {
			return fmt.Errorf("invalid event sequence %d", event.Sequence)
		}
		seen[event.Sequence] = true
		raw, e := json.Marshal(event)
		if e != nil {
			return e
		}
		if !bytes.Equal(previous[event.Sequence], raw) {
			if _, e = tx.Exec("INSERT INTO events(run_id,sequence,body) VALUES(?,?,?) ON CONFLICT(run_id,sequence) DO UPDATE SET body=excluded.body", identity.ID, event.Sequence, raw); e != nil {
				return e
			}
		}
		delete(previous, event.Sequence)
	}
	for sequence := range previous {
		if _, err = tx.Exec("DELETE FROM events WHERE run_id=? AND sequence=?", identity.ID, sequence); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) sqliteSave(snap Snapshot) error {
	if s.beforePersist != nil {
		s.beforePersist(snap.Run.ID)
	}
	db, err := s.sqliteDB()
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = sqliteWriteSnapshot(tx, snap); err != nil {
		return err
	}
	if s.beforeSQLiteCommit != nil {
		s.beforeSQLiteCommit()
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.notifyEventCommit(snap.Run.ID)
	s.notifySchedulingCommit(snap.Run.ID, snap.Events, snap.Run.EventSeq)
	return nil
}

func (s *Store) sqliteAppend(id string, events []Event) error {
	if len(events) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return err
	}
	defer unlock()
	if s.beforePersist != nil {
		s.beforePersist(id)
	}
	db, err := s.sqliteDB()
	if err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var runRaw, observationRaw []byte
	err = tx.QueryRow("SELECT run,observation FROM run_headers WHERE id=?", id).Scan(&runRaw, &observationRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return os.ErrNotExist
	}
	if err != nil {
		return err
	}
	var run Run
	var observation Observation
	if err = json.Unmarshal(runRaw, &run); err != nil {
		return err
	}
	if err = json.Unmarshal(observationRaw, &observation); err != nil {
		return err
	}
	if run.ID != id {
		return fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	if run.Deleted {
		return fmt.Errorf("run was deleted from archive")
	}
	committed := make([]Event, len(events))
	for i, event := range events {
		run.EventSeq++
		event.Sequence = run.EventSeq
		if event.CreatedAt.IsZero() {
			event.CreatedAt = time.Now().UTC()
		}
		raw, e := json.Marshal(event)
		if e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO events(run_id,sequence,body) VALUES(?,?,?)", id, event.Sequence, raw); e != nil {
			return e
		}
		committed[i] = event
	}
	run.UpdatedAt = time.Now().UTC()
	observation.Run = run
	runRaw, err = json.Marshal(run)
	if err != nil {
		return err
	}
	observationRaw, err = json.Marshal(observation)
	if err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO run_control(run_id,run,observation) VALUES(?,?,?) ON CONFLICT(run_id) DO UPDATE SET run=excluded.run,observation=excluded.observation", id, runRaw, observationRaw); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	s.notifyEventCommit(id)
	s.notifySchedulingCommit(id, committed, run.EventSeq)
	return nil
}

func (s *Store) sqliteEventBatch(id string, after int) (Run, []json.RawMessage, error) {
	db, err := s.sqliteDB()
	if err != nil {
		return Run{}, nil, err
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Run{}, nil, err
	}
	defer tx.Rollback()
	var raw []byte
	err = tx.QueryRow("SELECT run FROM run_headers WHERE id=?", id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		err = os.ErrNotExist
	}
	if err != nil {
		return Run{}, nil, err
	}
	var run Run
	if err = json.Unmarshal(raw, &run); err != nil {
		return Run{}, nil, err
	}
	if run.ID != id || run.Deleted {
		return Run{}, nil, os.ErrNotExist
	}
	events, err := sqliteReadEvents(tx, id, after)
	if err != nil {
		return run, nil, err
	}
	return run, events, tx.Commit()
}

func (s *Store) sqliteIDs() ([]string, error) {
	db, err := s.sqliteDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT id FROM runs ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (s *Store) sqliteList(includeDeleted bool) ([]Run, error) {
	db, err := s.sqliteDB()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query("SELECT id,run FROM run_headers")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []Run
	for rows.Next() {
		var id string
		var raw []byte
		if err = rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		var run Run
		if err = json.Unmarshal(raw, &run); err != nil {
			return nil, err
		}
		if run.ID != id {
			return nil, fmt.Errorf("snapshot identity mismatch: %s", id)
		}
		if includeDeleted || !run.Deleted {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	return runs, rows.Err()
}

func (s *Store) sqliteView(id string, events bool) (*storedView, error) {
	snap, err := s.sqliteSnapshot(id, events)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	v := &storedView{}
	if err = v.retainSnapshot(raw); err != nil {
		return nil, err
	}
	return v, nil
}

// Keep historical event bodies raw for workspace projection; telemetry never
// decodes then re-marshals a 50k event history just to select its display tail.
func (s *Store) sqliteWorkspace(id string, includeEvents bool) (Snapshot, error) {
	db, err := s.sqliteDB()
	if err != nil {
		return Snapshot{}, err
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback()
	snap, err := sqliteReadState(tx, id)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Run.Deleted {
		return Snapshot{}, os.ErrNotExist
	}
	if len(snap.Evidence)+len(snap.EvidenceChains)+len(snap.EvidenceChainRefs)+len(snap.Assets)+len(snap.Workers)+len(snap.Plan.Tasks)+len(snap.Requirements)+len(snap.Checks)+len(snap.Findings) > 0 {
		tx.Rollback()
		return s.Load(id)
	}
	previous := snap.Result
	refreshFrontier(&snap.Plan)
	refreshResult(&snap)
	if !reflect.DeepEqual(previous, snap.Result) {
		tx.Rollback()
		return s.Load(id)
	}
	if includeEvents {
		raw, e := sqliteReadEvents(tx, id, -1)
		if e != nil {
			return Snapshot{}, e
		}
		v := &storedView{events: make([]snapshotJSON, len(raw))}
		for i, event := range raw {
			v.events[i] = snapshotJSON(event)
		}
		v.eventsOnce.Do(func() {})
		snap.Events, err = v.workspaceEventProjection(snap)
		if err != nil {
			return Snapshot{}, err
		}
	}
	snap.artifactRoot = s.root
	return snap, tx.Commit()
}
