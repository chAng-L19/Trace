package agent

import (
	"encoding/json"
	"fmt"
)

type piDeferredResultKey struct{}

// Explicitly pure builtin adapters only. A generic MCP/local/exec declaration
// is not proof of read-only behavior. Mixed batches stay sequential in pi.
func (a *Agent) piReadOnlyTool(name string) bool {
	tool, ok := a.registry.Lookup(name)
	if !ok || tool.Adapter != "builtin" {
		return false
	}
	switch name {
	case "read", "artifact_read", "tool_search", "session_read", "skills_list", "read_skill":
		return true
	}
	return false
}

func (a *Agent) piParallelTools(definitions []ToolDefinition) []string {
	names := []string{}
	for _, d := range definitions {
		if a.piReadOnlyTool(d.Function.Name) {
			names = append(names, d.Function.Name)
		}
	}
	return names
}

func (a *Agent) piReadOnlyBatch(calls []ToolCall) bool {
	if len(calls) < 2 {
		return false
	}
	for _, call := range calls {
		if !a.piReadOnlyTool(call.Name) {
			return false
		}
	}
	return true
}

func readyPiResult(s *Snapshot, callID string) (string, bool, error) {
	if _, pending := pendingToolCalls(s)[callID]; !pending {
		return "", false, nil
	}
	for i := range s.Actions {
		action := &s.Actions[i]
		if actionRequestMatchesCurrent(s, *action) && action.CallID == callID && piResultAwaitingEmission(*action) {
			if action.Result == nil {
				return "", false, fmt.Errorf("ready action missing result: %s", callID)
			}
			raw, err := json.Marshal(action.Result)
			return string(raw), true, err
		}
	}
	return "", false, nil
}

func piResultAwaitingEmission(action Action) bool {
	return action.Status == "result_ready" || action.Result != nil && (action.Status == "uncertain" || action.Status == "interrupted")
}

func validateReadyPiAction(s *Snapshot, call ToolCall) error {
	for _, action := range s.Actions {
		if actionRequestMatchesCurrent(s, action) && action.CallID == call.ID && piResultAwaitingEmission(action) {
			if action.Name != call.Name || action.Arguments != call.Arguments {
				return fmt.Errorf("ready action identity mismatch: %s", call.ID)
			}
			if action.Result == nil {
				return fmt.Errorf("ready action missing result: %s", call.ID)
			}
			if !verifyArtifacts(s.artifactRoot, action.Result) {
				return fmt.Errorf("ready action artifact verification failed: %s", call.ID)
			}
			return nil
		}
	}
	return fmt.Errorf("ready action not found: %s", call.ID)
}

// Pi emits results in declaration order, including parallel batches. A crash
// between execution and emission resumes the saved result, never the action.
func settleReadyPiResult(s *Snapshot, callID string) (bool, error) {
	if _, pending := pendingToolCalls(s)[callID]; !pending {
		return false, nil
	}
	for i := range s.Actions {
		action := &s.Actions[i]
		if !actionRequestMatchesCurrent(s, *action) || action.CallID != callID || !piResultAwaitingEmission(*action) {
			continue
		}
		if action.Result == nil {
			return false, fmt.Errorf("ready action missing result: %s", callID)
		}
		for _, call := range latestPiCalls(s) {
			if call.ID == callID {
				break
			}
			if _, pending := pendingToolCalls(s)[call.ID]; pending {
				return false, fmt.Errorf("pi result out of declaration order: %s", callID)
			}
		}
		source := "runtime_observation"
		if action.Status == "result_ready" {
			action.Status = "completed"
			source = "executed"
			action.Result.EvidenceID = fmt.Sprintf("ev-%d", s.Run.EventSeq+1)
		}
		raw, err := json.Marshal(action.Result)
		if err != nil {
			return false, err
		}
		appendToolResult(s, callID, string(raw), source)
		return true, nil
	}
	return false, nil
}

func latestPiCalls(s *Snapshot) []ToolCall {
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role == "assistant" && len(s.Messages[i].ToolCalls) > 0 {
			return s.Messages[i].ToolCalls
		}
	}
	return nil
}
