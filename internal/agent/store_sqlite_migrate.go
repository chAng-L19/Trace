package agent

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// MigrateStoreToSQLite is explicit and offline: callers must stop execution
// owners first. It takes every Run lock as well as the cross-process Store lock.
// JSON is never dual-written; once the database is published it is authoritative.
// The old snapshots remain an immutable migration-time backup, not a rollback
// of subsequent commits. ExportSQLiteSnapshots is the current-state exit path.
func MigrateStoreToSQLite(root string) error {
	return migrateStoreToSQLite(root, nil)
}

func migrateStoreToSQLite(root string, beforePublish func() error) error {
	s, err := NewStore(root)
	if err != nil {
		return err
	}
	defer s.Close()
	if s.sqlite {
		return fmt.Errorf("store is already SQLite")
	}
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return err
	}
	defer unlock()
	ids, err := s.snapshotIDsLocked()
	if err != nil {
		return err
	}
	var snapshots []Snapshot
	for _, id := range ids {
		snap, e := s.loadRawLocked(id)
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		previous := 0
		for _, event := range snap.Events {
			if event.Sequence <= previous || event.Sequence > snap.Run.EventSeq {
				return fmt.Errorf("SQLite migration rejected run %s: events must have strictly increasing positive sequences within cursor %d (previous=%d, sequence=%d); JSON unchanged", id, snap.Run.EventSeq, previous, event.Sequence)
			}
			previous = event.Sequence
		}
		release, e := s.AcquireRunLock(id)
		if e != nil {
			return e
		}
		defer release()
		snapshots = append(snapshots, snap)
	}
	f, err := os.CreateTemp(root, ".sqlite-migration-*.tmp")
	if err != nil {
		return err
	}
	path := f.Name()
	if err = f.Close(); err != nil {
		return err
	}
	preserveCandidate := false
	defer func() {
		if !preserveCandidate {
			os.Remove(path)
		}
	}()
	db, err := openSQLite(path, true)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err = db.Exec(`
 PRAGMA journal_mode=DELETE;
 CREATE TABLE runs(id TEXT PRIMARY KEY,run BLOB NOT NULL,state BLOB NOT NULL,observation BLOB NOT NULL,result_override BLOB);
 CREATE TABLE events(run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,sequence INTEGER NOT NULL CHECK(sequence>0),body BLOB NOT NULL,PRIMARY KEY(run_id,sequence)) WITHOUT ROWID;
 CREATE TABLE purge_commits(stage TEXT PRIMARY KEY);
 PRAGMA user_version=2;`); err != nil {
		return err
	}
	if _, err = db.Exec(displaySchemaSQL); err != nil {
		return err
	}
	if err = ensureSQLiteControlSchema(db, 2); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, snap := range snapshots {
		if err = sqliteWriteSnapshot(tx, snap); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	var mode string
	if err = db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("SQLite WAL unavailable: %s", mode)
	}
	if err = db.Close(); err != nil {
		return err
	}
	f, err = os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	backup := filepath.Join(root, "sqlite-migration-json-backup")
	if err = os.MkdirAll(backup, 0700); err != nil {
		return err
	}
	marker, err := os.CreateTemp(root, ".sqlite-marker-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(marker.Name())
	_, err = marker.WriteString(filepath.Base(path) + "\n")
	if err == nil {
		err = marker.Sync()
	}
	ce := marker.Close()
	if err != nil {
		return err
	}
	if ce != nil {
		return ce
	}
	if err = os.Rename(marker.Name(), filepath.Join(root, sqliteStoreMarker)); err != nil {
		return err
	}
	preserveCandidate = true
	if err = syncStoreDirectory(root); err != nil {
		return err
	}
	if beforePublish != nil {
		if err = beforePublish(); err != nil {
			return err
		}
	}
	// Publication is the only authority switch. On interruption before this
	// rename JSON is untouched; afterwards JSON is ignored even if archival fails.
	if err = os.Rename(path, filepath.Join(root, sqliteStoreName)); err != nil {
		return err
	}
	if err = syncStoreDirectory(root); err != nil {
		return err
	}
	for _, snap := range snapshots {
		target := filepath.Join(backup, snap.Run.ID+".json")
		if err = os.Rename(filepath.Join(root, snap.Run.ID+".json"), target); err != nil {
			return err
		}
		if err = os.Chmod(target, 0400); err != nil {
			return err
		}
		if err = os.Remove(filepath.Join(root, snap.Run.ID+".display")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err = syncStoreDirectory(backup); err != nil {
		return err
	}
	return syncStoreDirectory(root)
}

// A durable marker names the fully synced candidate. Recovery completes the
// authority switch, never silently rolls back to old JSON. Once published, a
// missing database plus missing candidate is corruption and must fail closed.
func recoverSQLiteMigration(root string) error {
	raw, err := os.ReadFile(filepath.Join(root, sqliteStoreMarker))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	name := strings.TrimSpace(string(raw))
	if name == "sqlite-v1" {
		if _, err = os.Stat(filepath.Join(root, sqliteStoreName)); err != nil {
			return fmt.Errorf("legacy SQLite authority missing database: %w", err)
		}
		return finishSQLiteMigrationArchive(root)
	}
	if filepath.Base(name) != name || !strings.HasPrefix(name, ".sqlite-migration-") || !strings.HasSuffix(name, ".tmp") {
		return fmt.Errorf("invalid SQLite authority marker")
	}
	if _, err = os.Stat(filepath.Join(root, sqliteStoreName)); err == nil {
		return finishSQLiteMigrationArchive(root)
	} else if !os.IsNotExist(err) {
		return err
	}
	candidate := filepath.Join(root, name)
	if _, err = os.Stat(candidate); err != nil {
		return fmt.Errorf("SQLite authority missing database/candidate: %w", err)
	}
	if err = os.Rename(candidate, filepath.Join(root, sqliteStoreName)); err != nil {
		return err
	}
	if err = syncStoreDirectory(root); err != nil {
		return err
	}
	return finishSQLiteMigrationArchive(root)
}

func finishSQLiteMigrationArchive(root string) error {
	backup := filepath.Join(root, "sqlite-migration-json-backup")
	if err := os.MkdirAll(backup, 0700); err != nil {
		return err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(root, entry.Name())
		raw, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		var header struct{ Run Run }
		if json.Unmarshal(raw, &header) != nil || header.Run.ID == "" || header.Run.ID != strings.TrimSuffix(entry.Name(), ".json") {
			continue
		}
		target := filepath.Join(backup, entry.Name())
		if _, e = os.Stat(target); e == nil {
			return fmt.Errorf("migration backup already exists: %s", entry.Name())
		} else if !os.IsNotExist(e) {
			return e
		}
		if e = os.Rename(path, target); e != nil {
			return e
		}
		if e = os.Chmod(target, 0400); e != nil {
			return e
		}
	}
	// Derived JSON spans do not migrate or become an alternate authority.
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".display" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".display")
		if _, e := os.Stat(filepath.Join(backup, id+".json")); e != nil {
			continue
		}
		if e := os.Remove(filepath.Join(root, entry.Name())); e != nil && !os.IsNotExist(e) {
			return e
		}
	}
	if err = syncStoreDirectory(backup); err != nil {
		return err
	}
	return syncStoreDirectory(root)
}

func syncStoreDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ExportSQLiteSnapshots publishes detached JSON into a new directory, never
// switches the live backend. CAS/inputs must accompany these snapshots when
// an operator constructs a rollback root; missing CAS still fails the Judge.
func (s *Store) ExportSQLiteSnapshots(destination string) error {
	if !s.sqlite {
		return fmt.Errorf("store is not SQLite")
	}
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return err
	}
	defer unlock()
	ids, err := s.sqliteIDs()
	if err != nil {
		return err
	}
	for _, id := range ids {
		release, e := s.AcquireRunLock(id)
		if e != nil {
			return e
		}
		defer release()
	}
	if err = os.Mkdir(destination, 0700); err != nil {
		return err
	}
	for _, id := range ids {
		snap, e := s.sqliteLoad(id)
		if e != nil {
			return e
		}
		raw, e := json.Marshal(snap)
		if e != nil {
			return e
		}
		f, e := os.OpenFile(filepath.Join(destination, id+".json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = f.Write(raw)
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
	}
	return syncStoreDirectory(destination)
}

func (s *Store) sqliteCommitPurge(stage string, ids []string) error {
	db, err := s.sqliteDB()
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err = tx.Exec("DELETE FROM runs WHERE id=?", id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT INTO purge_commits(stage) VALUES(?)", filepath.Base(stage)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) sqlitePurgeCommitted(stage string) (bool, error) {
	db, err := s.sqliteDB()
	if err != nil {
		return false, err
	}
	var name string
	err = db.QueryRow("SELECT stage FROM purge_commits WHERE stage=?", filepath.Base(stage)).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}
