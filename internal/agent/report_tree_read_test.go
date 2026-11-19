package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// A valid, strict three-level tree. Revoking only the leaf observation must
// reach the parent and root through the existing task provenance rules.
func reportReadTree(t *testing.T, backend string) (*Store, []Snapshot) {
	t.Helper()
	store, root := u7Store(t, backend)
	tree := []Snapshot{root}
	for len(tree) < 3 {
		next, err := store.Create("Review a recorded observation", "prompt", "hash")
		if err != nil {
			t.Fatal(err)
		}
		tree = append(tree, next)
	}
	for i := len(tree) - 1; i >= 0; i-- {
		proof := stageScopeSnapshot(t, nil)
		_, err := store.Update(tree[i].Run.ID, func(s *Snapshot) error {
			s.Run.Status, s.Run.RequireVerifiedCompletion, s.Run.SuccessContains = StatusCompleted, true, "true"
			s.Requests, s.Responses, s.Actions, s.Evidence = proof.Requests, proof.Responses, proof.Actions, proof.Evidence
			s.Events, s.Run.EventSeq = proof.Events, proof.Run.EventSeq
			s.Messages = append(proof.Messages, Message{Role: "assistant", Content: "The recorded observation met the review condition."})
			if i > 0 {
				s.Run.ParentRunID, s.Run.ParentTaskID = tree[i-1].Run.ID, "part"
			}
			if i+1 < len(tree) {
				child := &tree[i+1]
				w := AgentWorker{ID: "worker", RunID: child.Run.ID, TaskID: "part", Visit: 1, Status: "completed", CleanupStatus: "completed"}
				s.Workers = []AgentWorker{w}
				s.Plan.Tasks = []PlanTask{{ID: w.TaskID, WorkerID: w.ID, ChildRunID: w.RunID, Status: "completed"}}
				s.ChildReports = []ChildReport{newChildReport(s.Run.ID, w, child, "completed")}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		tree[i], err = store.Load(tree[i].Run.ID)
		if err != nil || tree[i].Result.Status != "success" || len(tree[i].Reports) == 0 {
			t.Fatalf("level %d: result=%s, reports=%d, err=%v", i, tree[i].Result.Status, len(tree[i].Reports), err)
		}
	}
	return store, tree
}

type reportReadFile struct {
	digest string
	mtime  int64
}

type reportReadState struct {
	runs      map[string]string // includes event sequences, event bodies and all report receipts
	files     map[string]reportReadFile
	dbVersion int
}

func observeReportReads(t *testing.T, store *Store, tree []Snapshot) func() reportReadState {
	t.Helper()
	dataVersion := func() int { return 0 }
	if store.sqlite {
		// A pinned observer sees commits even if a writer saved identical rows.
		ctx := context.Background()
		conn, err := store.sqlitePool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		dataVersion = func() int {
			var v int
			if err := conn.QueryRowContext(ctx, "PRAGMA data_version").Scan(&v); err != nil {
				t.Fatal(err)
			}
			return v
		}
	}
	return func() reportReadState {
		t.Helper()
		state := reportReadState{map[string]string{}, map[string]reportReadFile{}, dataVersion()}
		file := func(name string) {
			path := filepath.Join(store.root, name)
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			state.files[name] = reportReadFile{promptHash(string(data)), info.ModTime().UnixNano()}
		}
		for _, run := range tree {
			id := run.Run.ID
			s, err := store.loadRawLocked(id)
			if err != nil {
				t.Fatal(err)
			}
			state.runs[id] = valueHash(s)
			if !store.sqlite {
				file(id + ".json")
			}
		}
		for _, name := range fixtureCASFiles(t, store) {
			file(filepath.Join("artifacts", name))
		}
		return state
	}
}

func TestReportDownloadsReadOnlyAcrossSourceTree(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, tree := reportReadTree(t, backend)
			root := &tree[0]
			archive := preferredReport(*root)
			if archive == nil || archive.Kind != "final" {
				t.Fatal("missing root final")
			}
			hash := childReportHash(root)
			fixed := u7Read(t, store, archive.Files[1].Hash)
			legacy, err := store.ReportVersion(root.Run.ID, hash)
			if err != nil {
				t.Fatal(err)
			}
			for i := range tree {
				tree[i].Plan.Frontier = []string{"stale saved frontier"}
				if err := store.savePreparedLocked(tree[i]); err != nil {
					t.Fatal(err)
				}
			}
			observe := observeReportReads(t, store, tree)
			for _, revoked := range []bool{false, true} {
				if revoked {
					leaf := &tree[len(tree)-1]
					leaf.Evidence[0].Content += " changed"
					if err := store.savePreparedLocked(*leaf); err != nil {
						t.Fatal(err)
					}
				}
				before := observe()
				unchanged := func(route string) {
					t.Helper()
					if after := observe(); !reflect.DeepEqual(before, after) {
						t.Fatalf("%s wrote ledger/events=%t, files=%t, database=%t (revoked=%t)", route, !reflect.DeepEqual(before.runs, after.runs), !reflect.DeepEqual(before.files, after.files), before.dbVersion != after.dbVersion, revoked)
					}
				}
				_, body, err := store.ReportDocument(root.Run.ID, "", "en")
				if (err != nil) != revoked || !revoked && body != fixed {
					t.Fatalf("default export (revoked=%t): %v", revoked, err)
				}
				unchanged("default")
				body, err = store.ReportVersion(root.Run.ID, hash)
				if (err != nil) != revoked || !revoked && body != legacy {
					t.Fatalf("source_hash export (revoked=%t): %v", revoked, err)
				}
				unchanged("source_hash")
				ref, body, err := store.ReportDocument(root.Run.ID, archive.ID, "en")
				if err != nil || ref.Hash != archive.Files[1].Hash || body != fixed {
					t.Fatalf("report_id changed (revoked=%t): %v", revoked, err)
				}
				unchanged("report_id")
			}
			// The same invalidation must still be committed by normal Store.Load.
			if _, err := store.Load(root.Run.ID); err != nil {
				t.Fatal(err)
			}
			for i, run := range tree {
				s, err := store.loadRawLocked(run.Run.ID)
				if err != nil || s.Result.Status == "success" || s.Run.EventSeq <= run.Run.EventSeq || len(s.Reports) <= len(run.Reports) {
					t.Fatalf("normal load did not persist level %d revocation, event and archive: %v", i, err)
				}
			}
		})
	}
}

func TestReportSingleMissingLanguagePreservesReceipt(t *testing.T) {
	langs := []string{"zh-CN", "en"}
	for _, backend := range []string{"json", "sqlite"} {
		for missing, lang := range langs {
			t.Run(backend+"/missing-"+lang, func(t *testing.T) {
				store, s := u7Store(t, backend)
				raw, err := json.Marshal(map[string]any{"trace_result": Result{Status: "failed", Summary: u7Text.ZhCN, Report: &u7Text}})
				if err != nil {
					t.Fatal(err)
				}
				s, err = store.Update(s.Run.ID, func(s *Snapshot) error {
					s.Run.Status = StatusCompleted
					s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
					return nil
				})
				if err != nil || len(s.Reports) != 1 || s.Reports[0].TranslationStatus != "complete" || len(s.Reports[0].Files) != 2 {
					t.Fatalf("bilingual fixture: %+v, %v", s.Reports, err)
				}
				receipt := s.Reports[0]
				remaining := receipt.Files[1-missing]
				want := u7Read(t, store, remaining.Hash)
				if err := os.Rename(filepath.Join(store.root, "artifacts", receipt.Files[missing].Hash), filepath.Join(store.root, "withheld-report")); err != nil {
					t.Fatal(err)
				}
				observe := observeReportReads(t, store, []Snapshot{s})
				before := observe()
				saved, err := store.loadRawLocked(s.Run.ID)
				if err != nil {
					t.Fatal(err)
				}
				view := ReportReceipts(saved)
				if len(view) != 1 || view[0].ID != receipt.ID || view[0].SourceStatus != "current" || view[0].TranslationStatus != "complete" || len(view[0].MissingLanguages) != 0 || !reflect.DeepEqual(view[0].Files, []ArtifactRef{remaining}) || !reflect.DeepEqual(saved.Reports[0], receipt) {
					t.Fatalf("file loss changed receipt or projection: %+v", view)
				}
				for _, id := range []string{"", receipt.ID} {
					if _, _, err := store.ReportDocument(s.Run.ID, id, lang); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("missing %s, report_id=%q: %v", lang, id, err)
					}
					ref, body, err := store.ReportDocument(s.Run.ID, id, langs[1-missing])
					if err != nil || ref.Hash != remaining.Hash || body != want {
						t.Fatalf("surviving language, report_id=%q: %v", id, err)
					}
				}
				if !reflect.DeepEqual(observe(), before) {
					t.Fatal("partial-file view or download rewrote the ledger or CAS")
				}
			})
		}
	}
}
