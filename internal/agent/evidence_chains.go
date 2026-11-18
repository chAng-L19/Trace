package agent

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Summary-first disclosure follows Muteki EvidenceChain.tsx (144df766,
// AGPL-3.0); ordered source bindings follow ARTEX db/finding_traffic.go
// (e6ec5699, AGPL-3.0). Independent adaptation: immutable, run-qualified
// references over Trace's existing ledger, never a second execution engine.
type EvidenceChainStep struct {
	RunID       string `json:"run_id"`
	EvidenceID  string `json:"evidence_id"`
	ContentHash string `json:"content_hash"`
	Label       string `json:"label,omitempty"`
}

type EvidenceChain struct {
	ID            string                   `json:"id"`
	Name          string                   `json:"name"`
	Summary       string                   `json:"summary"`
	Steps         []EvidenceChainStep      `json:"steps"`
	ContentHash   string                   `json:"content_hash"`
	CreatedAt     time.Time                `json:"created_at"`
	MethodID      string                   `json:"method_id,omitempty"`
	Variant       string                   `json:"variant,omitempty"`
	Applicability string                   `json:"applicability,omitempty"`
	Lesson        string                   `json:"lesson,omitempty"`
	DerivedFrom   []EvidenceChainReference `json:"derived_from,omitempty"`
}

type EvidenceChainReference struct {
	SourceRunID string `json:"source_run_id"`
	ChainID     string `json:"chain_id"`
	ChainHash   string `json:"chain_hash"`
	Purpose     string `json:"purpose,omitempty"`
}

type EvidenceChainOperation struct {
	EvidenceChainStep
	Verification string    `json:"verification"`
	Error        string    `json:"error,omitempty"`
	Evidence     *Evidence `json:"evidence,omitempty"`
	Action       *Action   `json:"action,omitempty"`
}

type EvidenceChainEntry struct {
	Key             string                   `json:"key,omitempty"`
	Reference       bool                     `json:"reference,omitempty"`
	ExpectedHash    string                   `json:"expected_hash,omitempty"`
	Purpose         string                   `json:"purpose,omitempty"`
	SourceStatus    string                   `json:"source_status"`
	SourceReason    string                   `json:"source_reason,omitempty"`
	HistoryReadable bool                     `json:"history_readable"`
	RunID           string                   `json:"run_id"`
	RunGoal         string                   `json:"run_goal"`
	RunDeleted      bool                     `json:"run_deleted"`
	Chain           EvidenceChain            `json:"chain"`
	Verification    string                   `json:"verification"`
	Operations      []EvidenceChainOperation `json:"operations,omitempty"`
	ReferencedBy    []EvidenceChainUsage     `json:"referenced_by,omitempty"`
}

type EvidenceChainUsage struct {
	RunID      string `json:"run_id"`
	RunGoal    string `json:"run_goal"`
	RunDeleted bool   `json:"run_deleted"`
	ChainHash  string `json:"chain_hash"`
	Purpose    string `json:"purpose,omitempty"`
}

func (s *Store) chainUsagesLocked() (map[string][]EvidenceChainUsage, error) {
	ids, err := s.snapshotIDsLocked()
	if err != nil {
		return nil, err
	}
	usages := map[string][]EvidenceChainUsage{}
	for _, id := range ids {
		snap, err := s.loadRawLocked(id)
		if err != nil {
			return nil, err
		}
		for _, r := range snap.EvidenceChainRefs {
			key := r.SourceRunID + "/" + r.ChainID
			usages[key] = append(usages[key], EvidenceChainUsage{RunID: id, RunGoal: snap.Run.Goal, RunDeleted: snap.Run.Deleted, ChainHash: r.ChainHash, Purpose: r.Purpose})
		}
	}
	return usages, nil
}

func chainHash(c EvidenceChain) string {
	// Empty optional ancestry is omitted on disk. Its nil/empty in-memory
	// representation must not change the fixed version after a JSON round trip.
	if len(c.DerivedFrom) == 0 {
		c.DerivedFrom = nil
	}
	return valueHash(struct {
		Name                                     string
		Summary                                  string
		Steps                                    []EvidenceChainStep
		MethodID, Variant, Applicability, Lesson string
		DerivedFrom                              []EvidenceChainReference
	}{c.Name, c.Summary, c.Steps, c.MethodID, c.Variant, c.Applicability, c.Lesson, c.DerivedFrom})
}

func chainOperation(s *Snapshot, step EvidenceChainStep) EvidenceChainOperation {
	op := EvidenceChainOperation{EvidenceChainStep: step, Verification: "invalid", Error: "来源证据缺失"}
	for i := range s.Evidence {
		e := &s.Evidence[i]
		if e.ID != step.EvidenceID {
			continue
		}
		op.Evidence = e
		for j := range s.Actions {
			a := &s.Actions[j]
			if a.RequestID == e.RequestID && a.CallID == e.CallID {
				if op.Action != nil {
					op.Action = nil
					op.Error = "来源调用身份不唯一"
					return op
				}
				op.Action = a
			}
		}
		if e.ContentHash != step.ContentHash {
			op.Error = "来源证据内容已变化"
			return op
		}
		op.Verification = verifyEvidence(s, e)
		op.Error = ""
		if op.Verification == "invalid" {
			op.Error = "来源账本或产物已失效"
		}
		return op
	}
	return op
}

// Caller holds the Store lock. Load leaf ledgers directly: Run A may cite B
// while B cites A, but chains never contain other chains or recurse.
func (s *Store) chainEntryLocked(owner Snapshot, chain EvidenceChain, detail bool) EvidenceChainEntry {
	entry := EvidenceChainEntry{RunID: owner.Run.ID, RunGoal: owner.Run.Goal, RunDeleted: owner.Run.Deleted, Chain: chain, Verification: "verified", SourceStatus: "current", HistoryReadable: true}
	sources := map[string]*Snapshot{owner.Run.ID: &owner}
	if len(chain.Steps) == 0 || chainHash(chain) != chain.ContentHash {
		entry.Verification = "invalid"
		entry.SourceReason = "链内容与固定 hash 不一致或无来源操作"
	}
	for _, step := range chain.Steps {
		source, seen := sources[step.RunID]
		if !seen {
			loaded, err := s.loadRawLocked(step.RunID)
			if err == nil {
				source = &loaded
			}
			sources[step.RunID] = source
		}
		op := EvidenceChainOperation{EvidenceChainStep: step, Verification: "invalid", Error: "来源会话缺失"}
		if source != nil {
			op = chainOperation(source, step)
		}
		if op.Verification == "invalid" || entry.Verification == "invalid" {
			entry.Verification = "invalid"
			if entry.SourceReason == "" {
				entry.SourceReason = op.Error
			}
		} else if op.Verification != "verified" {
			entry.Verification = "unverified"
		}
		if detail {
			entry.Operations = append(entry.Operations, op)
		}
	}
	if parentStatus := s.chainParentStatusLocked(chain.DerivedFrom); parentStatus == "invalid" || (parentStatus == "unverified" && entry.Verification == "verified") {
		entry.Verification = parentStatus
		if entry.SourceReason == "" {
			entry.SourceReason = "固定父链版本或其来源已变化"
		}
	}
	if entry.Verification != "verified" {
		entry.SourceStatus = entry.Verification
	}
	if entry.Verification != "invalid" && strings.HasPrefix(chain.MethodID, "auto-") {
		for _, newer := range owner.EvidenceChains {
			if newer.MethodID == chain.MethodID && newer.CreatedAt.After(chain.CreatedAt) {
				entry.SourceStatus, entry.SourceReason = "superseded", "同一结论已有新的事实或引用版本；此历史链保留原始固定来源"
				break
			}
		}
	}
	return entry
}

func (s *Store) chainParentStatusLocked(refs []EvidenceChainReference) string {
	// Retain at most one successfully decoded source for adjacent references.
	// This cache lives only within this locked read; every step still verifies
	// its evidence and current CAS files. Alternating sources simply miss.
	var cached Snapshot
	loaded := false
	load := func(id string) (Snapshot, error) {
		if loaded && cached.Run.ID == id {
			return cached, nil
		}
		snap, err := s.loadRawLocked(id)
		if err == nil {
			cached, loaded = snap, true
		}
		return snap, err
	}
	status := "verified"
	queue := append([]EvidenceChainReference(nil), refs...)
	seen := map[string]bool{}
	for len(queue) > 0 {
		ref := queue[0]
		queue = queue[1:]
		key := ref.SourceRunID + "/" + ref.ChainID + "/" + ref.ChainHash
		if seen[key] {
			continue
		}
		seen[key] = true
		owner, err := load(ref.SourceRunID)
		if err != nil {
			return "invalid"
		}
		found := false
		for _, parent := range owner.EvidenceChains {
			if parent.ID != ref.ChainID {
				continue
			}
			found = true
			if parent.ContentHash != ref.ChainHash || chainHash(parent) != ref.ChainHash {
				return "invalid"
			}
			for _, step := range parent.Steps {
				source, err := load(step.RunID)
				if err != nil {
					return "invalid"
				}
				op := chainOperation(&source, step)
				if op.Verification == "invalid" {
					return "invalid"
				}
				if op.Verification != "verified" {
					status = "unverified"
				}
			}
			queue = append(queue, parent.DerivedFrom...)
		}
		if !found {
			return "invalid"
		}
	}
	return status
}

func (s *Store) chainLocked(runID, chainID string, detail bool) (EvidenceChainEntry, error) {
	owner, err := s.loadRawLocked(runID)
	if err != nil {
		return EvidenceChainEntry{}, err
	}
	for _, chain := range owner.EvidenceChains {
		if chain.ID == chainID {
			return s.chainEntryLocked(owner, chain, detail), nil
		}
	}
	return EvidenceChainEntry{}, os.ErrNotExist
}

func (s *Store) GetEvidenceChain(runID, chainID string) (EvidenceChainEntry, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return EvidenceChainEntry{}, err
	}
	defer unlock()
	entry, err := s.chainLocked(runID, chainID, true)
	if err != nil {
		return entry, err
	}
	usages, err := s.chainUsagesLocked()
	entry.ReferencedBy = usages[runID+"/"+chainID]
	return entry, err
}

func (s *Store) ListEvidenceChains() ([]EvidenceChainEntry, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	ids, err := s.snapshotIDsLocked()
	if err != nil {
		return nil, err
	}
	entries := []EvidenceChainEntry{}
	for _, id := range ids {
		owner, err := s.loadRawLocked(id)
		if err != nil {
			return nil, err
		}
		for _, chain := range owner.EvidenceChains {
			entries = append(entries, s.chainEntryLocked(owner, chain, false))
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Chain.CreatedAt.After(entries[j].Chain.CreatedAt) })
	return entries, nil
}

// ListRunEvidenceChains reads only this Run and its fixed chain references.
// Source/parent verification is identical to the global catalog; an unreadable
// referenced source remains missing so the UI can retain its invalid citation.
func (s *Store) ListRunEvidenceChains(runID string) ([]EvidenceChainEntry, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	owner, err := s.loadRawLocked(runID)
	if err != nil {
		return nil, err
	}
	entries := []EvidenceChainEntry{}
	seen := map[string]bool{}
	for _, chain := range owner.EvidenceChains {
		entries = append(entries, s.chainEntryLocked(owner, chain, false))
		seen[runID+"/"+chain.ID] = true
	}
	for _, ref := range owner.EvidenceChainRefs {
		key := ref.SourceRunID + "/" + ref.ChainID
		if seen[key] {
			continue
		}
		seen[key] = true
		entry, err := s.chainLocked(ref.SourceRunID, ref.ChainID, false)
		if err == nil {
			entries = append(entries, entry)
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Chain.CreatedAt.After(entries[j].Chain.CreatedAt) })
	return entries, nil
}

func (s *Store) CreateEvidenceChain(runID string, chain EvidenceChain) (EvidenceChainEntry, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return EvidenceChainEntry{}, err
	}
	defer unlock()
	chain.Name, chain.Summary = strings.TrimSpace(chain.Name), strings.TrimSpace(chain.Summary)
	if chain.Name == "" || chain.Summary == "" || len(chain.Steps) == 0 {
		return EvidenceChainEntry{}, fmt.Errorf("证据链需要名称、用途摘要和至少一个操作")
	}
	for _, parent := range chain.DerivedFrom {
		entry, err := s.chainLocked(parent.SourceRunID, parent.ChainID, false)
		if err != nil {
			return EvidenceChainEntry{}, err
		}
		if parent.ChainHash != entry.Chain.ContentHash || entry.Verification == "invalid" {
			return EvidenceChainEntry{}, fmt.Errorf("经验来源链路已失效或版本变化")
		}
		if chain.MethodID == "" {
			chain.MethodID = entry.Chain.MethodID
		}
	}
	if chain.MethodID == "" {
		chain.MethodID = "method-" + promptHash(chain.Name)
	}
	if len(chain.DerivedFrom) > 0 && (strings.TrimSpace(chain.Lesson) == "" || strings.TrimSpace(chain.Applicability) == "" || strings.TrimSpace(chain.Variant) == "") {
		return EvidenceChainEntry{}, fmt.Errorf("吸收经验需要变体名称、适用条件与本次经验")
	}
	seen := map[string]bool{}
	for i := range chain.Steps {
		step := &chain.Steps[i]
		if step.RunID == "" {
			step.RunID = runID
		}
		key := step.RunID + "/" + step.EvidenceID
		if seen[key] {
			return EvidenceChainEntry{}, fmt.Errorf("重复操作 %s", key)
		}
		seen[key] = true
		source, err := s.loadRawLocked(step.RunID)
		if err != nil {
			return EvidenceChainEntry{}, err
		}
		found := false
		for _, e := range source.Evidence {
			if e.ID == step.EvidenceID {
				if step.ContentHash != "" && step.ContentHash != e.ContentHash {
					return EvidenceChainEntry{}, fmt.Errorf("操作 %s 已变化，请刷新", key)
				}
				step.ContentHash = e.ContentHash
				found = true
				break
			}
		}
		if !found {
			return EvidenceChainEntry{}, fmt.Errorf("操作 %s 缺失", key)
		}
	}
	chain.ContentHash, chain.CreatedAt = chainHash(chain), time.Now().UTC()
	chain.ID = "chain-" + chain.ContentHash
	owner, err := s.updateLocked(runID, func(snap *Snapshot) error {
		for _, c := range snap.EvidenceChains {
			if c.ID == chain.ID {
				return nil
			}
		}
		snap.EvidenceChains = append(snap.EvidenceChains, chain)
		addEvent(snap, "evidence_chain_created", "已整理可复用操作证据链", map[string]any{"chain_id": chain.ID, "steps": len(chain.Steps)})
		return nil
	})
	if err != nil {
		return EvidenceChainEntry{}, err
	}
	for _, c := range owner.EvidenceChains {
		if c.ID == chain.ID {
			chain = c
			break
		}
	}
	return s.chainEntryLocked(owner, chain, true), nil
}

func (s *Store) ReferenceEvidenceChain(runID string, ref EvidenceChainReference) (Snapshot, error) {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	entry, err := s.chainLocked(ref.SourceRunID, ref.ChainID, false)
	if err != nil {
		return Snapshot{}, err
	}
	if ref.ChainHash == "" || ref.ChainHash != entry.Chain.ContentHash || entry.Verification == "invalid" {
		return Snapshot{}, fmt.Errorf("证据链已变化或来源失效，请核对后重新引用")
	}
	return s.updateLocked(runID, func(snap *Snapshot) error {
		for i, r := range snap.EvidenceChainRefs {
			if r.SourceRunID == ref.SourceRunID && r.ChainID == ref.ChainID && r.ChainHash == ref.ChainHash {
				if r.Purpose != ref.Purpose {
					snap.EvidenceChainRefs[i] = ref
					addEvent(snap, "evidence_chain_reference_updated", "已更新整链引用用途", ref)
				}
				return nil
			}
		}
		snap.EvidenceChainRefs = append(snap.EvidenceChainRefs, ref)
		addEvent(snap, "evidence_chain_referenced", "已引用操作证据链，未重新执行", ref)
		return nil
	})
}
