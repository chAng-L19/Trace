package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func fixtureCASFiles(t *testing.T, store *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(store.root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	var hashes []string
	for _, entry := range entries {
		if !entry.IsDir() {
			hashes = append(hashes, entry.Name())
		}
	}
	return hashes
}

func TestPermanentDeleteRemovesHistoryCachesAndExclusiveCAS(t *testing.T) {
	_, store, snap, ref := catalogFixture(t)
	id := snap.Run.ID
	if _, err := store.Observe(id); err != nil {
		t.Fatal(err)
	}
	if _, err := store.viewFields(id, true); err != nil {
		t.Fatal(err)
	}
	// A purely local chain is removed with its owner, not a deletion blocker.
	_, err := store.CreateEvidenceChain(id, EvidenceChain{Name: "owned", Summary: "local source", Steps: []EvidenceChainStep{{RunID: id, EvidenceID: snap.Evidence[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	hashes := fixtureCASFiles(t, store)
	result, err := store.PurgeRun(id)
	if err != nil || !result.Permanent || !result.Deleted || result.ArtifactsDeleted != len(hashes) || result.ArtifactsRetained != 0 {
		t.Fatalf("purge: %+v %v", result, err)
	}
	if _, err = os.Stat(filepath.Join(store.root, id+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("history remains", err)
	}
	if _, err = store.OpenArtifact(ref.Hash); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exclusive CAS remains", err)
	}
	for _, hash := range hashes {
		if _, err = os.Stat(filepath.Join(store.root, "artifacts", hash)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("exclusive report or evidence CAS remains", hash, err)
		}
	}
	if _, err = store.Load(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Load resurrected history", err)
	}
	if _, err = store.Observe(id); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Observe resurrected history", err)
	}
	if _, _, err = store.EventBatch(id, 0); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("events resurrected history", err)
	}
	reopened, _ := NewStore(store.root)
	if runs, err := reopened.ListWithDeleted(); err != nil || len(runs) != 0 {
		t.Fatal("reopened list", runs, err)
	}
	if _, err = os.Stat(filepath.Join(store.root, id+".lock")); err != nil {
		t.Fatal("lock inode removed", err)
	}
}

func TestPermanentDeleteIncludesSettledWorkerDescendants(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	parent := createTestRun(t, a)
	child := createTestRun(t, a)
	grand := createTestRun(t, a)
	for p, c := range map[string]string{parent: child, child: grand} {
		_, err := store.Update(c, func(s *Snapshot) error { s.Run.ParentRunID = p; s.Run.ParentTaskID = "task"; return nil })
		if err != nil {
			t.Fatal(err)
		}
		_, err = store.Update(p, func(s *Snapshot) error {
			s.Workers = []AgentWorker{{ID: "worker", RunID: c, Status: "completed", CleanupStatus: "completed"}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	// An external reference to any descendant prevents the entire removal.
	other := createTestRun(t, a)
	_, err := store.Update(other, func(s *Snapshot) error {
		s.EvidenceChainRefs = []EvidenceChainReference{{SourceRunID: grand, ChainID: "missing"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(store.root, parent+".json"))
	if _, err = store.PurgeRun(parent); err == nil {
		t.Fatal("deleted referenced closure")
	}
	after, _ := os.ReadFile(filepath.Join(store.root, parent+".json"))
	if !bytes.Equal(before, after) {
		t.Fatal("failed purge modified history")
	}
	if _, err = store.PurgeRun(other); err != nil {
		t.Fatal(err)
	}
	result, err := store.PurgeRun(parent)
	if err != nil || len(result.RunIDs) != 3 {
		t.Fatalf("closure: %+v %v", result, err)
	}
	for _, id := range []string{parent, child, grand} {
		if _, err := store.Load(id); !errors.Is(err, os.ErrNotExist) {
			t.Fatal(id, err)
		}
	}
}

func TestPermanentDeleteProtectedReferencesAndOwners(t *testing.T) {
	for _, kind := range []string{"branch", "chain-reference", "derived", "step", "asset", "context", "attachment", "active", "lock", "dispatch", "worker-cleanup"} {
		t.Run(kind, func(t *testing.T) {
			a, store := newTestAgent(t, successProvider{})
			id := createTestRun(t, a)
			other := createTestRun(t, a)
			_, err := store.Update(other, func(s *Snapshot) error {
				s.Run.Deleted = true // Hidden consumers still protect their sources.
				switch kind {
				case "branch":
					s.Run.ParentRunID = id
				case "chain-reference":
					s.EvidenceChainRefs = []EvidenceChainReference{{SourceRunID: id}}
				case "derived":
					s.EvidenceChains = []EvidenceChain{{DerivedFrom: []EvidenceChainReference{{SourceRunID: id}}}}
				case "step":
					s.EvidenceChains = []EvidenceChain{{Steps: []EvidenceChainStep{{RunID: id}}}}
				case "asset":
					s.Assets = []Asset{{SourceRunID: id}}
				case "context":
					s.WorkerContext = &WorkerContext{Dependencies: []WorkerContextTask{{SourceRunID: id}}}
				case "attachment":
					s.Messages = []Message{{Attachments: []InputAttachment{{SourceRunID: id}}}}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = store.Update(id, func(s *Snapshot) error {
				switch kind {
				case "active":
					s.Run.Status = StatusRunning
				case "dispatch":
					s.Plan.Tasks = []PlanTask{{ID: "starting", Status: "running"}}
				case "worker-cleanup":
					s.Workers = []AgentWorker{{Status: "completed", CleanupStatus: "unknown"}}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if kind == "lock" {
				unlock, err := store.AcquireRunLock(id)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			before, _ := os.ReadFile(filepath.Join(store.root, id+".json"))
			if _, err = store.PurgeRun(id); err == nil {
				t.Fatal("protected source removed")
			}
			after, _ := os.ReadFile(filepath.Join(store.root, id+".json"))
			if !bytes.Equal(before, after) {
				t.Fatal("failed purge changed source")
			}
		})
	}
}

func TestPermanentDeleteHiddenSourceKeepsSharedCAS(t *testing.T) {
	_, store, snap, ref := catalogFixture(t)
	if _, err := store.PutInputAttachment("shared.txt", bytes.NewBufferString("flag{verified}")); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(snap.Run.ID); err != nil {
		t.Fatal(err)
	}
	if all, err := store.ListWithDeleted(); err != nil || len(all) != 1 || !all[0].Deleted {
		t.Fatal("hidden management list", all, err)
	}
	hashes := fixtureCASFiles(t, store)
	result, err := store.PurgeRun(snap.Run.ID)
	if err != nil || result.ArtifactsDeleted != len(hashes)-1 || result.ArtifactsRetained != 1 {
		t.Fatal(result, err)
	}
	f, err := store.OpenArtifact(ref.Hash)
	if err != nil {
		t.Fatal("shared upload lost", err)
	}
	f.Close()
	for _, hash := range hashes {
		if hash == ref.Hash {
			continue
		}
		if _, err = os.Stat(filepath.Join(store.root, "artifacts", hash)); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unshared report CAS retained", hash, err)
		}
	}
}

func TestPermanentDeleteActivePublisherRetainsCASWithWarning(t *testing.T) {
	a, store, snap, ref := catalogFixture(t)
	hashes := fixtureCASFiles(t, store)
	other := createTestRun(t, a)
	release, err := store.AcquireRunLock(other)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	result, err := store.PurgeRun(snap.Run.ID)
	if err != nil || !result.Deleted || result.CleanupWarning == "" || result.ArtifactsRetained != len(hashes) || result.ArtifactsDeleted != 0 {
		t.Fatal(result, err)
	}
	f, err := store.OpenArtifact(ref.Hash)
	if err != nil {
		t.Fatal("in-flight CAS removed", err)
	}
	f.Close()
	for _, hash := range hashes {
		if _, err = os.Stat(filepath.Join(store.root, "artifacts", hash)); err != nil {
			t.Fatal("in-flight report or evidence CAS removed", hash, err)
		}
	}
}

func TestPermanentDeleteRacingBranchDoesNotLoseSource(t *testing.T) {
	for i := 0; i < 10; i++ {
		a, store := newTestAgent(t, successProvider{})
		input, err := store.PutInputAttachment("source.txt", bytes.NewBufferString("source"))
		if err != nil {
			t.Fatal(err)
		}
		source, err := a.Create("source", input)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var branch Snapshot
		var branchErr error
		go func() { defer wg.Done(); <-start; branch, branchErr = a.Branch(source.Run.ID, source.Messages[0].ID) }()
		go func() { defer wg.Done(); <-start; _, _ = store.PurgeRun(source.Run.ID) }()
		close(start)
		wg.Wait()
		if branchErr == nil {
			if _, err := store.Load(source.Run.ID); err != nil {
				t.Fatal("successful branch lost source", branch.Run.ID, err)
			}
		}
	}
}

func TestPermanentDeleteRecoversInterruptedStaging(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "rollback", true: "finish"}[committed], func(t *testing.T) {
			_, store, snap, ref := catalogFixture(t)
			stage, err := os.MkdirTemp(store.root, ".run-delete-")
			if err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(filepath.Join(store.root, snap.Run.ID+".json"))
			if err = os.Rename(filepath.Join(store.root, "artifacts", ref.Hash), filepath.Join(stage, "cas-"+ref.Hash)); err != nil {
				t.Fatal(err)
			}
			if err = os.Rename(filepath.Join(store.root, snap.Run.ID+".json"), filepath.Join(stage, "run-"+snap.Run.ID+".json")); err != nil {
				t.Fatal(err)
			}
			if committed {
				if err = commitRunPurge(stage); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := NewStore(store.root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("stage not settled", err)
			}
			if committed {
				if _, err = reopened.Load(snap.Run.ID); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("committed deletion restored", err)
				}
			} else {
				restored, _ := os.ReadFile(filepath.Join(store.root, snap.Run.ID+".json"))
				if !bytes.Equal(original, restored) {
					t.Fatal("rollback changed original")
				}
				f, err := reopened.OpenArtifact(ref.Hash)
				if err != nil {
					t.Fatal("rollback lost CAS", err)
				}
				f.Close()
			}
		})
	}
}

func TestPermanentDeleteExistingStoreRecoversMissingDescendant(t *testing.T) {
	a, store := newTestAgent(t, successProvider{})
	parent := createTestRun(t, a)
	child := createTestRun(t, a)
	if _, err := store.Update(child, func(s *Snapshot) error { s.Run.ParentRunID = parent; s.Run.ParentTaskID = "task"; return nil }); err != nil {
		t.Fatal(err)
	}
	stage, err := os.MkdirTemp(store.root, ".run-delete-")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(store.root, child+".json"), filepath.Join(stage, "run-"+child+".json")); err != nil {
		t.Fatal(err)
	}
	result, err := store.PurgeRun(parent)
	if err != nil || len(result.RunIDs) != 2 {
		t.Fatal("interrupted child omitted", result, err)
	}
	reopened, err := NewStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{parent, child} {
		if _, err = reopened.Load(id); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("deleted closure resurrected", id, err)
		}
	}
}
