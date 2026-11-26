package agent

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Snapshot struct {
	// Rebuilt under the Store lock through provenance sourceFor; never serialized.
	delegatedAcceptance         map[string]delegatedObservation
	delegatedDescendantsRetired bool

	Reports              []ReportArchive              `json:"reports,omitempty"`
	ReportDelivery       *ReportDeliveryState         `json:"report_delivery,omitempty"`
	Recon                *ReconContext                `json:"recon,omitempty"`
	SearchUsage          map[string]SearchSourceUsage `json:"search_usage,omitempty"`
	ChildReports         []ChildReport                `json:"child_reports,omitempty"`
	RootContract         *RootContract                `json:"root_contract,omitempty"`
	artifactRoot         string
	workerContextMessage string
	WorkerContext        *WorkerContext            `json:"worker_context,omitempty"`
	WorkerCheckpoint     *WorkerCheckpoint         `json:"worker_checkpoint,omitempty"`
	WorkerSettlement     *WorkerSettlement         `json:"worker_settlement,omitempty"`
	Plan                 PlanState                 `json:"plan"`
	Assets               []Asset                   `json:"assets,omitempty"`
	Relations            []AssetRelation           `json:"relations,omitempty"`
	Workers              []AgentWorker             `json:"workers,omitempty"`
	WorkerMessages       []WorkerMessage           `json:"worker_messages,omitempty"`
	Mailbox              []QueuedMessage           `json:"mailbox,omitempty"`
	Compactions          []Compaction              `json:"compactions,omitempty"`
	Run                  Run                       `json:"run"`
	Prompt               string                    `json:"prompt"`
	Messages             []Message                 `json:"messages"`
	Requests             []ModelRequest            `json:"requests"`
	Events               []Event                   `json:"events"`
	Actions              []Action                  `json:"actions,omitempty"`
	Responses            []ResponseRecord          `json:"responses,omitempty"`
	Evidence             []Evidence                `json:"evidence"`
	EvidenceChains       []EvidenceChain           `json:"evidence_chains,omitempty"`
	EvidenceChainRefs    []EvidenceChainReference  `json:"evidence_chain_refs,omitempty"`
	Result               Result                    `json:"result"`
	Requirements         []VerificationRequirement `json:"requirements,omitempty"`
	Checks               []VerificationCheck       `json:"checks,omitempty"`
	Findings             []Finding                 `json:"findings,omitempty"`
}

type Store struct {
	sqlite             bool
	sqlitePool         *sql.DB
	sqliteDisplayConn  *sql.Conn // Reserved read connection; activityReadMu protects its generation.
	workerActivityMu   sync.Mutex
	workerActivities   map[string]*workerActivityCache
	artifactVerified   sync.Map
	observations       sync.Map
	eventViews         sync.Map
	eventViewMu        sync.Mutex
	viewReadMu         sync.Mutex        // Bound cold JSON views before allocating history.
	activityReadMu     sync.Mutex        // Coalesce cold activity reads before decoding.
	activityCache      activityReadCache // Small display bytes only; guarded by eventViewMu.
	eventViewEpoch     uint64            // Cache-release fence only; never execution state.
	eventReaders       map[string]int
	eventChanges       map[string]chan struct{}
	scheduling         map[string]*schedulingSubscription
	diagnostics        storeDiagnosticCounters
	root               string
	mu                 sync.Mutex
	beforePersist      func(string) // Tests inject filesystem faults under the Store lock.
	beforeSQLiteCommit func()       // Tests stop the process at the database commit boundary.
}

func NewStore(root string) (_ *Store, resultErr error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	store := &Store{root: root}
	defer func() {
		if resultErr != nil {
			store.Close()
		}
	}()
	migrationUnlock, migrationErr := acquireFileLock(filepath.Join(root, ".store.lock"), true)
	if migrationErr != nil {
		return nil, migrationErr
	}
	migrationErr = recoverSQLiteMigration(root)
	migrationUnlock()
	if migrationErr != nil {
		return nil, migrationErr
	}
	if _, err := os.Stat(filepath.Join(root, sqliteStoreName)); err == nil {
		store.sqlite = true
		store.sqlitePool, err = openSQLite(filepath.Join(root, sqliteStoreName), false)
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	unlock, err := store.acquireStoreLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	if store.sqlite {
		db, e := store.sqliteDB()
		if e != nil {
			return nil, e
		}
		var version int
		var mode string
		if e = db.QueryRow("PRAGMA user_version").Scan(&version); e != nil {
			return nil, e
		}
		if e = db.QueryRow("PRAGMA journal_mode").Scan(&mode); e != nil {
			return nil, e
		}
		if (version != 1 && version != 2 && version != 3) || mode != "wal" {
			return nil, fmt.Errorf("unsupported SQLite Store version/mode: %d/%s", version, mode)
		}
		if e = ensureSQLiteControlSchema(db, version); e != nil {
			return nil, e
		}
		if _, e = db.Exec(displaySchemaSQL); e != nil {
			return nil, e
		}
	}
	return store, nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *Store) Create(goal, prompt, promptHash string) (Snapshot, error) {
	return s.createInitialized(goal, prompt, promptHash, nil)
}

// Publish a configured Run once: no observer or crash recovery can execute an
// intermediate snapshot that lacks the declared budget or acceptance contract.
func (s *Store) createInitialized(goal, prompt, promptHash string, initialize func(*Snapshot) error) (Snapshot, error) {
	return s.createInitializedWithID("", goal, prompt, promptHash, initialize)
}

func (s *Store) createInitializedWithID(id, goal, prompt, promptHash string, initialize func(*Snapshot) error) (Snapshot, error) {
	waitStarted := time.Now()
	s.mu.Lock()
	s.diagnostics.record(storeMutexWait, waitStarted, 0)
	defer s.mu.Unlock()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	return s.createInitializedIDLocked(id, goal, prompt, promptHash, initialize)
}

func (s *Store) createLocked(goal, prompt, promptHash string) (Snapshot, error) {
	return s.createInitializedLocked(goal, prompt, promptHash, nil)
}

func (s *Store) createInitializedLocked(goal, prompt, promptHash string, initialize func(*Snapshot) error) (Snapshot, error) {
	return s.createInitializedIDLocked("", goal, prompt, promptHash, initialize)
}

func (s *Store) createInitializedIDLocked(id, goal, prompt, promptHash string, initialize func(*Snapshot) error) (Snapshot, error) {
	if id == "" {
		var err error
		id, err = newID()
		if err != nil {
			return Snapshot{}, err
		}
	} else {
		if raw, err := hex.DecodeString(id); err != nil || len(raw) != 16 {
			return Snapshot{}, fmt.Errorf("invalid reserved run ID")
		}
		if _, err := s.loadRawLocked(id); err == nil {
			return Snapshot{}, fmt.Errorf("reserved run already exists: %w", os.ErrExist)
		} else if !errors.Is(err, os.ErrNotExist) {
			return Snapshot{}, err
		}
	}
	now := time.Now().UTC()
	snap := Snapshot{Run: Run{ID: id, Goal: goal, Status: StatusCreated, PromptHash: promptHash, PromptBytes: len([]byte(prompt)), CreatedAt: now, UpdatedAt: now}, Prompt: prompt}
	snap.artifactRoot = s.root
	if initialize != nil {
		if err := initialize(&snap); err != nil {
			return Snapshot{}, err
		}
		assignMessageIDs(&snap)
		refreshUsage(&snap)
		s.refreshDerivedLocked(&snap, map[string]bool{id: true})
	}
	if err := s.saveLocked(snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func (s *Store) Load(id string) (Snapshot, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	return s.loadLocked(id)
}

func (s *Store) loadLocked(id string) (Snapshot, error) {
	started := time.Now()
	defer s.diagnostics.record(storeLoadVerify, started, 0)
	return s.loadVerifiedLocked(id, map[string]bool{})
}

func (s *Store) loadRawLocked(id string) (Snapshot, error) {
	if s.sqlite {
		return s.sqliteLoad(id)
	}
	if id == "" || strings.ContainsAny(id, `/\\`) || id == "." || id == ".." {
		return Snapshot{}, os.ErrNotExist
	}
	f, err := os.Open(filepath.Join(s.root, id+".json"))
	if err != nil {
		return Snapshot{}, err
	}
	defer f.Close()
	var snap Snapshot
	if err = decodeSnapshot(f, &snap); err != nil {
		return Snapshot{}, err
	}
	if snap.Run.ID == "" {
		return Snapshot{}, os.ErrNotExist
	}
	if snap.Run.ID != id {
		return Snapshot{}, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	snap.artifactRoot = s.root
	return snap, nil
}

func (s *Store) List() ([]Run, error) {
	return s.listRuns(false)
}

func (s *Store) ListWithDeleted() ([]Run, error) {
	return s.listRuns(true)
}

func (s *Store) listRuns(includeDeleted bool) ([]Run, error) {
	if s.sqlite {
		return s.sqliteList(includeDeleted)
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var runs []Run
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		run, err := s.listRun(strings.TrimSuffix(entry.Name(), ".json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if includeDeleted || !run.Deleted {
			runs = append(runs, run)
		}
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].CreatedAt.After(runs[j].CreatedAt) })
	return runs, nil
}

func (s *Store) saveLocked(snap Snapshot) error {
	if err := s.archiveReports(&snap); err != nil {
		return err
	}
	return s.savePreparedLocked(snap)
}

// The caller has already archived reports on this exact snapshot under the
// current Store lock. Keep the same persistence path without a second archive.
func (s *Store) savePreparedLocked(snap Snapshot) error {
	started := time.Now()
	defer s.diagnostics.record(storeSave, started, 0)
	if s.sqlite {
		return s.sqliteSave(snap)
	}
	if info, err := os.Stat(filepath.Join(s.root, snap.Run.ID+".json")); err == nil && info.Size() > largeSnapshotBytes {
		return s.saveLargeSnapshotLocked(snap)
	}
	encodeStarted := time.Now()
	b, err := json.Marshal(snap)
	s.diagnostics.record(storeJSONEncode, encodeStarted, int64(len(b)))
	if err != nil {
		return err
	}
	// Only readers opt into retaining full event history. A closed stream must
	// not have its released view resurrected by a concurrent commit.
	s.eventViewMu.Lock()
	_, retainEvents := s.eventViews.Load(snap.Run.ID)
	retainEvents = retainEvents || s.eventReaders[snap.Run.ID] > 0
	s.eventViewMu.Unlock()
	var view *storedView
	if retainEvents {
		view = &storedView{}
		if err = view.retainSnapshot(b); err != nil {
			return err
		}
		// These are the exact headers just marshalled above. Keep a detached
		// index so the first SSE read need not decode every historical body
		// again just to recover sequence numbers after a semantic commit.
		view.sequences = make([]int, len(snap.Events))
		for i, event := range snap.Events {
			view.sequences[i] = event.Sequence
		}
	}
	metadata, err := json.Marshal(Observation{Run: snap.Run, Plan: snap.Plan, Workers: snap.Workers, WorkerMessages: snap.WorkerMessages, Mailbox: snap.Mailbox})
	if err != nil {
		return err
	}
	staged, err := s.stageSnapshotLocked(snap.Run.ID, b)
	if err != nil {
		return err
	}
	defer os.Remove(staged.path)
	if err := s.publishSnapshotViewsLocked(snap.Run.ID, staged, view, metadata); err != nil {
		return err
	}
	s.notifyEventCommit(snap.Run.ID)
	s.notifySchedulingCommit(snap.Run.ID, snap.Events, snap.Run.EventSeq)
	return nil
}

func (s *Store) stageSnapshotLocked(id string, parts ...[]byte) (*stagedSnapshot, error) {
	if s.beforePersist != nil {
		s.beforePersist(id)
	}
	return s.stageSnapshot(parts...)
}

// Readers select the file generation and cache under this same short lock.
// Encoding, writing, fsync, cold decoding, and notifications stay outside it.
func (s *Store) publishSnapshotViewsLocked(id string, staged *stagedSnapshot, view *storedView, metadata json.RawMessage) error {
	started := time.Now()
	defer s.diagnostics.record(storeJSONPublish, started, 0)
	s.prepareJSONDisplayIndex(id, staged, view)
	if staged.displayPath != "" {
		defer os.Remove(staged.displayPath)
	}
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	info, err := s.publishSnapshotLocked(id, staged)
	if err != nil {
		return err
	}
	if view != nil {
		// Replace any retained old-generation view, not only the exact pointer
		// used to build the candidate. A completed cold reader may have primed
		// that same generation meanwhile. Never recreate a released cache.
		if _, retained := s.eventViews.Load(id); retained || s.eventReaders[id] > 0 {
			view.info = info
			s.eventViews.Store(id, view)
		}
	} else {
		s.eventViews.Delete(id)
	}
	s.observations.Store(id, &storedView{info: info, observation: metadata})
	return nil
}

type stagedSnapshot struct {
	path        string
	info        os.FileInfo
	displayPath string
}

// A candidate is not a committed generation. Expensive writes and fsync can
// happen before taking the store-wide lock; only its guarded rename publishes.
func (s *Store) stageSnapshot(parts ...[]byte) (_ *stagedSnapshot, err error) {
	started := time.Now()
	var written int64
	defer func() { s.diagnostics.record(storeJSONWriteSync, started, written) }()
	f, err := os.CreateTemp(s.root, ".trace-*.tmp")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()
	for _, part := range parts {
		var n int
		n, err = f.Write(part)
		written += int64(n)
		if err != nil {
			return nil, err
		}
	}
	if err = f.Sync(); err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	return &stagedSnapshot{path: tmp, info: info}, nil
}

func (s *Store) publishSnapshotLocked(id string, staged *stagedSnapshot) (os.FileInfo, error) {
	// Publishing the derived candidate first is safe: old readers reject its
	// generation, while the original atomic state rename remains the commit.
	if staged.displayPath != "" {
		_ = os.Rename(staged.displayPath, filepath.Join(s.root, id+".display"))
	}
	if err := os.Rename(staged.path, filepath.Join(s.root, id+".json")); err != nil {
		return nil, fmt.Errorf("persist run: %w", err)
	}
	return staged.info, nil
}

func (s *Store) Update(id string, fn func(*Snapshot) error) (Snapshot, error) {
	waitStarted := time.Now()
	s.mu.Lock()
	s.diagnostics.record(storeMutexWait, waitStarted, 0)
	defer s.mu.Unlock()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	return s.updateLocked(id, fn)
}

func (s *Store) acquireStoreLock() (func(), error) {
	// ponytail: one store-wide lock suits local snapshots; use database transactions if throughput requires it.
	started := time.Now()
	unlock, err := acquireFileLock(filepath.Join(s.root, ".store.lock"), true)
	s.diagnostics.record(storeFileLockWait, started, 0)
	return s.finishStoreLock(unlock, err)
}

func (s *Store) finishStoreLock(unlock func(), err error) (func(), error) {
	if err != nil {
		return nil, err
	}
	if !s.sqlite {
		if _, e := os.Stat(filepath.Join(s.root, sqliteStoreMarker)); e == nil {
			unlock()
			return nil, fmt.Errorf("SQLite authority marker present; reopen or recover migration")
		} else if !os.IsNotExist(e) {
			unlock()
			return nil, e
		}
		if _, e := os.Stat(filepath.Join(s.root, sqliteStoreName)); e == nil {
			unlock()
			return nil, fmt.Errorf("store backend changed; reopen Store")
		} else if !os.IsNotExist(e) {
			unlock()
			return nil, e
		}
	}
	if err = s.recoverRunPurgesLocked(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// ErrRunLockBusy identifies contention, never a lock-file access or I/O failure.
var ErrRunLockBusy = errors.New("run lock busy")

func (s *Store) AcquireRunLock(id string) (func(), error) {
	if id == "" || strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
		return nil, os.ErrNotExist
	}
	release, err := acquireFileLock(filepath.Join(s.root, id+".lock"), false)
	if err != nil {
		return nil, fmt.Errorf("acquire run lock %q: %w", id, err)
	}
	return release, nil
}
func (s *Store) updateLocked(id string, fn func(*Snapshot) error) (Snapshot, error) {
	snap, err := s.loadLocked(id)
	if err != nil {
		return Snapshot{}, err
	}
	if snap.Run.Deleted {
		return Snapshot{}, fmt.Errorf("run was deleted from archive")
	}
	if err := fn(&snap); err != nil {
		return Snapshot{}, err
	}
	assignMessageIDs(&snap)
	snap.Run.UpdatedAt = time.Now().UTC()
	refreshUsage(&snap)
	s.refreshDerivedLocked(&snap, map[string]bool{id: true})
	if err := s.archiveReports(&snap); err != nil {
		return Snapshot{}, err
	}
	if err := s.savePreparedLocked(snap); err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func addEvent(snap *Snapshot, typ, summary string, data any) {
	snap.Run.EventSeq++
	snap.Events = append(snap.Events, Event{Sequence: snap.Run.EventSeq, Type: typ, Summary: summary, Data: data, CreatedAt: time.Now().UTC()})
}
