package agent

import (
	"crypto/sha256"
	"fmt"
	"time"
)

// ARTEX e6ec5699 db/tasks.go:433 keeps control writes separate from history.
// Trace still performs its full source verification, usage and report refresh
// under the original cross-process Store lock. Only an unchanged history may
// reuse its SQLite state; the returned Snapshot remains complete.
func (s *Store) updatePause(id string, fn func(*Snapshot) error) (Snapshot, error) {
	if !s.sqlite {
		return s.Update(id, fn)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer unlock()
	snap, err := s.loadRawLocked(id)
	if err != nil {
		return Snapshot{}, err
	}
	before, err := pauseStateHash(snap)
	if err != nil {
		return Snapshot{}, err
	}
	// Preserve verified Load's commit-before-callback boundary. A legacy run
	// header may already need only its derived Result normalized; routing that
	// preliminary commit through full save would rewrite history before the
	// control-only path was ever reached. Child sources retain their usual gates.
	if s.refreshDerivedLocked(&snap, map[string]bool{id: true}) {
		snap.Run.UpdatedAt = time.Now().UTC()
		if err = s.archiveReports(&snap); err != nil {
			return Snapshot{}, err
		}
		if before, err = s.savePausePreparedLocked(snap, before); err != nil {
			return Snapshot{}, err
		}
	}
	if snap.Run.Deleted {
		return Snapshot{}, fmt.Errorf("run was deleted from archive")
	}
	if err = fn(&snap); err != nil {
		return Snapshot{}, err
	}
	assignMessageIDs(&snap)
	snap.Run.UpdatedAt = time.Now().UTC()
	refreshUsage(&snap)
	s.refreshDerivedLocked(&snap, map[string]bool{id: true})
	if err = s.archiveReports(&snap); err != nil {
		return Snapshot{}, err
	}
	_, err = s.savePausePreparedLocked(snap, before)
	if err != nil {
		return Snapshot{}, err
	}
	return snap, nil
}

func (s *Store) savePausePreparedLocked(snap Snapshot, before [32]byte) ([32]byte, error) {
	after, err := pauseStateHash(snap)
	if err != nil {
		return after, err
	}
	if before == after {
		err = s.sqliteSaveControl(snap)
	} else {
		err = s.savePreparedLocked(snap)
	}
	return after, err
}

func pauseStateHash(snap Snapshot) ([32]byte, error) {
	snap.Run, snap.Events, snap.Result = Run{}, nil, Result{}
	hash := sha256.New()
	err := encodeSnapshot(hash, snap)
	var sum [32]byte
	copy(sum[:], hash.Sum(nil))
	return sum, err
}
