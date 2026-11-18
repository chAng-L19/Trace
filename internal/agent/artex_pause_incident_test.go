package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Opt-in diagnostics on a disposable server copy. No product hooks/logging,
// payload contents, elevated limits, or forced GC alter the measured path.
func TestArtexIncidentPauseStages(t *testing.T) {
	root := os.Getenv("TRACE_INCIDENT_FIXTURE_DIR")
	if root == "" {
		t.Skip("requires isolated complete incident fixture")
	}
	root, err := filepath.Abs(root)
	if err != nil || !strings.HasPrefix(filepath.ToSlash(root), "/home/ubuntu/trace/validation/") {
		t.Fatal("requires disposable server validation directory")
	}
	started := time.Now()
	mark := func(stage string) {
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		t.Logf("stage=%s elapsed_ms=%.2f heap_alloc=%d heap_inuse=%d sys=%d gc=%d", stage, float64(time.Since(started).Microseconds())/1000, m.HeapAlloc, m.HeapInuse, m.Sys, m.NumGC)
	}
	store, err := NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const id = "21747a3b09678fd8d7e854dc82969267"
	unlock, err := store.acquireStoreLock()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	store.beforePersist = func(string) { mark("before_persist") }
	mark("before_raw")
	snap, err := store.loadRawLocked(id)
	if err != nil {
		t.Fatal(err)
	}
	mark("after_raw")
	beforeResult := snap.Result
	before, err := pauseStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	mark("after_initial_hash")
	changed := store.refreshDerivedLocked(&snap, map[string]bool{id: true})
	mark("after_initial_refresh")
	after, err := pauseStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	mark("after_refreshed_hash")
	t.Logf("initial_derived_changed=%t result_changed=%t body_unchanged=%t run_status=%s events=%d requests=%d", changed, !reflect.DeepEqual(beforeResult, snap.Result), before == after, snap.Run.Status, len(snap.Events), len(snap.Requests))
	if mode := os.Getenv("TRACE_PAUSE_STAGE_COMMIT"); mode != "1" && mode != "sql" {
		return
	}
	if err = store.archiveReports(&snap); err != nil {
		t.Fatal(err)
	}
	mark("after_initial_archive")
	archived, err := pauseStateHash(snap)
	if err != nil {
		t.Fatal(err)
	}
	mark("after_archived_hash")
	t.Logf("archive_body_unchanged=%t", before == archived)
	if before != archived {
		t.Fatal("initial source/archive body changed; diagnostic declines control-only save")
	}
	if os.Getenv("TRACE_PAUSE_STAGE_COMMIT") == "sql" {
		// Retained legacy-row diagnostic: version 3 deliberately does not execute
		// this UPDATE in production. Compare it with mode=1's sqliteSaveControl.
		run, err := json.Marshal(snap.Run)
		if err != nil {
			t.Fatal(err)
		}
		result, err := json.Marshal(snap.Result)
		if err != nil {
			t.Fatal(err)
		}
		observation, err := json.Marshal(Observation{Run: snap.Run, Plan: snap.Plan, Workers: snap.Workers, WorkerMessages: snap.WorkerMessages, Mailbox: snap.Mailbox})
		if err != nil {
			t.Fatal(err)
		}
		tx, err := store.sqlitePool.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		mark("before_legacy_sql_row_update")
		if _, err = tx.Exec("UPDATE runs SET run=?,observation=?,result_override=? WHERE id=?", run, observation, result, id); err != nil {
			t.Fatal(err)
		}
		mark("after_legacy_sql_row_update")
		if err = sqliteWriteEvents(tx, snap.Run, snap.Events); err != nil {
			t.Fatal(err)
		}
		mark("after_sql_events")
		if err = tx.Commit(); err != nil {
			t.Fatal(err)
		}
		mark("after_sql_commit")
		return
	}
	snap.Run.Status, snap.Run.PauseReason = StatusPaused, "operator_pause"
	snap.Run.UpdatedAt = time.Now().UTC()
	addEvent(&snap, "run_paused", "已请求暂停运行", nil)
	refreshUsage(&snap)
	store.refreshDerivedLocked(&snap, map[string]bool{id: true})
	mark("after_pause_refresh")
	if err = store.archiveReports(&snap); err != nil {
		t.Fatal(err)
	}
	mark("after_pause_archive")
	if err = store.sqliteSaveControl(snap); err != nil {
		t.Fatal(err)
	}
	mark("after_control_commit")
}
