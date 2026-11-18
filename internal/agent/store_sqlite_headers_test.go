package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestSQLiteControlRowsAvoidHistoryUpdateAndPreserveReaders(t *testing.T) {
	a, store, before := sqliteControlFixture(t)
	id := before.Run.ID
	state, _ := sqliteControlBytes(t, store, id)
	if _, err := store.sqlitePool.Exec("CREATE TRIGGER reject_runs_update BEFORE UPDATE ON runs BEGIN SELECT RAISE(ABORT, 'control must not update history row'); END"); err != nil {
		t.Fatal(err)
	}
	paused, err := a.Pause(id)
	if err != nil {
		t.Fatal(err)
	}
	if err = store.AppendEvents(id, []Event{{Type: "model_stream", Data: "late telemetry"}}); err != nil {
		t.Fatal(err)
	}
	after, result := sqliteControlBytes(t, store, id)
	if !bytes.Equal(state, after) || result == nil {
		t.Fatal("control/telemetry rewrote history or lost result")
	}
	run, events, err := store.EventBatch(id, paused.Run.EventSeq)
	if err != nil || run.Status != StatusPaused || len(events) != 1 || run.EventSeq != paused.Run.EventSeq+1 {
		t.Fatal("event reader missed current control row", err)
	}
	obs, err := store.Observe(id)
	if err != nil || !reflect.DeepEqual(obs.Run, run) {
		t.Fatal("observation missed current control row", err)
	}
	listed, err := store.List()
	if err != nil || len(listed) != 1 || !reflect.DeepEqual(listed[0], run) {
		t.Fatal("list missed current control row", err)
	}
	usage, err := store.readUsageSnapshot(id)
	if err != nil || !reflect.DeepEqual(usage.Run, run) {
		t.Fatal("usage read ignored current control row", err)
	}
	current, err := store.Load(id)
	if err != nil || !reflect.DeepEqual(current.Run, run) || !reflect.DeepEqual(current.Result, paused.Result) {
		t.Fatal("full read lost control or telemetry result", err)
	}
	projection, err := store.ActivityWorkspace(id)
	if err != nil || !reflect.DeepEqual(projection.Snapshot.Run, run) {
		t.Fatal("display missed current control row", err)
	}
	tx, err := store.sqlitePool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	budget := activityProjectionMaxBytes
	capacity, err := store.loadCapacityLocked(context.Background(), tx, id, &budget)
	tx.Rollback()
	wantCapacityRun := Run{ID: run.ID, ParentRunID: run.ParentRunID, ParentTaskID: run.ParentTaskID, Status: run.Status, Deleted: run.Deleted}
	if err != nil || !reflect.DeepEqual(capacity.Run, wantCapacityRun) {
		t.Fatal("capacity read ignored current control row", err)
	}
	if _, err = store.sqlitePool.Exec("DROP TRIGGER reject_runs_update"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Update(id, func(s *Snapshot) error { s.Run.Status = StatusRunning; s.Run.PauseReason = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	var overlays int
	if err = store.sqlitePool.QueryRow("SELECT count(*) FROM run_control WHERE run_id=? AND (run IS NOT NULL OR observation IS NOT NULL OR result IS NOT NULL)", id).Scan(&overlays); err != nil || overlays != 0 {
		t.Fatal("full save retained stale control fields", overlays, err)
	}
	if _, err = a.Pause(id); err != nil {
		t.Fatal(err)
	}
	if err = store.sqliteCommitPurge(filepath.Join(store.root, "header-purge"), []string{id}); err != nil {
		t.Fatal(err)
	}
	if err = store.sqlitePool.QueryRow("SELECT count(*) FROM run_control WHERE run_id=?", id).Scan(&overlays); err != nil || overlays != 0 {
		t.Fatal("purge retained control row", overlays, err)
	}
	if _, err = store.Observe(id); !os.IsNotExist(err) {
		t.Fatal("purged control remained readable", err)
	}
}

func TestSQLiteControlExternalColumnsInvalidateIndependently(t *testing.T) {
	for _, field := range []string{"run", "observation", "result_override", "state"} {
		t.Run(field, func(t *testing.T) {
			a, store, before := sqliteControlFixture(t)
			id := before.Run.ID
			paused, err := a.Pause(id)
			if err != nil {
				t.Fatal(err)
			}
			externalRun := before.Run
			externalRun.Goal = "external header"
			run, _ := json.Marshal(externalRun)
			observation, _ := json.Marshal(Observation{Run: externalRun})
			result := []byte(`{"status":"incomplete","terminal_reason":"external result"}`)
			var value []byte
			switch field {
			case "run":
				value = run
			case "observation":
				value = observation
			case "result_override":
				value = result
			case "state":
				before.Run, before.Events = Run{}, nil
				before.Result = Result{Status: "incomplete", TerminalReason: "external result"}
				value, err = json.Marshal(before)
				if err != nil {
					t.Fatal(err)
				}
			}
			if _, err = store.sqlitePool.Exec("UPDATE runs SET "+field+"=? WHERE id=?", value, id); err != nil {
				t.Fatal(err)
			}
			got, err := store.sqliteLoad(id)
			if err != nil {
				t.Fatal(err)
			}
			wantRun, wantResult := paused.Run, paused.Result
			if field == "run" {
				wantRun = externalRun
			}
			if field == "state" || field == "result_override" {
				wantResult = Result{Status: "incomplete", TerminalReason: "external result"}
			}
			if !reflect.DeepEqual(got.Run, wantRun) || !reflect.DeepEqual(got.Result, wantResult) {
				t.Fatal("base-column update reset an unrelated control field")
			}
			obs, err := store.Observe(id)
			wantObserved := paused.Run
			if field == "observation" {
				wantObserved = externalRun
			}
			if err != nil || !reflect.DeepEqual(obs.Run, wantObserved) {
				t.Fatal("observation invalidation crossed independent fields", err)
			}
		})
	}
}

func TestSQLiteControlVersionTwoFallbackAndNoHistoryMigration(t *testing.T) {
	root := t.TempDir()
	db, err := openSQLite(filepath.Join(root, sqliteStoreName), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE runs(id TEXT PRIMARY KEY,run BLOB NOT NULL,state BLOB NOT NULL,observation BLOB NOT NULL,result_override BLOB);
CREATE TABLE events(run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,sequence INTEGER NOT NULL,body BLOB NOT NULL,PRIMARY KEY(run_id,sequence));
CREATE TABLE purge_commits(stage TEXT PRIMARY KEY);
CREATE TRIGGER run_control_state_changed AFTER UPDATE OF state ON runs BEGIN
 UPDATE runs SET result_override=NULL WHERE id=NEW.id;
END;
PRAGMA user_version=2;`); err != nil {
		t.Fatal(err)
	}
	state := []byte(`{"messages":[{"content":"legacy"}],"result":{"status":"incomplete","terminal_reason":"old state result"}}`)
	run := []byte(`{"id":"legacy","status":"paused","pause_reason":"operator_pause"}`)
	observation := []byte(`{"run":{"id":"legacy","status":"paused","pause_reason":"operator_pause"}}`)
	result := []byte(`{"status":"incomplete","terminal_reason":"operator_pause"}`)
	if _, err = db.Exec("INSERT INTO runs VALUES(?,?,?,?,?)", "legacy", run, state, observation, result); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TRIGGER reject_migration_update BEFORE UPDATE ON runs BEGIN SELECT RAISE(ABORT, 'migration must leave history untouched'); END"); err != nil {
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
		t.Fatal("version 2 migration failed", version, err)
	}
	after, overlay := sqliteControlBytes(t, store, "legacy")
	if !bytes.Equal(state, after) || !bytes.Equal(result, overlay) {
		t.Fatal("version 2 migration rewrote history or lost result")
	}
	if err = store.AppendEvents("legacy", []Event{{Type: "model_stream", Data: "late"}}); err != nil {
		t.Fatal(err)
	}
	got, err := store.sqliteLoad("legacy")
	if err != nil || got.Run.Status != StatusPaused || got.Run.EventSeq != 1 || got.Result.TerminalReason != "operator_pause" {
		t.Fatal("telemetry lost legacy version 2 result", err)
	}
	if _, err = store.sqlitePool.Exec("INSERT INTO run_control(run_id,run) VALUES('missing',?)", run); err == nil {
		t.Fatal("orphan control row bypassed authoritative run identity")
	}
	if _, err = store.sqlitePool.Exec("DROP TRIGGER reject_migration_update"); err != nil {
		t.Fatal(err)
	}
	if _, err = store.sqlitePool.Exec("UPDATE run_control SET result=? WHERE run_id='legacy'", result); err != nil {
		t.Fatal(err)
	}
	if _, err = store.sqlitePool.Exec("UPDATE runs SET state=? WHERE id='legacy'", state); err != nil {
		t.Fatal(err)
	}
	_, overlay = sqliteControlBytes(t, store, "legacy")
	if overlay != nil {
		t.Fatal("old version 2 state trigger was not replaced")
	}
}

func TestSQLiteControlReadKeepsTransactionGeneration(t *testing.T) {
	a, store, before := sqliteControlFixture(t)
	id := before.Run.ID
	tx, err := store.sqlitePool.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var pinned []byte
	if err = tx.QueryRow("SELECT run FROM run_headers WHERE id=?", id).Scan(&pinned); err != nil {
		t.Fatal(err)
	}
	paused, err := a.Pause(id)
	if err != nil {
		t.Fatal(err)
	}
	old, err := sqliteReadState(tx, id)
	if err != nil || !reflect.DeepEqual(old.Run, before.Run) || !reflect.DeepEqual(old.Result, before.Result) {
		t.Fatal("read transaction mixed original state and newer control fields", err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	current, err := store.sqliteLoad(id)
	if err != nil || !reflect.DeepEqual(current.Run, paused.Run) || !reflect.DeepEqual(current.Result, paused.Result) {
		t.Fatal("new transaction missed committed control fields", err)
	}
}
