package agent

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// An explicit operator review resolves the ownership hold, never the original
// execution evidence or process cleanup. Its event pins the original visit.
type executorOwnershipReview struct {
	ChangedEventSequence int      `json:"changed_event_sequence"`
	EffectsReconciled    bool     `json:"effects_reconciled"`
	Observation          string   `json:"observation"`
	EvidenceIDs          []string `json:"evidence_ids,omitempty"`
}

func (a *Agent) recordExecutorOwnershipReviewLocked(s *Snapshot, requestID, callID, content string) (bool, error) {
	var envelope map[string]json.RawMessage
	if json.Unmarshal([]byte(content), &envelope) != nil {
		return false, nil
	}
	raw, present := envelope["executor_ownership_review"]
	if !present {
		return false, nil
	}
	if len(envelope) != 1 {
		return true, fmt.Errorf("ownership review must be a single explicit envelope")
	}
	var review executorOwnershipReview
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&review); err != nil {
		return true, fmt.Errorf("ownership review: %w", err)
	}
	if !review.EffectsReconciled || review.ChangedEventSequence <= 0 || strings.TrimSpace(review.Observation) == "" {
		return true, fmt.Errorf("ownership review requires the original changed event, explicit reconciled effects and actual observations")
	}
	var action *Action
	for i := range s.Actions {
		candidate := &s.Actions[i]
		if candidate.RequestID == requestID && candidate.CallID == callID {
			if action != nil {
				return true, fmt.Errorf("ownership review action identity is ambiguous")
			}
			action = candidate
		}
	}
	if action == nil || action.ExecutorOwnershipError == "" || action.Result == nil || action.Status == "started" {
		return true, fmt.Errorf("ownership review needs the original completed observation and unresolved hold")
	}
	type ownershipEvent struct {
		RequestID string          `json:"request_id"`
		CallID    string          `json:"call_id"`
		Owners    []executorOwner `json:"owners"`
	}
	var original []executorOwner
	changed := false
	for _, event := range s.Events {
		if event.Type != "action_executor_admitted" && event.Type != "action_executor_changed" {
			continue
		}
		data, err := json.Marshal(event.Data)
		var recorded ownershipEvent
		if err != nil || json.Unmarshal(data, &recorded) != nil || recorded.RequestID != requestID || recorded.CallID != callID {
			continue
		}
		if event.Type == "action_executor_admitted" && event.Sequence < review.ChangedEventSequence {
			if original != nil {
				return true, fmt.Errorf("ownership review admission identity is ambiguous")
			}
			original = recorded.Owners
		}
		if event.Type == "action_executor_changed" && event.Sequence == review.ChangedEventSequence {
			changed = len(original) > 0 && original[0].RunID == s.Run.ID && slices.Equal(original, recorded.Owners)
		}
	}
	if !changed {
		return true, fmt.Errorf("ownership review does not match the original admitted worker visits")
	}
	var references []EvidenceReference
	if len(review.EvidenceIDs) > 0 {
		var err error
		references, err = evidenceReferences(s, review.EvidenceIDs)
		if err != nil {
			return true, err
		}
	}
	oldHold := action.ExecutorOwnershipError
	resultHash := valueHash(action.Result)
	action.ExecutorOwnershipError = ""
	if action.ResourceCleanupError == oldHold {
		action.ResourceCleanupError = ""
	}
	action.Status = "resolved_manually"
	addEvent(s, "action_executor_reviewed", "原执行访次的外部效果已人工核对；原始观察保持未知，不重放", map[string]any{
		"request_id": requestID, "call_id": callID, "changed_event_sequence": review.ChangedEventSequence,
		"owners": original, "result_hash": resultHash, "observation": review.Observation, "references": references,
	})
	if _, pending := pendingToolCalls(s)[callID]; pending && actionRequestMatchesCurrent(s, *action) {
		appendToolResult(s, callID, content, "manual")
	}
	a.refreshWorkerCleanupReceiptLocked(s)
	return true, nil
}
