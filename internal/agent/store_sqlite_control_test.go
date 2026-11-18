package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func sqliteControlFixture(t *testing.T) (*Agent, *Store, Snapshot) {
	t.Helper()
	a, store := treeCapacityAgent(t, "sqlite", MockProvider{})
	id := createTestRun(t, a)
	snap, err := store.Update(id, func(s *Snapshot) error {
		s.Run.Status = StatusRunning
		s.Run.Budget.MaxModelRequests = 12345
		s.Messages = append(s.Messages, Message{ID: "history", Role: "assistant", Content: strings.Repeat("history", 1000)})
		s.Actions = []Action{{CallID: "uncertain", Adapter: "exec", Status: "uncertain", ResourceOwner: "preserved"}}
		addEvent(s, "model_stream", "old event", map[string]any{"delta": "original"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return a, store, snap
}

func sqliteControlBytes(t *testing.T, s *Store, id string) (state, result []byte) {
	t.Helper()
	if err := s.sqlitePool.QueryRow("SELECT r.state,h.result_override FROM runs r JOIN run_headers h ON h.id=r.id WHERE r.id=?", id).Scan(&state, &result); err != nil {
		t.Fatal(err)
	}
	return state, result
}

func TestSQLitePauseControlPreservesStateAndReopens(t *testing.T) {
	a, store, before := sqliteControlFixture(t)
	id := before.Run.ID
	state, _ := sqliteControlBytes(t, store, id)
	_, events, err := store.EventBatch(id, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.ActivityWorkspace(id); err != nil {
		t.Fatal(err)
	}
	paused, err := a.Pause(id)
	if err != nil {
		t.Fatal(err)
	}
	current, result := sqliteControlBytes(t, store, id)
	if !bytes.Equal(state, current) || result == nil {
		t.Fatal("pause rewrote history instead of committing result override")
	}
	if paused.Run.Status != StatusPaused || paused.Result.TerminalReason != "operator_pause" || !reflect.DeepEqual(paused.Actions, before.Actions) || !reflect.DeepEqual(paused.Messages, before.Messages) || paused.Run.Budget != before.Run.Budget {
		t.Fatal("control lost full snapshot, uncertain ledger or budget")
	}
	_, afterEvents, err := store.EventBatch(id, 0)
	if err != nil || len(afterEvents) != len(events)+1 {
		t.Fatal("pause did not append its event", err)
	}
	for i := range events {
		if !bytes.Equal(events[i], afterEvents[i]) {
			t.Fatal("pause changed old event prefix")
		}
	}
	for _, reader := range []*Store{store, func() *Store {
		other, err := NewStore(store.root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { other.Close() })
		return other
	}()} {
		got, err := reader.Load(id)
		if err != nil {
			t.Fatal(err)
		}
		wantJSON, _ := json.Marshal(paused)
		gotJSON, _ := json.Marshal(got)
		if !bytes.Equal(wantJSON, gotJSON) {
			t.Fatal("reopened authoritative snapshot differs from control return")
		}
		projection, err := reader.ActivityWorkspace(id)
		if err != nil || !reflect.DeepEqual(projection.Snapshot.Result, Result{}) || projection.Snapshot.Run.Status != StatusPaused {
			t.Fatal("warm/cold display lost control status or exposed an acceptance result", err)
		}
		// Activity intentionally omits business acceptance. The shared index
		// reader must still merge its authoritative field in the same transaction.
		tx, err := reader.sqlitePool.Begin()
		if err != nil {
			t.Fatal(err)
		}
		index, _, err := readSQLiteDisplayIndex(context.Background(), tx, id)
		tx.Rollback()
		if err != nil {
			t.Fatal(err)
		}
		var indexedResult Result
		if err = json.Unmarshal(index.Fields["result"], &indexedResult); err != nil || !reflect.DeepEqual(indexedResult, paused.Result) {
			t.Fatal("shared index lost authoritative result override", err)
		}
	}
	export := filepath.Join(t.TempDir(), "export")
	if err = store.ExportSQLiteSnapshots(export); err != nil {
		t.Fatal(err)
	}
	jsonStore, err := NewStore(export)
	if err != nil {
		t.Fatal(err)
	}
	exported, err := jsonStore.Load(id)
	if err != nil || !reflect.DeepEqual(exported.Result, paused.Result) {
		t.Fatal("export lost authoritative result", err)
	}
	if _, err = a.Pause(id); err == nil {
		t.Fatal("duplicate operator pause succeeded")
	}
	if _, err = store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; s.Run.PauseReason = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	_, result = sqliteControlBytes(t, store, id)
	if result != nil {
		t.Fatal("full update retained stale result override")
	}
}

func TestSQLitePauseControlChangedBodyFallsBack(t *testing.T) {
	_, store, before := sqliteControlFixture(t)
	state, _ := sqliteControlBytes(t, store, before.Run.ID)
	_, err := store.updatePause(before.Run.ID, func(s *Snapshot) error {
		s.Run.Status = StatusPaused
		s.Run.PauseReason = "operator_pause"
		s.Messages[0].Content = "changed existing history"
		addEvent(s, "run_paused", "control", nil)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	after, result := sqliteControlBytes(t, store, before.Run.ID)
	if bytes.Equal(state, after) || result != nil {
		t.Fatal("changed body used control-only commit")
	}
	got, err := store.Load(before.Run.ID)
	if err != nil || got.Messages[0].Content != "changed existing history" {
		t.Fatal("fallback lost body edit", err)
	}
}

func TestSQLitePauseControlPersistsInitialResultBeforeCallback(t *testing.T) {
	for _, reject := range []bool{false, true} {
		t.Run(map[bool]string{false: "pause", true: "callback-error"}[reject], func(t *testing.T) {
			_, store, before := sqliteControlFixture(t)
			id := before.Run.ID
			state, _ := sqliteControlBytes(t, store, id)
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(state, &fields); err != nil {
				t.Fatal(err)
			}
			fields["result"] = json.RawMessage(`{"status":"incomplete","terminal_reason":"operator_pause"}`)
			state, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.sqlitePool.Exec("UPDATE runs SET state=? WHERE id=?", state, id); err != nil {
				t.Fatal(err)
			}
			if _, err = store.sqlitePool.Exec("CREATE TRIGGER reject_history_update BEFORE UPDATE OF state ON runs BEGIN SELECT RAISE(ABORT, 'must not rewrite history for initial result refresh'); END"); err != nil {
				t.Fatal(err)
			}
			callbackErr := errors.New("callback rejected control")
			called := false
			_, err = store.updatePause(id, func(s *Snapshot) error {
				called = true
				persisted, err := store.sqliteLoad(id)
				if err != nil || !reflect.DeepEqual(persisted.Result, before.Result) {
					t.Fatal("initial verified result was not committed before callback", err)
				}
				if reject {
					return callbackErr
				}
				s.Run.Status, s.Run.PauseReason = StatusPaused, "operator_pause"
				addEvent(s, "run_paused", "control", nil)
				return nil
			})
			if !called || reject && !errors.Is(err, callbackErr) || !reject && err != nil {
				t.Fatal("initial result normalization failed or swallowed callback error", err)
			}
			after, override := sqliteControlBytes(t, store, id)
			got, loadErr := store.sqliteLoad(id)
			if loadErr != nil || !bytes.Equal(state, after) || override == nil {
				t.Fatal("initial result refresh rewrote history or lost override", loadErr)
			}
			if reject && (got.Run.Status != StatusRunning || !reflect.DeepEqual(got.Result, before.Result) || len(got.Events) != len(before.Events)) {
				t.Fatal("rejected callback rolled back prior verification or committed pause")
			}
			if !reject && (got.Run.Status != StatusPaused || got.Result.TerminalReason != "operator_pause" || len(got.Events) != len(before.Events)+1) {
				t.Fatal("pause after initial result refresh did not persist")
			}
		})
	}
}

func TestSQLitePauseControlPersistsRefreshedFrontier(t *testing.T) {
	a, store, before := sqliteControlFixture(t)
	id := before.Run.ID
	before.Plan.Tasks = []PlanTask{{ID: "ready", Status: "pending", Goal: "next"}}
	before.Plan.Frontier = nil
	unlock, err := store.acquireStoreLock()
	if err != nil {
		t.Fatal(err)
	}
	err = store.saveLocked(before)
	unlock()
	if err != nil {
		t.Fatal(err)
	}
	paused, err := a.Pause(id)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := store.sqliteLoad(id)
	if err != nil {
		t.Fatal(err)
	}
	observation, err := store.Observe(id)
	if err != nil || !reflect.DeepEqual(raw.Plan.Frontier, []string{"ready"}) || !reflect.DeepEqual(raw.Plan, paused.Plan) || !reflect.DeepEqual(raw.Plan, observation.Plan) {
		t.Fatal("control reused state that had an unpersisted frontier refresh", err)
	}
	for _, empty := range []bool{false, true} {
		if empty {
			raw.Plan.Tasks, raw.Plan.Frontier = nil, nil
			if err := store.saveLocked(raw); err != nil {
				t.Fatal(err)
			}
		}
		writes := 0
		store.beforePersist = func(string) { writes++ }
		for i := 0; i < 2; i++ {
			if _, err := store.Load(id); err != nil {
				t.Fatal(err)
			}
		}
		store.beforePersist = nil
		if writes != 0 {
			t.Fatalf("stable frontier triggered repeated saves: empty=%t writes=%d", empty, writes)
		}
	}
}

func TestSQLitePauseControlRollbackAndCommitVisibility(t *testing.T) {
	a, store, before := sqliteControlFixture(t)
	id := before.Run.ID
	other, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	state, _ := sqliteControlBytes(t, store, id)
	if _, err = store.sqlitePool.Exec("CREATE TRIGGER reject_pause BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'control fixture failure'); END"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Pause(id); err == nil {
		t.Fatal("accepted failed event commit")
	}
	current, result := sqliteControlBytes(t, store, id)
	observed, err := other.Observe(id)
	if err != nil || result != nil || !bytes.Equal(current, state) || !reflect.DeepEqual(observed.Run, before.Run) {
		t.Fatal("failed pause partially committed", err)
	}
	if _, err = store.sqlitePool.Exec("DROP TRIGGER reject_pause"); err != nil {
		t.Fatal(err)
	}
	store.beforeSQLiteCommit = func() {
		observed, err := other.Observe(id)
		if err != nil || observed.Run.Status != StatusRunning {
			t.Error("other Store observed uncommitted control", err)
		}
	}
	defer func() { store.beforeSQLiteCommit = nil }()
	if _, err = a.Pause(id); err != nil {
		t.Fatal(err)
	}
	observed, err = other.Observe(id)
	if err != nil || observed.Run.Status != StatusPaused {
		t.Fatal("other Store missed committed control", err)
	}
}

func TestSQLitePauseControlCrashRecovery(t *testing.T) {
	if root := os.Getenv("TRACE_PAUSE_CRASH_ROOT"); root != "" {
		store, err := NewStore(root)
		if err != nil {
			t.Fatal(err)
		}
		a, err := New(store, MockProvider{}, "")
		if err != nil {
			t.Fatal(err)
		}
		store.beforeSQLiteCommit = func() { os.Exit(73) }
		_, err = a.Pause(os.Getenv("TRACE_PAUSE_CRASH_ID"))
		t.Fatal("crash barrier was not reached", err)
	}
	_, store, before := sqliteControlFixture(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestSQLitePauseControlCrashRecovery$")
	cmd.Env = append(os.Environ(), "TRACE_PAUSE_CRASH_ROOT="+store.root, "TRACE_PAUSE_CRASH_ID="+before.Run.ID)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 73 {
		t.Fatalf("missing crash exit: %v %s", err, out)
	}
	other, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	got, err := other.Load(before.Run.ID)
	if err != nil || !reflect.DeepEqual(got.Run, before.Run) || !reflect.DeepEqual(got.Result, before.Result) || len(got.Events) != len(before.Events) {
		t.Fatal("crashed control did not roll back atomically", err)
	}
	_, result := sqliteControlBytes(t, other, before.Run.ID)
	if result != nil {
		t.Fatal("crashed result override survived")
	}
}

func TestSQLiteControlSchemaMigration(t *testing.T) {
	root := t.TempDir()
	db, err := openSQLite(filepath.Join(root, sqliteStoreName), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE runs(id TEXT PRIMARY KEY,run BLOB NOT NULL,state BLOB NOT NULL,observation BLOB NOT NULL);
CREATE TABLE events(run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,sequence INTEGER NOT NULL,body BLOB NOT NULL,PRIMARY KEY(run_id,sequence));
CREATE TABLE purge_commits(stage TEXT PRIMARY KEY);
PRAGMA user_version=1;`); err != nil {
		t.Fatal(err)
	}
	before := []byte(`{"messages":[{"content":"legacy"}],"result":{"status":"incomplete"}}`)
	if _, err = db.Exec("INSERT INTO runs VALUES(?,?,?,?)", "legacy", []byte(`{"id":"legacy"}`), before, []byte(`{"run":{"id":"legacy"}}`)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var version int
	if err = store.sqlitePool.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 3 {
		t.Fatal("schema migration was not versioned", version, err)
	}
	after, result := sqliteControlBytes(t, store, "legacy")
	if !bytes.Equal(before, after) || result != nil {
		t.Fatal("schema migration changed original state")
	}
	got, err := store.sqliteLoad("legacy")
	if err != nil || len(got.Messages) != 1 || got.Messages[0].Content != "legacy" {
		t.Fatal("version 1 history no longer loads", err)
	}
}

func TestSQLitePauseControlLegacyFieldsAndExternalReplacement(t *testing.T) {
	a, store, before := sqliteControlFixture(t)
	id := before.Run.ID
	state, _ := sqliteControlBytes(t, store, id)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(state, &fields); err != nil {
		t.Fatal(err)
	}
	fields["run"] = json.RawMessage(`{"error":"legacy error"}`)
	fields["events"] = json.RawMessage(`[{"sequence":1,"type":"legacy"}]`)
	state, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	// The event previously lived in state, not in the SQL events table.
	if _, err = store.sqlitePool.Exec("DELETE FROM events WHERE run_id=?", id); err != nil {
		t.Fatal(err)
	}
	if _, err = store.sqlitePool.Exec("UPDATE runs SET state=? WHERE id=?", state, id); err != nil {
		t.Fatal(err)
	}
	paused, err := a.Pause(id)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.sqliteLoad(id)
	if err != nil || len(got.Events) != len(paused.Events) || !reflect.DeepEqual(got.Run, paused.Run) || !reflect.DeepEqual(got.Result, paused.Result) {
		t.Fatal("legacy embedded headers or events were merged twice", err)
	}
	_, result := sqliteControlBytes(t, store, id)
	if result == nil {
		t.Fatal("legacy control did not use override")
	}
	// An original whole-state replacement also replaces its Result authority.
	fields["result"] = json.RawMessage(`{"status":"incomplete","terminal_reason":"external replacement"}`)
	state, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.sqlitePool.Exec("UPDATE runs SET state=? WHERE id=?", state, id); err != nil {
		t.Fatal(err)
	}
	_, result = sqliteControlBytes(t, store, id)
	got, err = store.sqliteSnapshot(id, false)
	if err != nil || result != nil || got.Result.TerminalReason != "external replacement" {
		t.Fatal("external state replacement retained stale overlay", err)
	}
}

func TestSQLiteControlResultDisplayBudget(t *testing.T) {
	_, store, before := sqliteControlFixture(t)
	id := before.Run.ID
	if _, err := store.ActivityWorkspace(id); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"blob", "text"} {
		t.Run(kind, func(t *testing.T) {
			query, value := "UPDATE runs SET result_override=zeroblob(?) WHERE id=?", any(activityProjectionMaxBytes+1)
			if kind == "text" {
				query, value = "UPDATE runs SET result_override=? WHERE id=?", strings.Repeat("界", activityProjectionMaxBytes/3+1)
			}
			if _, err := store.sqlitePool.Exec(query, value, id); err != nil {
				t.Fatal(err)
			}
			if _, err := store.ActivityWorkspace(id); !errors.Is(err, errActivityTooLarge) {
				t.Fatal("oversized override bypassed display byte budget", err)
			}
			tx, err := store.sqlitePool.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, _, err := readSQLiteDisplayIndex(context.Background(), tx, id); !errors.Is(err, errActivityTooLarge) {
				t.Fatal("direct index projection bypassed display byte budget", err)
			}
		})
	}
}

func TestSQLiteControlSchemaRejectsInvalidVersionAndColumn(t *testing.T) {
	for _, kind := range []string{"missing-column", "missing-control", "unknown-version"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			db, err := openSQLite(filepath.Join(root, sqliteStoreName), true)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE runs(id TEXT PRIMARY KEY,run BLOB NOT NULL,state BLOB NOT NULL,observation BLOB NOT NULL);
CREATE TABLE purge_commits(stage TEXT PRIMARY KEY);
PRAGMA user_version=2;`); err != nil {
				t.Fatal(err)
			}
			if kind == "unknown-version" {
				if _, err = db.Exec("PRAGMA user_version=4"); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "missing-control" {
				if _, err = db.Exec("ALTER TABLE runs ADD COLUMN result_override BLOB; PRAGMA user_version=3;"); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			if store, err := NewStore(root); err == nil {
				store.Close()
				t.Fatal("opened invalid SQLite control schema")
			}
		})
	}
}
