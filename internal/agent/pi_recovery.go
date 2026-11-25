package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"trace/internal/worker"
)

var errPiRecoveryArchiveMissing = errors.New("pending tool batch has no archived request")

type piRecovery struct {
	Response ModelResponse
	Tools    []ToolDefinition
	Settled  map[string]bool
}

// Missing schemas cannot authorize old execution. Close only the model-facing
// batch with diagnostic observations, leaving original actions and cleanup intact.
func (a *Agent) observeMissingPiArchive(id string) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		if isStopped(s.Run.Status) {
			return errPiHalt
		}
		requestID := currentRequestID(s)
		for _, request := range s.Requests {
			if request.ID == requestID {
				return fmt.Errorf("request archive changed during recovery: %s", requestID)
			}
		}
		pending := pendingToolCalls(s)
		for _, call := range latestPiCalls(s) {
			if _, ok := pending[call.ID]; !ok {
				continue
			}
			if observed, err := appendHistoricalToolObservation(s, call, errPiRecoveryArchiveMissing.Error()); observed || err != nil {
				if err != nil {
					return err
				}
				continue
			}
			raw, err := json.Marshal(ToolResult{ExitCode: -1, WorkerStatus: worker.StatusNotStarted, Error: "Original request archive is missing; this pending call was not executed. Select available tools and continue with a new call."})
			if err != nil {
				return err
			}
			appendToolResult(s, call.ID, string(raw), "runtime_observation")
		}
		addEvent(s, "pi_recovery_observed", "缺少历史请求归档；诊断已交回模型，原调用未执行或重放", nil)
		return nil
	})
}

// Recovery uses agentloop's strict argument validation and ordered settlement.
// Do not archive a new request before settling the original batch: action and
// evidence identities must continue to belong to its durable request.
func (a *Agent) preparePiRecovery(id string) (piRecovery, bool, error) {
	s, err := a.store.Load(id)
	if err != nil {
		return piRecovery{}, false, err
	}
	if isStopped(s.Run.Status) {
		return piRecovery{}, false, errPiHalt
	}
	pending := pendingToolCalls(&s)
	if len(pending) == 0 {
		return piRecovery{}, false, nil
	}
	recovery := piRecovery{Settled: map[string]bool{}}
	pendingCount := 0
	requestID := currentRequestID(&s)
	for i := len(s.Messages) - 1; i >= 0; i-- {
		m := s.Messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			continue
		}
		if m.RequestID != "" {
			requestID = m.RequestID
		}
		for _, call := range m.ToolCalls {
			recovery.Response.ToolCalls = append(recovery.Response.ToolCalls, call)
			if _, ok := pending[call.ID]; ok {
				pendingCount++
			} else {
				recovery.Settled[call.ID] = true
			}
		}
		break
	}
	if pendingCount != len(pending) {
		return piRecovery{}, false, fmt.Errorf("pending calls do not belong to the current assistant batch")
	}
	if requestID != currentRequestID(&s) {
		return piRecovery{}, false, fmt.Errorf("pending batch request differs from current durable request")
	}
	for _, response := range s.Responses {
		if response.RequestID == requestID {
			recovery.Response.StopReason = response.Response.StopReason
			break
		}
	}
	for _, request := range s.Requests {
		if request.ID == requestID {
			recovery.Tools = request.Tools
			return recovery, true, nil
		}
	}
	// A current catalog cannot prove the schemas of a historical batch.
	// Legacy/manual calls remain resolvable through explicit tool submission.
	return piRecovery{}, false, fmt.Errorf("%w: %s", errPiRecoveryArchiveMissing, requestID)
}
