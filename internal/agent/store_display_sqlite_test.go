package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

func TestSQLiteDisplayChunkReaderByteOffsetsAndEOF(t *testing.T) {
	s := sqliteTestStore(t)
	created, err := s.Create("chunk reader", "", "")
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.sqliteDB()
	if err != nil {
		t.Fatal(err)
	}
	data := bytes.Repeat([]byte{'x'}, sqliteDisplayChunkBytes+19)
	copy(data, []byte("字节"))
	copy(data[sqliteDisplayChunkBytes-3:], []byte("边界𠮷after"))
	for _, kind := range []string{"blob", "text"} {
		t.Run(kind, func(t *testing.T) {
			var value any = data
			if kind == "text" {
				value = string(data)
			}
			if _, err := db.Exec("UPDATE runs SET state=? WHERE id=?", value, created.Run.ID); err != nil {
				t.Fatal(err)
			}
			tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newSQLiteStateReader(ctx, tx, created.Run.ID, 0)
			defer r.Close()
			hash := sha256.New()
			n, err := io.Copy(hash, r)
			want := sha256.Sum256(data)
			if err != nil || n != int64(len(data)) || !bytes.Equal(hash.Sum(nil), want[:]) {
				t.Fatal("chunk stream changed original bytes", n, err)
			}
			if r.rows != nil || r.reader != nil {
				t.Fatal("EOF retained a completed SQL chunk")
			}
			if r.readAtReady || r.readAtData != nil {
				t.Fatal("sequential scan retained a second chunk cache")
			}
			first := make([]byte, 17)
			if _, err := r.ReadAt(first, 1); err != nil {
				t.Fatal(err)
			}
			owned := &r.readAtData[0]
			if r.readAtOffset != 0 || len(r.readAtData) != sqliteDisplayChunkBytes || r.rows != nil {
				t.Fatal("selected chunk was not owned and bounded")
			}
			if _, err := r.ReadAt(first, 25); err != nil || &r.readAtData[0] != owned || !bytes.Equal(first, data[25:42]) {
				t.Fatal("same aligned chunk was not reused", err)
			}
			for _, offset := range []int64{0, sqliteDisplayChunkBytes - 5, int64(len(data) - 3)} {
				buf := make([]byte, 17)
				n, err := r.ReadAt(buf, offset)
				end := min(int64(len(data)), offset+int64(len(buf)))
				if !bytes.Equal(buf[:n], data[offset:end]) || n != int(end-offset) || (n < len(buf) && !errors.Is(err, io.EOF)) || (n == len(buf) && err != nil) {
					t.Fatal("selected bytes changed at chunk boundary", offset, n, err)
				}
			}
			if cap(r.readAtData) > sqliteDisplayChunkBytes || r.readAtOffset != sqliteDisplayChunkBytes || r.rows != nil {
				t.Fatal("cross-boundary read retained multiple chunks")
			}
			cancel()
			if _, err := r.ReadAt(make([]byte, 1), sqliteDisplayChunkBytes); !errors.Is(err, context.Canceled) {
				t.Fatal("cache hit ignored cancellation", err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if r.readAtData != nil || r.readAtReady {
				t.Fatal("Close retained selected-record chunk")
			}
			if _, err := r.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal("closed reader resumed", err)
			}
		})
	}
}

func TestSQLiteDisplayTransactionGenerationAndConcurrentWrite(t *testing.T) {
	s := sqliteTestStore(t)
	created, err := s.Create("old", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(created.Run.ID, func(v *Snapshot) error { v.Messages = []Message{{Role: "user", Content: "old"}}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActivityWorkspace(created.Run.ID); err != nil {
		t.Fatal(err)
	}
	tx, err := s.displayTransaction(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var size, version int64
	var run []byte
	if err = tx.QueryRow("SELECT length(state),run FROM runs WHERE id=?", created.Run.ID).Scan(&size, &run); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow("PRAGMA data_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	other, err := NewStore(s.root)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	_, err = other.Update(created.Run.ID, func(v *Snapshot) error { v.Run.Goal = "new"; v.Messages[0].Content = "new"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	var stillVersion int64
	if err = tx.QueryRow("PRAGMA data_version").Scan(&stillVersion); err != nil || stillVersion != version {
		t.Fatal("generation escaped the established read snapshot", err)
	}
	source := newSQLiteStateReader(context.Background(), tx, created.Run.ID, size)
	data, err := io.ReadAll(source)
	if err != nil {
		t.Fatal(err)
	}
	if err = source.Close(); err != nil {
		t.Fatal(err)
	}
	var old Snapshot
	if err = json.Unmarshal(data, &old); err != nil || old.Messages[0].Content != "old" {
		t.Fatal("state/run cut mixed transactions", err)
	}
	var oldRun Run
	if err = json.Unmarshal(run, &oldRun); err != nil || oldRun.Goal != "old" {
		t.Fatal("Run escaped read snapshot", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	p, err := s.ActivityWorkspace(created.Run.ID)
	if err != nil || p.Snapshot.Run.Goal != "new" || p.Snapshot.Messages[0].Content != "new" {
		t.Fatal("new committed generation stayed stale", err)
	}
}

func TestSQLiteDisplayCancelledAndReplacedConnectionRecovers(t *testing.T) {
	s := sqliteTestStore(t)
	created, err := s.Create("cancel", "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Update(created.Run.ID, func(v *Snapshot) error { v.Messages = []Message{{Role: "user", Content: "before"}}; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ActivityWorkspace(created.Run.ID); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 16; attempt++ {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		tx, err := s.displayTransaction(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		var size int64
		if err = tx.QueryRowContext(ctx, "SELECT length(state) FROM runs WHERE id=?", created.Run.ID).Scan(&size); err != nil {
			t.Fatal(err)
		}
		source := newSQLiteStateReader(ctx, tx, created.Run.ID, size)
		defer source.Close()
		if _, err = source.Read(make([]byte, 1)); err != nil {
			t.Fatal(err)
		}
		cancel()
		if _, err = source.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
			t.Fatal("cancelled reader kept scanning", err)
		}
		if err = source.Close(); err != nil {
			t.Fatal(err)
		}
		if err = tx.Rollback(); err != nil {
			t.Fatal("cancelled display transaction lost synchronous cleanup ownership", attempt, err)
		}
		if _, err = s.ActivityWorkspace(created.Run.ID); err != nil {
			t.Fatal("cancelled transaction poisoned display connection", attempt, err)
		}
	}
	// Also exercise drivers that discard the physical connection on cancel.
	if err = s.sqliteDisplayConn.Close(); err != nil && !errors.Is(err, sql.ErrConnDone) {
		t.Fatal(err)
	}
	_, err = s.Update(created.Run.ID, func(v *Snapshot) error { v.Messages[0].Content = "after"; return nil })
	if err != nil {
		t.Fatal(err)
	}
	p, err := s.ActivityWorkspace(created.Run.ID)
	if err != nil || p.Snapshot.Messages[0].Content != "after" {
		t.Fatal("new connection reused an unrelated data_version token", err)
	}
}
