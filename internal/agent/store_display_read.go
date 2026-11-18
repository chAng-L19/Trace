package agent

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
)

const sqliteDisplayChunkBytes = 32 << 20

// Keep at most one row-owned chunk in Go while scanning the authoritative BLOB.
// All chunks and selected record re-reads share the caller's read transaction.
// The existing Store gate bounds concurrent SQLite VM work as well.
type sqliteStateReader struct {
	ctx          context.Context
	tx           *sql.Tx
	id           string
	rows         *sql.Rows
	reader       *bytes.Reader
	offset       int64
	eof, closed  bool
	readAtData   []byte
	readAtOffset int64
	readAtReady  bool
}

func newSQLiteStateReader(ctx context.Context, tx *sql.Tx, id string, _ int64) *sqliteStateReader {
	return &sqliteStateReader{ctx: ctx, tx: tx, id: id}
}

func (r *sqliteStateReader) Read(p []byte) (int, error) {
	if err := r.check(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if r.reader == nil {
		if r.eof {
			return 0, io.EOF
		}
		rows, raw, err := r.chunk(r.offset, sqliteDisplayChunkBytes)
		if err != nil {
			return 0, err
		}
		r.rows, r.reader = rows, bytes.NewReader(raw)
		r.eof = len(raw) < sqliteDisplayChunkBytes
		if len(raw) == 0 {
			r.closeChunk()
			return 0, io.EOF
		}
	}
	n, err := r.reader.Read(p)
	r.offset += int64(n)
	if r.reader.Len() == 0 {
		if closeErr := r.closeChunk(); err == nil {
			err = closeErr
		}
	}
	return n, err
}

func (r *sqliteStateReader) ReadAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, errors.New("negative SQLite state read offset")
	}
	n := 0
	for n < len(p) {
		if err := r.check(); err != nil {
			return n, err
		}
		position := offset + int64(n)
		aligned := position / sqliteDisplayChunkBytes * sqliteDisplayChunkBytes
		if !r.readAtReady || r.readAtOffset != aligned {
			rows, raw, err := r.chunk(aligned, sqliteDisplayChunkBytes)
			if err != nil {
				return n, err
			}
			// A single owned aligned chunk avoids re-reading the whole SQLite
			// BLOB for every small selected response. Reuse its allocation, and
			// never retain the driver's RawBytes after closing this row.
			if cap(r.readAtData) < len(raw) {
				r.readAtData = make([]byte, len(raw))
			} else {
				r.readAtData = r.readAtData[:len(raw)]
			}
			copy(r.readAtData, raw)
			if err = rows.Close(); err != nil {
				r.readAtData, r.readAtReady = nil, false
				return n, err
			}
			r.readAtOffset, r.readAtReady = aligned, true
		}
		start := position - aligned
		if start >= int64(len(r.readAtData)) {
			return n, io.EOF
		}
		n += copy(p[n:], r.readAtData[start:])
	}
	return n, nil
}

func (r *sqliteStateReader) check() error {
	if r.closed {
		return io.ErrClosedPipe
	}
	return r.ctx.Err()
}

func (r *sqliteStateReader) chunk(offset int64, length int) (*sql.Rows, sql.RawBytes, error) {
	if err := r.check(); err != nil {
		return nil, nil, err
	}
	// CAST makes offsets bytes even for externally written TEXT state values.
	// The pinned SQLite driver can drop an owned statement if query cancellation
	// races with handing rows back. Let this chunk finish that handoff; the
	// caller still observes its original context before and after each chunk.
	rows, err := r.tx.QueryContext(context.WithoutCancel(r.ctx), "SELECT substr(CAST(state AS BLOB),?,?) FROM runs WHERE id=?", offset+1, length, r.id)
	if canceled := r.check(); canceled != nil {
		if rows != nil {
			rows.Close()
		}
		return nil, nil, canceled
	}
	if err != nil {
		return nil, nil, err
	}
	if !rows.Next() {
		err := rows.Err()
		rows.Close()
		if err != nil {
			return nil, nil, err
		}
		return nil, nil, sql.ErrNoRows
	}
	var raw sql.RawBytes
	if err := rows.Scan(&raw); err != nil {
		rows.Close()
		return nil, nil, err
	}
	return rows, raw, nil
}

// data_version is comparable only on one physical connection. Replacing that
// connection invalidates every version token, retaining only disposable hashes.
func (s *Store) displayTransaction(ctx context.Context) (*sql.Tx, error) {
	for attempt := 0; attempt < 2; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if s.sqliteDisplayConn == nil {
			db, err := s.sqliteDB()
			if err != nil {
				return nil, err
			}
			s.sqliteDisplayConn, err = db.Conn(ctx)
			if err != nil {
				return nil, err
			}
			s.eventViewMu.Lock()
			for _, entry := range s.activityCache.entries {
				entry.key.version = 0
			}
			s.eventViewMu.Unlock()
		}
		// Keep cleanup synchronous on this reserved connection: automatic context
		// rollback can mark a Tx done before releasing its driver's transaction.
		// The caller checks the original context and closes its source, then rolls
		// back before releasing the Store gate; connection waits stay cancellable.
		tx, err := s.sqliteDisplayConn.BeginTx(context.WithoutCancel(ctx), &sql.TxOptions{ReadOnly: true})
		if canceled := ctx.Err(); canceled != nil {
			if tx != nil {
				tx.Rollback()
			}
			return nil, canceled
		}
		if err == nil {
			return tx, nil
		}
		if !errors.Is(err, sql.ErrConnDone) && !errors.Is(err, driver.ErrBadConn) {
			return nil, err
		}
		s.sqliteDisplayConn.Close()
		s.sqliteDisplayConn = nil
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
	return nil, sql.ErrConnDone
}

func (r *sqliteStateReader) Close() error {
	r.closed = true
	r.readAtData, r.readAtReady = nil, false
	return r.closeChunk()
}

func (r *sqliteStateReader) closeChunk() error {
	r.reader = nil
	if r.rows == nil {
		return nil
	}
	err := r.rows.Close()
	r.rows = nil
	return err
}

type displaySource struct {
	ctx context.Context
	io.Reader
	io.ReaderAt
}

func (r displaySource) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func (r displaySource) ReadAt(p []byte, offset int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.ReaderAt.ReadAt(p, offset)
}
