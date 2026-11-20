package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const displaySchemaSQL = `
CREATE TABLE IF NOT EXISTS run_display_index(
 run_id TEXT PRIMARY KEY REFERENCES runs(id) ON DELETE CASCADE,
 version INTEGER NOT NULL, state_hash BLOB NOT NULL, data BLOB NOT NULL, checksum BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS run_display_records(
 run_id TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
 kind TEXT NOT NULL, ordinal INTEGER NOT NULL CHECK(ordinal>=0),
 body BLOB NOT NULL, checksum BLOB NOT NULL,
 PRIMARY KEY(run_id,kind,ordinal)) WITHOUT ROWID;
CREATE TRIGGER IF NOT EXISTS run_display_state_changed AFTER UPDATE OF state ON runs BEGIN
 DELETE FROM run_display_index WHERE run_id=NEW.id;
 DELETE FROM run_display_records WHERE run_id=NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS run_display_run_inserted AFTER INSERT ON runs BEGIN
 DELETE FROM run_display_index WHERE run_id=NEW.id;
 DELETE FROM run_display_records WHERE run_id=NEW.id;
END;
CREATE TRIGGER IF NOT EXISTS run_display_run_deleted AFTER DELETE ON runs BEGIN
 DELETE FROM run_display_index WHERE run_id=OLD.id;
 DELETE FROM run_display_records WHERE run_id=OLD.id;
END;
CREATE TRIGGER IF NOT EXISTS run_display_row_inserted AFTER INSERT ON run_display_records BEGIN
 DELETE FROM run_display_index WHERE run_id=NEW.run_id;
END;
CREATE TRIGGER IF NOT EXISTS run_display_row_updated AFTER UPDATE ON run_display_records BEGIN
 DELETE FROM run_display_index WHERE run_id=OLD.run_id OR run_id=NEW.run_id;
END;
CREATE TRIGGER IF NOT EXISTS run_display_row_deleted AFTER DELETE ON run_display_records BEGIN
 DELETE FROM run_display_index WHERE run_id=OLD.run_id;
END;
CREATE TRIGGER IF NOT EXISTS run_display_index_updated AFTER UPDATE ON run_display_index BEGIN
 DELETE FROM run_display_index WHERE run_id=OLD.run_id OR run_id=NEW.run_id;
END;`

func sqliteWriteDisplayIndex(ctx context.Context, tx *sql.Tx, id string, source interface {
	io.Reader
	io.ReaderAt
}) error {
	if _, err := tx.Exec("DELETE FROM run_display_index WHERE run_id=?", id); err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM run_display_records WHERE run_id=?", id); err != nil {
		return err
	}
	stmt, err := tx.Prepare("INSERT INTO run_display_records(run_id,kind,ordinal,body,checksum) VALUES(?,?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()
	hash := sha256.New()
	index, err := buildDisplayRecordIndex(displaySource{ctx, io.TeeReader(source, hash), source}, func(kind string, ordinal int, raw []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		sum := sha256.Sum256(raw)
		_, err := stmt.Exec(id, kind, ordinal, raw, sum[:])
		return err
	}, func(kind string) error {
		_, err := tx.Exec("DELETE FROM run_display_records WHERE run_id=? AND kind=?", id, kind)
		return err
	})
	if err != nil {
		return err
	}
	copy(index.State[:], hash.Sum(nil))
	raw, err := json.Marshal(index)
	if err != nil {
		return err
	}
	if len(raw) > activityProjectionMaxBytes {
		return errActivityTooLarge
	}
	sum := sha256.Sum256(raw)
	_, err = tx.Exec("INSERT INTO run_display_index(run_id,version,state_hash,data,checksum) VALUES(?,?,?,?,?)", id, displayIndexVersion, index.State[:], raw, sum[:])
	return err
}

// Only old/imported or externally changed states enter this path. Build once
// from the existing bounded reader, retaining no complete driver-owned state
// beside the decoder. SQLite VM memory and first-build cost remain measurable.
func (s *Store) rebuildSQLiteDisplayIndex(ctx context.Context, id string) error {
	unlock, err := s.acquireStoreLockContext(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	db, err := s.sqliteDB()
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(context.WithoutCancel(ctx), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var version int
	err = tx.QueryRow("SELECT version FROM run_display_index WHERE run_id=?", id).Scan(&version)
	if err == nil && version == displayIndexVersion {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err = sqliteBuildLegacyDisplayIndex(ctx, tx, id); err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	return tx.Commit()
}

func sqliteBuildLegacyDisplayIndex(ctx context.Context, tx *sql.Tx, id string) error {
	source := newSQLiteStateReader(ctx, tx, id)
	defer source.Close()
	if err := sqliteWriteDisplayIndex(ctx, tx, id, source); err != nil {
		return err
	}
	if err := source.Close(); err != nil {
		return err
	}
	return ctx.Err()
}

func sqliteStageDisplayIndex(tx *sql.Tx, id string, state []byte) error {
	if _, err := tx.Exec("SAVEPOINT display_index"); err != nil {
		return err
	}
	err := sqliteWriteDisplayIndex(context.Background(), tx, id, bytes.NewReader(state))
	if errors.Is(err, errActivityTooLarge) {
		// A display budget never narrows the original Store's write contract.
		// The authority still commits; this generation retains no partial index.
		if _, rollbackErr := tx.Exec("ROLLBACK TO display_index"); rollbackErr != nil {
			return rollbackErr
		}
	} else if err != nil {
		return err
	}
	_, err = tx.Exec("RELEASE display_index")
	return err
}

func (s *Store) indexedSQLiteTransaction(ctx context.Context, id string) (*sql.Tx, []byte, activityCacheKey, error) {
	for attempt := 0; attempt < 3; attempt++ {
		tx, err := s.displayTransaction(ctx)
		if err != nil {
			return nil, nil, activityCacheKey{}, err
		}
		var run, hash, result []byte
		var version sql.NullInt64
		var resultSize sql.NullInt64
		err = tx.QueryRowContext(context.WithoutCancel(ctx), "SELECT r.run,i.version,i.state_hash,length(CAST(r.result_override AS BLOB)),CASE WHEN length(CAST(r.result_override AS BLOB))<=? THEN r.result_override END FROM run_headers r LEFT JOIN run_display_index i ON i.run_id=r.id WHERE r.id=?", activityProjectionMaxBytes, id).Scan(&run, &version, &hash, &resultSize, &result)
		if canceled := ctx.Err(); canceled != nil {
			err = canceled
		}
		if errors.Is(err, sql.ErrNoRows) {
			err = os.ErrNotExist
		}
		if err != nil {
			tx.Rollback()
			return nil, nil, activityCacheKey{}, err
		}
		if resultSize.Valid && resultSize.Int64 > activityProjectionMaxBytes {
			tx.Rollback()
			return nil, nil, activityCacheKey{}, errActivityTooLarge
		}
		if version.Valid && version.Int64 == displayIndexVersion && len(hash) == sha256.Size {
			key := activityCacheKey{}
			copy(key.state[:], hash)
			if result != nil {
				h := sha256.New()
				h.Write(hash)
				h.Write(result)
				copy(key.state[:], h.Sum(nil))
			}
			if err = tx.QueryRow("PRAGMA data_version").Scan(&key.version); err != nil {
				tx.Rollback()
				return nil, nil, key, err
			}
			return tx, run, key, nil
		}
		tx.Rollback()
		if err = s.rebuildSQLiteDisplayIndex(ctx, id); err != nil {
			return nil, nil, activityCacheKey{}, err
		}
	}
	return nil, nil, activityCacheKey{}, fmt.Errorf("display generation changed during index construction")
}

func readSQLiteDisplayIndex(ctx context.Context, tx *sql.Tx, id string) (*displayRecordIndex, *activityRecords, error) {
	read := func(query string, args ...any) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var raw, checksum []byte
		if err := tx.QueryRowContext(context.WithoutCancel(ctx), query, args...).Scan(&raw, &checksum); err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if raw == nil {
			return nil, errActivityTooLarge
		}
		sum := sha256.Sum256(raw)
		if !bytes.Equal(sum[:], checksum) {
			return nil, fmt.Errorf("display index checksum mismatch")
		}
		return raw, nil
	}
	raw, err := read("SELECT CASE WHEN length(data)<=? THEN data ELSE NULL END,checksum FROM run_display_index WHERE run_id=?", activityProjectionMaxBytes, id)
	if err != nil {
		return nil, nil, err
	}
	var index displayRecordIndex
	if err = json.Unmarshal(raw, &index); err != nil {
		return nil, nil, err
	}
	if index.Version != displayIndexVersion || index.Fields == nil {
		return nil, nil, fmt.Errorf("invalid display index version/fields")
	}
	var result []byte
	var resultSize sql.NullInt64
	if err = tx.QueryRowContext(context.WithoutCancel(ctx), "SELECT length(CAST(result_override AS BLOB)),CASE WHEN length(CAST(result_override AS BLOB))<=? THEN result_override END FROM run_headers WHERE id=?", activityProjectionMaxBytes, id).Scan(&resultSize, &result); err != nil {
		return nil, nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, err
	}
	if resultSize.Valid && resultSize.Int64 > activityProjectionMaxBytes {
		return nil, nil, errActivityTooLarge
	}
	if result != nil {
		index.Fields["result"] = result
	}
	records := index.records(func(kind string, ordinal int) ([]byte, error) {
		return read("SELECT CASE WHEN length(body)<=? THEN body ELSE NULL END,checksum FROM run_display_records WHERE run_id=? AND kind=? AND ordinal=?", activityProjectionMaxBytes, id, kind, ordinal)
	})
	return &index, records, nil
}
