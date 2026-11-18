package agent

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// Version 3 keeps control fields in a small row in the original database.
// Updating the old runs row copies its large state inside SQLite even when the
// statement does not assign state. Migration only adds schema, not history I/O.
func ensureSQLiteControlSchema(db *sql.DB, version int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 1 {
		if _, err = tx.Exec("ALTER TABLE runs ADD COLUMN result_override BLOB"); err != nil {
			return err
		}
	} else if version != 2 && version != 3 {
		return fmt.Errorf("unsupported SQLite control schema: %d", version)
	}
	// Reject a version marker that lacks its authoritative column.
	rows, err := tx.Query("SELECT result_override FROM runs LIMIT 0")
	if err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if version == 3 {
		rows, err = tx.Query("SELECT run_id,run,observation,result FROM run_control LIMIT 0")
		if err != nil {
			return err
		}
		if err = rows.Close(); err != nil {
			return err
		}
	} else if _, err = tx.Exec("DROP TRIGGER IF EXISTS run_control_state_changed"); err != nil {
		return err
	}
	if _, err = tx.Exec(`CREATE TABLE IF NOT EXISTS run_control(
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 run BLOB, observation BLOB, result BLOB);
CREATE VIEW IF NOT EXISTS run_headers AS
 SELECT r.id, COALESCE(c.run,r.run) AS run,
 COALESCE(c.observation,r.observation) AS observation,
 COALESCE(c.result,r.result_override) AS result_override
 FROM runs r LEFT JOIN run_control c ON c.run_id=r.id;
CREATE TRIGGER IF NOT EXISTS run_control_state_changed AFTER UPDATE OF state ON runs BEGIN
 UPDATE runs SET result_override=NULL WHERE id=NEW.id;
 UPDATE run_control SET result=NULL WHERE run_id=NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS run_control_run_changed AFTER UPDATE OF run ON runs BEGIN
 UPDATE run_control SET run=NULL WHERE run_id=NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS run_control_observation_changed AFTER UPDATE OF observation ON runs BEGIN
 UPDATE run_control SET observation=NULL WHERE run_id=NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS run_control_result_changed AFTER UPDATE OF result_override ON runs BEGIN
 UPDATE run_control SET result=NULL WHERE run_id=NEW.id;
END;`); err != nil {
		return err
	}
	// Also reject incomplete version-3 headers rather than silently reading base
	// fields after a damaged or interrupted external schema replacement.
	rows, err = tx.Query("SELECT id,run,observation,result_override FROM run_headers LIMIT 0")
	if err != nil {
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	if version != 3 {
		if _, err = tx.Exec("PRAGMA user_version=3"); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Called only by updatePause after comparing the complete typed state under
// the same Store fence. This is not a general save of a possibly stale snapshot.
func (s *Store) sqliteSaveControl(snap Snapshot) error {
	if s.beforePersist != nil {
		s.beforePersist(snap.Run.ID)
	}
	db, err := s.sqliteDB()
	if err != nil {
		return err
	}
	run, err := json.Marshal(snap.Run)
	if err != nil {
		return err
	}
	result, err := json.Marshal(snap.Result)
	if err != nil {
		return err
	}
	observation, err := json.Marshal(Observation{Run: snap.Run, Plan: snap.Plan, Workers: snap.Workers, WorkerMessages: snap.WorkerMessages, Mailbox: snap.Mailbox})
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	updated, err := tx.Exec("INSERT INTO run_control(run_id,run,observation,result) VALUES(?,?,?,?) ON CONFLICT(run_id) DO UPDATE SET run=excluded.run,observation=excluded.observation,result=excluded.result", snap.Run.ID, run, observation, result)
	if err != nil {
		return err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("control run is missing: %s", snap.Run.ID)
	}
	if err = sqliteWriteEvents(tx, snap.Run, snap.Events); err != nil {
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
