package agent

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync/atomic"
	"testing"
)

func TestSQLiteDisplayChunkCancellationOwnsReturnedRows(t *testing.T) {
	for _, test := range []struct {
		name   string
		before bool
		err    error
	}{
		{name: "cancel at rows handoff"},
		{name: "transaction error preserves cancellation", err: sql.ErrTxDone},
		{name: "already cancelled", before: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.WithValue(context.Background(), displayCancelContextKey{}, "preserved"))
			defer cancel()
			probe := &displayCancelDriver{cancel: cancel, queryErr: test.err}
			db := sql.OpenDB(probe)
			defer db.Close()
			tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			r := newSQLiteStateReader(ctx, tx, "run")
			defer r.Close()
			if test.before {
				cancel()
			}
			if n, err := r.Read(make([]byte, 1)); n != 0 || !errors.Is(err, context.Canceled) {
				t.Fatal("cancelled query delivered bytes or lost its context error", n, err)
			}
			if !probe.transactionCancelable {
				t.Fatal("read transaction lost its original cancellation context")
			}
			if test.before {
				if probe.queryCalls != 0 {
					t.Fatal("already cancelled reader entered the driver")
				}
				return
			}
			if probe.queryCalls != 1 || probe.queryCancelable || probe.queryValue != "preserved" {
				t.Fatal("chunk handoff context is not isolated or lost its values", probe.queryCalls, probe.queryCancelable, probe.queryValue)
			}
			wantClosed := int32(1)
			if test.err != nil {
				wantClosed = 0
			}
			if probe.rowsClosed.Load() != wantClosed || probe.rowsRead.Load() != 0 {
				t.Fatal("cancelled handoff leaked or read returned rows", probe.rowsClosed.Load(), probe.rowsRead.Load())
			}
			if r.rows != nil || r.reader != nil || r.readAtData != nil {
				t.Fatal("cancelled handoff retained a chunk")
			}
		})
	}
}

type displayCancelContextKey struct{}

// QueryContext cancels its caller just before transferring an owned result.
// It models the driver's handoff window without timing sleeps or a large BLOB.
type displayCancelDriver struct {
	cancel                context.CancelFunc
	queryErr              error
	transactionCancelable bool
	queryCancelable       bool
	queryCalls            int
	queryValue            any
	rowsClosed            atomic.Int32
	rowsRead              atomic.Int32
}

func (d *displayCancelDriver) Connect(context.Context) (driver.Conn, error) { return d, nil }
func (d *displayCancelDriver) Driver() driver.Driver                        { return d }
func (d *displayCancelDriver) Open(string) (driver.Conn, error)             { return d, nil }
func (d *displayCancelDriver) Close() error                                 { return nil }
func (d *displayCancelDriver) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (d *displayCancelDriver) Begin() (driver.Tx, error) { return displayCancelTx{}, nil }
func (d *displayCancelDriver) BeginTx(ctx context.Context, _ driver.TxOptions) (driver.Tx, error) {
	d.transactionCancelable = ctx.Done() != nil
	return displayCancelTx{}, nil
}
func (d *displayCancelDriver) QueryContext(ctx context.Context, _ string, _ []driver.NamedValue) (driver.Rows, error) {
	d.queryCalls++
	d.queryCancelable = ctx.Done() != nil
	d.queryValue = ctx.Value(displayCancelContextKey{})
	d.cancel()
	if d.queryErr != nil {
		return nil, d.queryErr
	}
	return &displayCancelRows{probe: d}, nil
}

type displayCancelTx struct{}

func (displayCancelTx) Commit() error   { return nil }
func (displayCancelTx) Rollback() error { return nil }

type displayCancelRows struct{ probe *displayCancelDriver }

func (*displayCancelRows) Columns() []string { return []string{"state"} }
func (r *displayCancelRows) Close() error {
	r.probe.rowsClosed.Add(1)
	return nil
}
func (r *displayCancelRows) Next([]driver.Value) error {
	r.probe.rowsRead.Add(1)
	return io.EOF
}
