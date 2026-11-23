package agent

import (
	"encoding/json"
	"fmt"

	"trace/internal/worker"
)

// Only reviewed builtins that coordinate work or consume existing ledgers may
// run in a coordinator. New tools/adapters default to execution-only; names or
// read-only hints from external registrations cannot grant this capability.
// task_claim/task_complete are worker execution ownership operations. Evidence
// checks/reviews inspect saved receipts (reviews have no tools); finding_retest
// only associates an already completed probe and does not start one.
func (r *ToolRegistry) permitsRunTool(run Run, name, adapter string) bool {
	if run.Responsibility == "" || run.Responsibility == ResponsibilityWorker {
		return true
	}
	if run.Responsibility != ResponsibilityCoordinator || adapter != "builtin" {
		return false
	}
	tool, ok := r.Lookup(name)
	if !ok || tool.Adapter != "builtin" {
		return false
	}
	switch name {
	case "tool_search", "tool_select", "search_sources", "skills_list",
		"artifact_read", "session_read",
		"plan", "task_dispose", "task_retry",
		"worker_dispatch", "worker_status", "worker_history", "worker_wait",
		"worker_message", "worker_resume", "worker_cancel", "worker_recover",
		"assets", "asset_upsert", "asset_relate",
		"verification_status", "request_input", "evidence_check",
		"finding_submit", "finding_review", "finding_retest",
		"evidence_chain_create", "evidence_chain_read", "evidence_chain_reference", "evidence_chain_search":
		return true
	}
	return false
}

func (a *Agent) runToolDefinitions(run Run, definitions []ToolDefinition) []ToolDefinition {
	out := make([]ToolDefinition, 0, len(definitions))
	for _, d := range definitions {
		tool, _ := a.registry.Lookup(d.Function.Name)
		if a.registry.permitsRunTool(run, d.Function.Name, tool.Adapter) {
			out = append(out, d)
		}
	}
	return out
}

// Admission shares the existing intent/result ledger. Historical intents take
// precedence over role policy and must never become new attempts or denials.
func (a *Agent) toolAdmissionObservation(s *Snapshot, call ToolCall, adapter string) (bool, error) {
	if observed, err := appendHistoricalToolObservation(s, call, ""); observed || err != nil {
		return observed, err
	}
	if a.registry.permitsRunTool(s.Run, call.Name, adapter) {
		return false, nil
	}
	result := ToolResult{ExitCode: -1, WorkerStatus: worker.StatusNotStarted,
		Error:      fmt.Sprintf("Run responsibility %q does not permit %q via %q; this call was not executed. Delegate concrete work with plan and worker_dispatch. If dispatch fails, report its reason; the coordinator must not take over execution.", s.Run.Responsibility, call.Name, adapter),
		Diagnostic: &ToolDiagnostic{Code: "run_role_restricted", Summary: "The run may coordinate work but may not execute this tool.", NextAction: "Dispatch a Worker for concrete work, or report the dispatch blocker."}}
	raw, err := json.Marshal(result)
	if err != nil {
		return false, err
	}
	appendToolResult(s, call.ID, string(raw), "runtime_observation")
	return true, nil
}

// Check before argument validation and adapter setup as well as at the final
// action boundary. Allowed fresh calls need no extra Store write.
func (a *Agent) observePendingToolAdmission(snap Snapshot, call ToolCall, adapter string) (Snapshot, bool, error) {
	needed := !a.registry.permitsRunTool(snap.Run, call.Name, adapter) || hasHistoricalToolIntent(&snap, call)
	if !needed {
		return snap, false, nil
	}
	observed := false
	snap, err := a.store.Update(snap.Run.ID, func(s *Snapshot) error {
		if isStopped(s.Run.Status) {
			observed = true
			return nil
		}
		pending, ok := pendingToolCalls(s)[call.ID]
		if !ok || pending.Name != call.Name || pending.Arguments != call.Arguments {
			return fmt.Errorf("unknown pending tool call: %s", call.ID)
		}
		var err error
		observed, err = a.toolAdmissionObservation(s, pending, adapter)
		return err
	})
	return snap, observed, err
}

func hasHistoricalToolIntent(s *Snapshot, call ToolCall) bool {
	for _, action := range s.Actions {
		if action.CallID == call.ID && actionRequestMayMatchCurrent(s, action) {
			return true
		}
	}
	return false
}
