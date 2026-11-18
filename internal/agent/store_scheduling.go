package agent

import (
	"strings"
	"sync"
)

type schedulingSubscription struct {
	through int
	readers map[chan struct{}]struct{}
}

// A coalesced hint, never a second task queue. Callers subscribe before Observe
// and retain polling for commits made by a different process.
func (s *Store) SubscribeScheduling(id string) (<-chan struct{}, func()) {
	s.eventViewMu.Lock()
	if s.scheduling == nil {
		s.scheduling = map[string]*schedulingSubscription{}
	}
	entry := s.scheduling[id]
	created := entry == nil
	if entry == nil {
		entry = &schedulingSubscription{readers: map[chan struct{}]struct{}{}}
		s.scheduling[id] = entry
	}
	changed := make(chan struct{}, 1)
	entry.readers[changed] = struct{}{}
	s.eventViewMu.Unlock()
	var once sync.Once
	// Register first, then establish the durable cursor. A concurrent commit
	// may coalesce one harmless hint, but old events cannot wake every token.
	if observation, err := s.Observe(id); created && err == nil {
		s.eventViewMu.Lock()
		if observation.Run.EventSeq > entry.through {
			entry.through = observation.Run.EventSeq
		}
		s.eventViewMu.Unlock()
	}
	return changed, func() {
		once.Do(func() {
			s.eventViewMu.Lock()
			defer s.eventViewMu.Unlock()
			delete(entry.readers, changed)
			if len(entry.readers) == 0 {
				delete(s.scheduling, id)
			}
		})
	}
}

func schedulingEvent(kind string) bool {
	if strings.HasPrefix(kind, "worker_message") {
		return true
	}
	switch kind {
	case "worker_started", "worker_resumed", "worker_finished", "worker_cancel_requested", "worker_lease_expired", "worker_yielded", "worker_interrupted", "worker_capacity_updated",
		"task_claimed", "task_completed", "task_disposed", "task_retry_requested", "task_blocked_capability", "task_capability_restored", "local_task_resumed",
		"plan_updated", "plan_failed", "run_paused", "run_cancelled", "run_failed", "run_completed",
		"budget_paused", "budget_updated", "tree_budget_updated", "checkpoint_requested", "budget_checkpoint_requested", "message_queued", "message_added", "message_consumed", "message_cancelled", "message_edited", "mailbox_reordered":
		return true
	}
	return false
}

func (s *Store) notifySchedulingCommit(id string, events []Event, through int) {
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	entry := s.scheduling[id]
	if entry == nil {
		return
	}
	wake := false
	for i := len(events) - 1; i >= 0; i-- {
		event := events[i]
		if event.Sequence > 0 && event.Sequence <= entry.through {
			break
		}
		if schedulingEvent(event.Type) {
			wake = true
		}
	}
	entry.through = through
	if wake {
		for changed := range entry.readers {
			select {
			case changed <- struct{}{}:
			default:
			}
		}
	}
}
