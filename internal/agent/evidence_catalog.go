package agent

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EvidenceCatalogEntry keeps original run identity after conversation removal.
// Organization follows ARTEX's retained global findings (db/findings.go,
// e6ec5699, AGPL-3.0); this is an independent Go adaptation. The
// existing Trace snapshot remains the authoritative execution/provenance ledger.
type EvidenceCatalogEntry struct {
	RunID       string               `json:"run_id"`
	RunGoal     string               `json:"run_goal"`
	RunDeleted  bool                 `json:"run_deleted"`
	Evidence    Evidence             `json:"evidence"`
	ReportLinks []ReportEvidenceLink `json:"report_links"`
	Action      *Action              `json:"action,omitempty"`
	CreatedAt   time.Time            `json:"created_at"`
}

func catalogEntry(s Snapshot, evidence Evidence) EvidenceCatalogEntry {
	created := s.Run.CreatedAt
	var action *Action
	for _, event := range s.Events {
		if event.Sequence == evidence.SourceEventSequence {
			created = event.CreatedAt
			break
		}
	}
	for i := range s.Actions {
		if s.Actions[i].RequestID == evidence.RequestID && s.Actions[i].CallID == evidence.CallID {
			action = &s.Actions[i]
			break
		}
	}
	return catalogEntryWithSources(s, evidence, created, action)
}

func catalogEntryWithSources(s Snapshot, evidence Evidence, created time.Time, action *Action) EvidenceCatalogEntry {
	return EvidenceCatalogEntry{RunID: s.Run.ID, RunGoal: s.Run.Goal, RunDeleted: s.Run.Deleted, Evidence: evidence, CreatedAt: created, Action: action, ReportLinks: reportEvidenceLinks(&s, evidence.ID)}
}

// Index only within this already verified snapshot. Duplicate identities retain
// the first match, exactly as the single-evidence detail path does.
func catalogEntries(s Snapshot) []EvidenceCatalogEntry {
	if len(s.Evidence) == 0 {
		return nil
	}
	// Small catalogs cost less to scan than to allocate and populate indexes.
	if len(s.Evidence) <= 16 {
		entries := make([]EvidenceCatalogEntry, 0, len(s.Evidence))
		for _, evidence := range s.Evidence {
			entries = append(entries, catalogEntry(s, evidence))
		}
		return entries
	}
	eventTimes := make(map[int]time.Time, len(s.Events))
	for _, event := range s.Events {
		if _, exists := eventTimes[event.Sequence]; !exists {
			eventTimes[event.Sequence] = event.CreatedAt
		}
	}
	actions := make(map[[2]string]*Action, len(s.Actions))
	for i := range s.Actions {
		action := &s.Actions[i]
		key := [2]string{action.RequestID, action.CallID}
		if _, exists := actions[key]; !exists {
			actions[key] = action
		}
	}
	entries := make([]EvidenceCatalogEntry, 0, len(s.Evidence))
	for _, evidence := range s.Evidence {
		created, exists := eventTimes[evidence.SourceEventSequence]
		if !exists {
			created = s.Run.CreatedAt
		}
		entries = append(entries, catalogEntryWithSources(s, evidence, created, actions[[2]string{evidence.RequestID, evidence.CallID}]))
	}
	return entries
}

func (s *Store) snapshotIDsLocked() ([]string, error) {
	if s.sqlite {
		return s.sqliteIDs()
	}
	entries, err := os.ReadDir(s.root)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			ids = append(ids, strings.TrimSuffix(entry.Name(), ".json"))
		}
	}
	return ids, nil
}

func (s *Store) ListEvidence() ([]EvidenceCatalogEntry, error) {
	return s.listEvidence("")
}

func (s *Store) ListRunEvidence(runID string) ([]EvidenceCatalogEntry, error) {
	if runID == "" {
		return nil, os.ErrNotExist
	}
	return s.listEvidence(runID)
}

func (s *Store) listEvidence(runID string) ([]EvidenceCatalogEntry, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	ids := []string{runID}
	if runID == "" {
		ids, err = s.snapshotIDsLocked()
		if err != nil {
			return nil, err
		}
	}
	entries := []EvidenceCatalogEntry{}
	for _, id := range ids {
		snap, err := s.loadLocked(id)
		if err != nil {
			return nil, err
		}
		entries = append(entries, catalogEntries(snap)...)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].RunID+entries[i].Evidence.ID < entries[j].RunID+entries[j].Evidence.ID
		}
		return entries[i].CreatedAt.After(entries[j].CreatedAt)
	})
	return entries, nil
}

func (s *Store) GetEvidence(runID, evidenceID string) (EvidenceCatalogEntry, error) {
	snap, err := s.Load(runID)
	if err != nil {
		return EvidenceCatalogEntry{}, err
	}
	for _, evidence := range snap.Evidence {
		if evidence.ID == evidenceID {
			return catalogEntry(snap, evidence), nil
		}
	}
	return EvidenceCatalogEntry{}, os.ErrNotExist
}
