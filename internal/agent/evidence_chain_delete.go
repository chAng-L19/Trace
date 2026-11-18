package agent

import (
	"fmt"
	"os"
	"time"
)

// Removing an organization record never deletes an operation or a CAS blob.
func (s *Store) DeleteEvidenceChain(runID, chainID string) error {
	release, err := s.AcquireRunLock(runID)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return err
	}
	defer unlock()
	owner, err := s.loadRawLocked(runID)
	if err != nil {
		return err
	}
	if owner.Run.Status == StatusRunning || owner.Run.Status == StatusWaitingTool {
		return fmt.Errorf("请先暂停会话，再删除证据链")
	}
	ids, err := s.snapshotIDsLocked()
	if err != nil {
		return err
	}
	for _, id := range ids {
		snap, err := s.loadRawLocked(id)
		if err != nil {
			return err
		}
		for _, ref := range snap.EvidenceChainRefs {
			if ref.SourceRunID == runID && ref.ChainID == chainID {
				return fmt.Errorf("证据链仍被会话 %s 引用，请先取消引用", id)
			}
		}
		for _, chain := range snap.EvidenceChains {
			for _, ref := range chain.DerivedFrom {
				if ref.SourceRunID == runID && ref.ChainID == chainID {
					return fmt.Errorf("证据链仍是方法变体 %s 的经验来源", chain.Name)
				}
			}
		}
	}
	for i, chain := range owner.EvidenceChains {
		if chain.ID != chainID {
			continue
		}
		owner.EvidenceChains = append(owner.EvidenceChains[:i], owner.EvidenceChains[i+1:]...)
		owner.Run.UpdatedAt = time.Now().UTC()
		addEvent(&owner, "evidence_chain_removed", "已移除证据链，原始操作保留", map[string]string{"chain_id": chainID})
		return s.saveLocked(owner)
	}
	return os.ErrNotExist
}

func (s *Store) UnreferenceEvidenceChain(runID string, ref EvidenceChainReference) error {
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return err
	}
	defer unlock()
	owner, err := s.loadRawLocked(runID)
	if err != nil {
		return err
	}
	for i, r := range owner.EvidenceChainRefs {
		if r.SourceRunID != ref.SourceRunID || r.ChainID != ref.ChainID || r.ChainHash != ref.ChainHash {
			continue
		}
		owner.EvidenceChainRefs = append(owner.EvidenceChainRefs[:i], owner.EvidenceChainRefs[i+1:]...)
		owner.Run.UpdatedAt = time.Now().UTC()
		addEvent(&owner, "evidence_chain_unreferenced", "已取消整链引用，来源链路保留", ref)
		return s.saveLocked(owner)
	}
	return os.ErrNotExist
}
