package agent

import (
	"encoding/json"
	"fmt"

	"trace/internal/worker"
)

// A historical intent is never permission to replay. Observations close the
// model's pending call without inventing execution or cleanup receipts.
func appendHistoricalToolObservation(s *Snapshot, call ToolCall, detail string) (bool, error) {
	matched := -1
	ambiguous := false
	for i, action := range s.Actions {
		if action.CallID != call.ID || !actionRequestMayMatchCurrent(s, action) {
			continue
		}
		if matched != -1 || !actionRequestMatchesCurrent(s, action) {
			ambiguous = true
		}
		matched = i
	}
	if matched == -1 {
		return false, nil
	}
	result := ToolResult{ExitCode: -1, OutcomeUncertain: true,
		Error: "Historical tool outcome is unknown; this call was not replayed. Inspect existing side effects and choose a new call to continue."}
	if ambiguous {
		result.Error = "Historical tool batch ownership is ambiguous; this call was not replayed and no historical result was consumed. Inspect existing side effects and choose a new call to continue."
	} else {
		action := &s.Actions[matched]
		if action.Status == "started" && action.Result == nil {
			action.Status = "uncertain"
			addEvent(s, "action_uncertain", "历史动作不重放，结果交回模型核对", map[string]string{"call_id": call.ID, "request_id": action.RequestID})
		}
		if action.Name == call.Name && action.Arguments == call.Arguments && action.Result != nil && verifyArtifacts(s.artifactRoot, action.Result) {
			observation := *action.Result
			if detail != "" {
				observation.OutcomeUncertain = true
				observation.Error += " Historical call was not replayed. " + detail
			}
			raw, err := json.Marshal(observation)
			if err != nil {
				return false, err
			}
			appendToolResult(s, call.ID, string(raw), "runtime_observation")
			return true, nil
		}
		if action.Result != nil {
			result.WorkerCleanupError = action.Result.WorkerCleanupError
		}
	}
	if detail != "" {
		result.Error += " " + detail
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return false, err
	}
	appendToolResult(s, call.ID, string(raw), "runtime_observation")
	return true, nil
}

func (a *Agent) recordUnavailableTool(id, callID, name string) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		if isStopped(s.Run.Status) {
			return nil
		}
		call, ok := pendingToolCalls(s)[callID]
		if !ok || call.Name != name {
			return fmt.Errorf("unknown pending tool call: %s", callID)
		}
		tool, _ := a.registry.Lookup(call.Name)
		if observed, err := a.toolAdmissionObservation(s, call, tool.Adapter); observed || err != nil {
			return err
		}
		raw, err := json.Marshal(ToolResult{ExitCode: -1, WorkerStatus: worker.StatusNotStarted,
			Error: fmt.Sprintf("Tool %q is unavailable and was not executed. Select an available tool and continue with a new call.", name)})
		if err != nil {
			return err
		}
		appendToolResult(s, callID, string(raw), "runtime_observation")
		return nil
	})
}
