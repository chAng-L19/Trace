package agent

import (
	"fmt"
	"os"
	"strings"
)

// Operation identity is scoped to the parent Run, across all participants.
// The receipt lives in the existing durable inbox, including after delivery.
func (a *Agent) SendWorkerMessageOperation(id, from, to, content, kind, replyTo, operationID string) (Snapshot, WorkerMessage, bool, error) {
	if strings.TrimSpace(content) == "" {
		return Snapshot{}, WorkerMessage{}, false, fmt.Errorf("message required")
	}
	if len(operationID) > 200 || strings.ContainsAny(operationID, "/\\\r\n") {
		return Snapshot{}, WorkerMessage{}, false, fmt.Errorf("invalid operation_id")
	}
	hash := valueHash(struct{ From, To, Content, Kind, ReplyTo string }{from, to, content, kind, replyTo})
	var receipt WorkerMessage
	created := false
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		for _, m := range s.WorkerMessages {
			if operationID == "" || m.OperationID != operationID {
				continue
			}
			if m.RequestHash != hash {
				return fmt.Errorf("operation_id already belongs to different worker input")
			}
			receipt = m
			return nil
		}
		if err := appendRelatedWorkerMessage(s, from, to, content, kind, replyTo); err != nil {
			return err
		}
		m := &s.WorkerMessages[len(s.WorkerMessages)-1]
		if operationID != "" {
			m.OperationID, m.RequestHash = operationID, hash
			// Keep the committed event and inbox receipt identical.
			s.Events[len(s.Events)-1].Data = *m
		}
		receipt, created = *m, true
		return nil
	})
	return snap, receipt, created, err
}

func (s *Store) WorkerMessageOperation(id, operationID string) (WorkerMessage, error) {
	snap, err := s.Observe(id)
	if err != nil {
		return WorkerMessage{}, err
	}
	if !snap.Run.Deleted && operationID != "" {
		for _, m := range snap.WorkerMessages {
			if m.OperationID == operationID {
				return m, nil
			}
		}
	}
	return WorkerMessage{}, os.ErrNotExist
}
