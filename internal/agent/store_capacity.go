package agent

// References: pi 22fc026b agent.ts forwards the active cancellation signal;
// Muteki a73535a1 platform/store.py keeps read snapshots in the original Store.
// Trace retains its cross-process Store fence and original capacity predicates.
import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

func (s *Store) acquireStoreLockContext(ctx context.Context) (func(), error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		unlock, err := acquireFileLock(filepath.Join(s.root, ".store.lock"), false)
		if !errors.Is(err, ErrRunLockBusy) {
			if err != nil {
				return nil, err
			}
			if err = ctx.Err(); err != nil {
				unlock()
				return nil, err
			}
			unlock, err = s.finishStoreLock(unlock, nil)
			if err == nil && ctx.Err() != nil {
				unlock()
				return nil, ctx.Err()
			}
			return unlock, err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// All nodes come from one Store-locked generation. The map only avoids reading
// the requested child's ancestry twice; it is discarded after this request.
func (s *Store) capacityTreeLocked(ctx context.Context, id string) (*Snapshot, []*Snapshot, error) {
	var tx *sql.Tx
	if s.sqlite {
		db, err := s.sqliteDB()
		if err != nil {
			return nil, nil, err
		}
		// Keep rollback ownership synchronous, including optional derived-index
		// backfill. No authoritative state is changed by this transaction.
		tx, err = db.BeginTx(context.WithoutCancel(ctx), nil)
		if err != nil {
			return nil, nil, err
		}
		defer tx.Rollback()
	}
	loaded := map[string]*Snapshot{}
	budget := activityProjectionMaxBytes
	load := func(id string) (*Snapshot, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if snap := loaded[id]; snap != nil {
			return snap, nil
		}
		budget -= int(reflect.TypeOf(Snapshot{}).Size())
		if budget < 0 {
			return nil, errActivityTooLarge
		}
		snap, err := s.loadCapacityLocked(ctx, tx, id, &budget)
		if err == nil {
			loaded[id] = snap
		}
		return snap, err
	}
	root, err := load(id)
	if err != nil {
		return nil, nil, err
	}
	ancestors := map[string]bool{root.Run.ID: true}
	for root.Run.ParentTaskID != "" && root.Run.ParentRunID != "" {
		if ancestors[root.Run.ParentRunID] {
			return nil, nil, fmt.Errorf("worker budget ancestry contains a cycle")
		}
		ancestors[root.Run.ParentRunID] = true
		parent, err := load(root.Run.ParentRunID)
		if err != nil {
			return nil, nil, err
		}
		if parent.Run.Deleted {
			return nil, nil, fmt.Errorf("worker budget ancestor is deleted")
		}
		registered := false
		for _, w := range parent.Workers {
			registered = registered || w.RunID == root.Run.ID && w.TaskID == root.Run.ParentTaskID
		}
		if !registered {
			return nil, nil, fmt.Errorf("worker budget ancestry is unregistered")
		}
		root = parent
	}
	queue := []*Snapshot{root}
	nodes := []*Snapshot{}
	seen := map[string]bool{}
	for len(queue) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		snap := queue[0]
		queue = queue[1:]
		if seen[snap.Run.ID] {
			return nil, nil, fmt.Errorf("duplicate worker ancestry %s", snap.Run.ID)
		}
		seen[snap.Run.ID] = true
		nodes = append(nodes, snap)
		children := map[string]bool{}
		for _, w := range snap.Workers {
			if children[w.RunID] {
				continue
			}
			children[w.RunID] = true
			child, err := load(w.RunID)
			if err != nil {
				return nil, nil, err
			}
			if child.Run.ParentRunID != snap.Run.ID || child.Run.ParentTaskID != w.TaskID {
				return nil, nil, fmt.Errorf("worker tree identity mismatch: %s", w.RunID)
			}
			queue = append(queue, child)
		}
	}
	if tx != nil {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, err
		}
	}
	return root, nodes, nil
}

func (s *Store) loadCapacityLocked(ctx context.Context, tx *sql.Tx, id string, budget *int) (*Snapshot, error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil, os.ErrNotExist
	}
	var source io.Reader
	var run []byte
	if tx != nil {
		var runSize int64
		err := tx.QueryRowContext(context.WithoutCancel(ctx), "SELECT length(CAST(run AS BLOB)), CASE WHEN length(CAST(run AS BLOB))<=? THEN run END FROM run_headers WHERE id=?", activityProjectionMaxBytes, id).Scan(&runSize, &run)
		if errors.Is(err, sql.ErrNoRows) {
			err = os.ErrNotExist
		}
		if err != nil {
			return nil, err
		}
		if runSize > activityProjectionMaxBytes {
			return nil, errActivityTooLarge
		}
		index, _, err := readSQLiteDisplayIndex(ctx, tx, id)
		if errors.Is(err, sql.ErrNoRows) {
			err = sqliteBuildLegacyDisplayIndex(ctx, tx, id)
			if err == nil {
				index, _, err = readSQLiteDisplayIndex(ctx, tx, id)
			}
		}
		if err != nil {
			return nil, err
		}
		source = bytes.NewReader(index.Capacity)
	} else {
		f, err := os.Open(filepath.Join(s.root, id+".json"))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		index, _, err := s.jsonDisplayIndex(ctx, id, f, true)
		if err != nil {
			return nil, err
		}
		source = bytes.NewReader(index.Capacity)
	}
	snap := &Snapshot{}
	if err := readCapacityProjection(ctx, source, snap, budget); err != nil {
		return nil, err
	}
	if tx != nil {
		// SQLite telemetry owns the current Run, independently of state bytes.
		snap.Run = Run{}
		if err := readCapacityProjection(ctx, bytes.NewReader(run), &snap.Run, budget); err != nil {
			return nil, err
		}
	}
	if snap.Run.ID == "" {
		return nil, os.ErrNotExist
	}
	if snap.Run.ID != id {
		return nil, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	return snap, nil
}

// These are the complete inputs of occupiedWorkerSlots, localWorkerOwner,
// resumableLocalTask and toolCleanupError. No evidence or result body is read
// into this detached Snapshot, and no caller may persist it or use it to admit.
var capacityProjectionFields = map[reflect.Type]string{
	reflect.TypeOf(Snapshot{}):         "run plan workers worker_context worker_settlement actions",
	reflect.TypeOf(Run{}):              "id parent_run_id parent_task_id status deleted resource_limits",
	reflect.TypeOf(PlanState{}):        "capacity tasks",
	reflect.TypeOf(PlanTask{}):         "id status worker_id child_run_id lease_until local_execution local_worker_visit disposition_reason",
	reflect.TypeOf(AgentWorker{}):      "id task_id run_id status cleanup_status visit settled_visit",
	reflect.TypeOf(WorkerContext{}):    "worker_id run_id parent_run_id task_id",
	reflect.TypeOf(WorkerSettlement{}): "parent_run_id task_id worker_id run_id visit cleanup_status observed_at",
	reflect.TypeOf(Action{}):           "adapter status result resource_cleanup_status resource_cleanup_error",
	reflect.TypeOf(ToolResult{}):       "worker_cleanup_error",
}

type capacityContextReader struct {
	ctx context.Context
	io.Reader
}

func (r capacityContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(p)
}

func readCapacityProjection(ctx context.Context, source io.Reader, out any, budget *int) error {
	r := &activityReader{Reader: capacityContextReader{ctx, source}, through: activityProjectionMaxBytes}
	d := json.NewDecoder(r)
	if err := readCapacityValue(ctx, d, r, reflect.ValueOf(out).Elem(), budget); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		if err != nil {
			return err
		}
		return errors.New("invalid trailing capacity snapshot data")
	}
	return ctx.Err()
}

func readCapacityValue(ctx context.Context, d *json.Decoder, r *activityReader, v reflect.Value, budget *int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.through = d.InputOffset() + activityProjectionMaxBytes
	typ := v.Type()
	if typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	fields, object := capacityProjectionFields[typ]
	if !object && v.Kind() != reflect.Slice {
		before := d.InputOffset()
		if err := d.Decode(v.Addr().Interface()); err != nil {
			return err
		}
		*budget -= int(d.InputOffset() - before)
		if *budget < 0 {
			return errActivityTooLarge
		}
		return nil
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	if token == nil {
		if v.Kind() == reflect.Pointer || v.Kind() == reflect.Slice {
			v.SetZero()
		}
		return nil
	}
	if v.Kind() == reflect.Slice {
		if token != json.Delim('[') {
			return errors.New("invalid capacity snapshot array")
		}
		count := 0
		for d.More() {
			// Charge every retained element, including capacity already allocated
			// by a previous append. The second element-size covers slice growth;
			// duplicate arrays deliberately consume the same conservative budget.
			*budget -= 2 * int(v.Type().Elem().Size())
			if *budget < 0 {
				return errActivityTooLarge
			}
			if count >= v.Cap() {
				v.Set(reflect.Append(v, reflect.Zero(v.Type().Elem())))
			} else if count >= v.Len() {
				v.SetLen(count + 1)
			}
			if err := readCapacityValue(ctx, d, r, v.Index(count), budget); err != nil {
				return err
			}
			count++
		}
		if count == 0 {
			v.Set(reflect.MakeSlice(v.Type(), 0, 0))
		} else {
			v.SetLen(count)
		}
	} else {
		if token != json.Delim('{') {
			return errors.New("invalid capacity snapshot object")
		}
		if v.Kind() == reflect.Pointer {
			if v.IsNil() {
				*budget -= int(typ.Size())
				if *budget < 0 {
					return errActivityTooLarge
				}
				v.Set(reflect.New(typ))
			}
			v = v.Elem()
		}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("invalid capacity snapshot field")
			}
			found := false
			for i := 0; i < v.NumField(); i++ {
				field := typ.Field(i)
				tag := strings.Split(field.Tag.Get("json"), ",")[0]
				if field.PkgPath == "" && strings.EqualFold(name, tag) && strings.Contains(" "+fields+" ", " "+tag+" ") {
					if err := readCapacityValue(ctx, d, r, v.Field(i), budget); err != nil {
						return err
					}
					found = true
					break
				}
			}
			if !found {
				if err := discardActivityJSON(d, r); err != nil {
					return err
				}
			}
		}
	}
	_, err = d.Token()
	return err
}
