package agent

// Muteki coordinator_recovery.py (AGPL-3.0, c60c8fee) supplies the
// no-new-facts/replan distinction. Trace deduplicates unchanged corrective
// prompts without turning stagnation into an implicit execution budget.
import (
	"encoding/json"
	"sort"
	"strings"
	"time"
)

func completionProgressFingerprint(s *Snapshot) string {
	set := map[string]bool{}
	for _, action := range s.Actions {
		if action.Result == nil || action.Status == "started" || action.Status == "result_ready" {
			continue
		}
		if !observationAction(action) && action.Name != "write" && action.Name != "edit" && action.Name != "artifact_register" {
			continue
		}
		r := *action.Result
		r.EvidenceID, r.WorkerPID = "", 0
		r.Artifacts = append([]ArtifactRef(nil), r.Artifacts...)
		for i := range r.Artifacts {
			r.Artifacts[i].Preview = nil
		}
		set["action:"+valueHash(struct {
			Tool, Adapter, Arguments, Status string
			Result                           ToolResult
		}{action.Name, action.Adapter, canonicalActionArguments(action.Arguments), action.Status, r})] = true
	}
	for _, task := range s.Plan.Tasks {
		task.LeaseUntil = time.Time{}
		task.WorkerID, task.ChildRunID, task.EvidenceRunID = "", "", ""
		task.EvidenceIDs = nil
		set["task:"+valueHash(task)] = true
	}
	for _, check := range s.Checks {
		// Repeating a check with fresh IDs or paraphrased reasoning is not progress.
		set["check:"+valueHash([]string{check.RequirementID, check.RequirementHash, check.Status})] = true
	}
	for _, asset := range s.Assets {
		asset.EvidenceIDs = nil
		set["asset:"+valueHash(asset)] = true
	}
	for _, worker := range s.Workers {
		set["worker:"+valueHash([]string{worker.TaskID, worker.Status, worker.CleanupStatus})] = true
	}
	for _, message := range s.Messages {
		if message.Role != "user" || strings.HasPrefix(message.Content, "Runtime completion audit:") {
			continue
		}
		set["input:"+promptHash(message.Content)] = true
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return valueHash(keys)
}

func canonicalActionArguments(raw string) string {
	var value any
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return raw
	}
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func completionDiagnosticFingerprint(reasons []string, issues []ResultValidationIssue) string {
	// Compare stable diagnostics, never events, timestamps or the whole result.
	set := map[string]bool{}
	for _, reason := range reasons {
		set["reason:"+reason] = true
	}
	for _, issue := range issues {
		set["issue:"+valueHash(issue)] = true
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return valueHash(keys)
}

func deferIncompleteCompletion(s *Snapshot, reasons []string) bool {
	if deferResultReferenceRepair(s, reasons) {
		return true
	}
	fingerprint := completionProgressFingerprint(s)
	diagnostic := completionDiagnosticFingerprint(reasons, s.Result.ValidationIssues)
	previous, previousDiagnostic := "", ""
	stalled := false
	for i := len(s.Events) - 1; i >= 0; i-- {
		if s.Events[i].Type != "completion_deferred" && s.Events[i].Type != "completion_stalled" {
			continue
		}
		var data struct {
			ProgressFingerprint   string `json:"progress_fingerprint"`
			DiagnosticFingerprint string `json:"diagnostic_fingerprint"`
		}
		raw, _ := json.Marshal(s.Events[i].Data)
		if json.Unmarshal(raw, &data) != nil || data.ProgressFingerprint == "" {
			continue
		}
		if s.Events[i].Type == "completion_deferred" {
			previous = data.ProgressFingerprint
			previousDiagnostic = data.DiagnosticFingerprint
			break
		}
		if data.ProgressFingerprint == fingerprint && data.DiagnosticFingerprint == diagnostic {
			stalled = true
		}
	}
	if previous == fingerprint {
		// Only the existing explicit budget, not a repeated fingerprint, may pause.
		if reason := budgetReason(s, "model"); reason != "" {
			pauseBudget(s, reason)
			return true
		}
		if previousDiagnostic == diagnostic {
			s.Run.Status, s.Run.PauseReason = StatusRunning, ""
			refreshResult(s)
			if !stalled {
				addEvent(s, "completion_stalled", "纠偏后无新增进展，继续采取不同动作或重新规划", map[string]any{"reasons": reasons, "progress_fingerprint": fingerprint, "diagnostic_fingerprint": diagnostic, "replan_requested": true})
			}
			return true
		}
	}
	// Legacy events lack a diagnostic signature: send the precise audit once.
	s.Run.Status, s.Run.PauseReason = StatusRunning, ""
	s.Messages = append(s.Messages, Message{Role: "user", Content: "Runtime completion audit: " + strings.Join(reasons, "; ") + ". Resolve the exact remaining gaps using existing observations first. Correct references or answer formatting without repeating target execution; collect new evidence only when an actual required observation is missing. Use evidence_check with archived evidence for declared requirements. Dispose failed/cancelled tasks explicitly with task_dispose when further execution is unwarranted; disposal is not successful acceptance. If blocked, request_input must name the exact missing input. " + resultValidationRepairContext(s), CreatedAt: time.Now().UTC()})
	addEvent(s, "completion_deferred", "验收证据尚未满足，执行具体纠偏", map[string]any{"reasons": reasons, "progress_fingerprint": fingerprint, "diagnostic_fingerprint": diagnostic})
	return true
}
