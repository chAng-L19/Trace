package agent

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestReportCommitReturnsPersistedArchivesAfterSourceRefresh(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			store, initial := u7Store(t, backend)
			raw, err := json.Marshal(map[string]any{"trace_result": Result{Status: "failed", Summary: u7Text.ZhCN, Report: &u7Text}})
			if err != nil {
				t.Fatal(err)
			}
			updated, err := store.Update(initial.Run.ID, func(s *Snapshot) error {
				s.Run.Status = StatusCompleted
				s.Messages = append(s.Messages, Message{Role: "assistant", Content: string(raw)})
				return nil
			})
			if err != nil || len(updated.Reports) != 1 {
				t.Fatalf("update did not return its archived report: %+v %v", updated.Reports, err)
			}
			readRaw := func() Snapshot {
				t.Helper()
				unlock, lockErr := store.acquireStoreLock()
				if lockErr != nil {
					t.Fatal(lockErr)
				}
				defer unlock()
				s, loadErr := store.loadRawLocked(initial.Run.ID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				return s
			}
			if persisted := readRaw(); !reflect.DeepEqual(updated.Reports, persisted.Reports) {
				t.Fatal("update returned different archives from its commit")
			}
			original := updated.Reports[0]
			originalBody := u7Read(t, store, original.Files[0].Hash)
			// Simulate a persisted stale derived result. The lower-level save still
			// archives its input; verified Load must revoke it and archive the refresh.
			updated.Run.Goal += " changed source"
			updated.Result.Status = "success"
			unlock, err := store.acquireStoreLock()
			if err != nil {
				t.Fatal(err)
			}
			err = store.saveLocked(updated)
			unlock()
			if err != nil {
				t.Fatal(err)
			}
			stale := readRaw()
			if len(stale.Reports) != 2 || stale.Result.Status != "success" {
				t.Fatalf("direct save skipped report archiving: %+v", stale.Reports)
			}
			loaded, err := store.Load(initial.Run.ID)
			if err != nil || loaded.Result.Status != "failed" || len(loaded.Reports) != 3 {
				t.Fatalf("source refresh did not return its new archive: %+v %v", loaded.Reports, err)
			}
			persisted := readRaw()
			if !reflect.DeepEqual(loaded.Reports, persisted.Reports) || !reflect.DeepEqual(loaded.Result, persisted.Result) {
				t.Fatal("verified load returned different reports or result from its commit")
			}
			latest := loaded.Reports[len(loaded.Reports)-1]
			if latest.SourceDigest == stale.Reports[len(stale.Reports)-1].SourceDigest || latest.TranslationStatus != "translation_missing" || u7Read(t, store, original.Files[0].Hash) != originalBody {
				t.Fatal("source refresh reused stale translation or changed the old CAS report")
			}
		})
	}
}
