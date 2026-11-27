package agent

// Adapted contracts: REA (MIT) evidenceCompletionLedger.ts and Muteki
// judgement.py (AGPL-3.0): exact evidence snapshots, revocable assessment and
// separate review/reproduction. This implementation uses Trace's single Store.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"strconv"
	"strings"
)

func valueHash(v any) string {
	b, _ := json.Marshal(v)
	// Struct fields and decoded map keys must hash identically. UseNumber
	// preserves integer predicates larger than float64's exact range.
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	var canonical any
	if decoder.Decode(&canonical) == nil {
		b, _ = json.Marshal(canonical)
	}
	return promptHash(string(b))
}

func validatePredicate(p EvidencePredicate) error {
	switch p.Kind {
	case "contains", "equals":
		if p.Value == "" {
			return fmt.Errorf("predicate value is required")
		}
	case "regex":
		if p.Value == "" {
			return fmt.Errorf("regex is required")
		}
		_, err := regexp.Compile(p.Value)
		return err
	case "json_equals":
		if p.Pointer != "" && !strings.HasPrefix(p.Pointer, "/") {
			return fmt.Errorf("use an RFC 6901 JSON pointer")
		}
	default:
		return fmt.Errorf("unsupported predicate kind %q", p.Kind)
	}
	return nil
}

func predicateMatches(p EvidencePredicate, output string) (bool, error) {
	if err := validatePredicate(p); err != nil {
		return false, err
	}
	switch p.Kind {
	case "contains":
		return strings.Contains(output, p.Value), nil
	case "equals":
		return output == p.Value, nil
	case "regex":
		r, _ := regexp.Compile(p.Value)
		return r.MatchString(output), nil
	case "json_equals":
		var v any
		d := json.NewDecoder(strings.NewReader(output))
		d.UseNumber()
		if err := d.Decode(&v); err != nil {
			return false, err
		}
		if err := d.Decode(new(any)); err != io.EOF {
			return false, fmt.Errorf("output contains trailing JSON data")
		}
		if p.Pointer != "" {
			for _, key := range strings.Split(p.Pointer[1:], "/") {
				key = strings.ReplaceAll(strings.ReplaceAll(key, "~1", "/"), "~0", "~")
				switch item := v.(type) {
				case map[string]any:
					var ok bool
					v, ok = item[key]
					if !ok {
						return false, fmt.Errorf("JSON pointer key missing")
					}
				case []any:
					i, err := strconv.Atoi(key)
					if err != nil || i < 0 || i >= len(item) || strconv.Itoa(i) != key {
						return false, fmt.Errorf("JSON pointer index missing")
					}
					v = item[i]
				default:
					return false, fmt.Errorf("JSON pointer traverses a scalar")
				}
			}
		}
		// Normalize both through JSON so numeric decoding choices cannot alter equality.
		return valueHash(v) == valueHash(p.Expected), nil
	}
	return false, fmt.Errorf("unsupported predicate")
}

func evidenceReferences(s *Snapshot, ids []string) ([]EvidenceReference, error) {
	if len(ids) == 0 {
		return nil, fmt.Errorf("execution evidence is required")
	}
	refs := []EvidenceReference{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return nil, fmt.Errorf("duplicate evidence %s", id)
		}
		seen[id] = true
		if observation, ok := delegatedObservationFor(s, id); ok {
			if observation.ArtifactOnly {
				return nil, fmt.Errorf("evidence %s is an artifact reference, not an execution observation", id)
			}
			refs = append(refs, observation.Reference)
			continue
		}
		found := false
		for _, e := range s.Evidence {
			if e.ID == id {
				if !evidenceApplicable(s, e) || verifyEvidence(s, &e) != "verified" {
					return nil, fmt.Errorf("evidence %s is not verified", id)
				}
				refs = append(refs, EvidenceReference{ID: id, Hash: e.ContentHash})
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("evidence %s missing", id)
		}
	}
	return refs, nil
}

func checkReferencesCurrent(s *Snapshot, c VerificationCheck) bool {
	if len(c.EvidenceIDs) != len(c.References) {
		return false
	}
	for i, id := range c.EvidenceIDs {
		if c.References[i].ID != id {
			return false
		}
	}
	return referencesCurrent(s, c.References)
}

func referencesCurrent(s *Snapshot, refs []EvidenceReference) bool {
	ids := make([]string, len(refs))
	for i, r := range refs {
		ids[i] = r.ID
	}
	current, err := evidenceReferences(s, ids)
	return err == nil && reflect.DeepEqual(current, refs)
}

func checkPredicate(s *Snapshot, p EvidencePredicate, ids []string, toolID, argumentsHash string, after int) (string, string) {
	if _, err := evidenceReferences(s, ids); err != nil {
		return "unknown", err.Error()
	}
	matched := false
	for _, id := range ids {
		if observation, ok := delegatedObservationFor(s, id); ok {
			if after > 0 {
				return "unknown", "a run-local sequence boundary cannot be applied to delegated evidence"
			}
			if toolID != "" && observation.ToolID != toolID {
				return "unknown", "evidence tool does not match the declared probe"
			}
			if argumentsHash != "" && observation.ArgumentsHash != argumentsHash {
				return "unknown", "probe arguments do not match the declared hash"
			}
			pass, err := predicateMatches(p, observation.Output)
			if err != nil {
				return "unknown", err.Error()
			}
			matched = matched || pass
			continue
		}
		for _, e := range s.Evidence {
			if e.ID != id {
				continue
			}
			if e.SourceEventSequence <= after {
				return "unknown", "evidence predates the required execution boundary"
			}
			for _, a := range s.Actions {
				if a.CallID != e.CallID || a.RequestID != e.RequestID {
					continue
				}
				if after > 0 && actionStartSequence(s, a) <= after {
					return "unknown", "probe must start after the required execution boundary"
				}
				if toolID != "" && a.Name != toolID {
					return "unknown", "evidence tool does not match the declared probe"
				}
				if argumentsHash != "" && promptHash(a.Arguments) != argumentsHash {
					return "unknown", "probe arguments do not match the declared hash"
				}
			}
			output, ok := verifiedOutput(s, id)
			if !ok {
				return "unknown", "evidence stdout unavailable"
			}
			pass, err := predicateMatches(p, output)
			if err != nil {
				return "unknown", err.Error()
			}
			matched = matched || pass
		}
	}
	if matched {
		return "pass", "declared predicate matched verified execution output"
	}
	return "fail", "declared predicate did not match execution output"
}

func (a *Agent) SetVerificationRequirements(id string, requirements []VerificationRequirement) (Snapshot, error) {
	return a.SetAcceptanceContract(id, requirements, nil)
}

func ValidateRequirements(requirements []VerificationRequirement) error {
	seen := map[string]bool{}
	for _, r := range requirements {
		if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Description) == "" || seen[r.ID] {
			return fmt.Errorf("unique requirement ID and description are required")
		}
		seen[r.ID] = true
		if r.Category != "goal" && r.Category != "cleanup" {
			return fmt.Errorf("category must be goal or cleanup")
		}
		if err := validatePredicate(r.Predicate); err != nil {
			return err
		}
		if r.Category == "cleanup" && (r.ToolID == "" || len(r.ArgumentsHash) != 64) {
			return fmt.Errorf("cleanup requires an explicit probe tool and arguments SHA256")
		}
	}
	return nil
}

func (a *Agent) SetAcceptanceContract(id string, requirements []VerificationRequirement, successContains *string) (Snapshot, error) {
	if err := ValidateRequirements(requirements); err != nil {
		return Snapshot{}, err
	}
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	return a.store.Update(id, func(s *Snapshot) error {
		if len(s.Requests) > 0 || len(s.Actions) > 0 || s.Run.Status == StatusRunning || s.Run.Status == StatusWaitingTool {
			return fmt.Errorf("requirements are fixed before execution; create a new run to change them")
		}
		if s.RootContract != nil {
			for _, id := range s.RootContract.RequirementIDs {
				matched := false
				for _, r := range requirements {
					if r.ID == id && valueHash(r) == s.RootContract.RequirementHashes[id] {
						matched = true
					}
				}
				if !matched {
					return fmt.Errorf("root requirement cannot be removed or changed: %s", id)
				}
			}
		}
		if successContains != nil {
			s.Run.SuccessContains = *successContains
		}
		s.Requirements = requirements
		s.Checks = nil
		addEvent(s, "requirements_set", "运行验收条件已保存", map[string]any{"requirements": requirements, "success_contains": s.Run.SuccessContains})
		return nil
	})
}

func requirementByID(s *Snapshot, id string) *VerificationRequirement {
	for i := range s.Requirements {
		if s.Requirements[i].ID == id {
			return &s.Requirements[i]
		}
	}
	return nil
}

func refreshVerification(s *Snapshot) {
	for i := range s.Checks {
		c := &s.Checks[i]
		r := requirementByID(s, c.RequirementID)
		if r == nil || c.RequirementHash != valueHash(r) || !checkReferencesCurrent(s, *c) {
			c.Status = "unknown"
			c.Reason = "requirement or source evidence changed"
			continue
		}
		c.Status, c.Reason = checkRequirement(s, *r, c.EvidenceIDs)
		if r.RequireReview && c.Status == "pass" {
			if !reviewCurrent(s, c.Review, valueHash(r), c.References) {
				c.Status = "unknown"
				c.Reason = "independent semantic assessment is missing or stale"
			} else {
				c.Status, c.Reason = c.Review.Status, c.Review.Reason
			}
		}
	}
	refreshFindings(s)
}

func observationAction(a Action) bool {
	return a.Adapter == "exec" || a.Adapter == "local" || a.Adapter == "mcp" || a.Adapter == "builtin" && (a.Name == "read" || a.Name == "artifact_read")
}

// A cleanup probe says nothing about later effects. Unknown external calls are
// treated as potential mutations; bookkeeping and the declared probes are not.
func cleanupFreshnessFloor(s *Snapshot) int {
	floor := 0
	// Children may mutate the same target until they have stopped. Their own
	// evidence stays in the child ledger; the parent settlement event is enough
	// to require a new parent probe after all known workers finish.
	for _, w := range s.Workers {
		if workerUnsettled(s, w) {
			return s.Run.EventSeq
		}
	}
	for _, e := range s.Events {
		if (e.Type == "worker_started" || e.Type == "worker_finished") && e.Sequence > floor {
			floor = e.Sequence
		}
	}
	for _, a := range s.Actions {
		if !observationAction(a) && a.Name != "write" && a.Name != "edit" {
			continue
		}
		if a.Adapter == "builtin" && (a.Name == "read" || a.Name == "artifact_read") {
			continue
		}
		probe := false
		for _, r := range s.Requirements {
			if r.Category == "cleanup" && r.ToolID == a.Name && r.ArgumentsHash == promptHash(a.Arguments) {
				probe = true
				break
			}
		}
		if probe {
			continue
		}
		if a.Status == "started" || a.Status == "uncertain" {
			return s.Run.EventSeq
		}
		if sequence := actionStartSequence(s, a); sequence > floor {
			floor = sequence
		}
		for _, e := range s.Evidence {
			if e.CallID == a.CallID && e.RequestID == a.RequestID && e.SourceEventSequence > floor {
				floor = e.SourceEventSequence
			}
		}
	}
	return floor
}

func actionStartSequence(s *Snapshot, a Action) int {
	for _, e := range s.Events {
		if e.Type != "action_started" {
			continue
		}
		raw, _ := json.Marshal(e.Data)
		var fields map[string]string
		json.Unmarshal(raw, &fields)
		if fields["call_id"] == a.CallID && fields["request_id"] == a.RequestID {
			return e.Sequence
		}
	}
	return 0
}

func latestCheck(s *Snapshot, id string) *VerificationCheck {
	for i := len(s.Checks) - 1; i >= 0; i-- {
		if s.Checks[i].RequirementID == id {
			return &s.Checks[i]
		}
	}
	return nil
}

func unmetRequirements(s *Snapshot) []string {
	var missing []string
	for _, r := range s.Requirements {
		c := latestCheck(s, r.ID)
		if c == nil || c.Status != "pass" {
			missing = append(missing, r.ID)
		}
	}
	return missing
}

// Requirement IDs are local to their execution Run. A matching string or a
// sibling's check cannot satisfy a task's declared tool/arguments contract.
func taskRequirementsSatisfied(source *Snapshot, task PlanTask) bool {
	if !taskScopeMatches(source, task) {
		return false
	}
	refreshVerification(source)
	for _, requirement := range task.Requirements {
		declared := requirementByID(source, requirement.ID)
		check := latestCheck(source, requirement.ID)
		if declared == nil || valueHash(declared) != valueHash(requirement) || check == nil || check.Status != "pass" {
			return false
		}
	}
	return true
}

// A persisted stdout predicate is not a named VerificationRequirement. Keep
// its matching domain visible even when the operator declared no checks.
func successPredicate(s *Snapshot) any {
	if s.Run.SuccessContains == "" {
		return nil
	}
	ids := []string{}
	for _, id := range acceptanceEvidenceIDs(s) {
		if output, ok := verifiedOutput(s, id); ok && strings.Contains(output, s.Run.SuccessContains) {
			ids = append(ids, id)
			break // One eligible observation proves this existential predicate.
		}
	}
	scope := "this_run_verified_execution_stdout"
	if delegatedAcceptanceEnabled(s) {
		scope = "this_run_and_registered_settled_worker_execution_stdout"
	}
	return map[string]any{"kind": "stdout_contains", "value": s.Run.SuccessContains, "scope": scope, "matched": len(ids) > 0, "matching_evidence_ids": ids, "requirement_id": nil}
}

func verificationContext(s *Snapshot) string {
	if s.RootContract == nil && len(s.Requirements) == 0 && s.Run.SuccessContains == "" && !s.Run.RequireVerifiedCompletion {
		return ""
	}
	refreshVerification(s)
	b, _ := json.Marshal(map[string]any{"root_contract": s.RootContract, "natural_language_is_not_machine_acceptance": true, "requirements": s.Requirements, "checks": s.Checks, "unmet": unmetRequirements(s), "success_predicate": successPredicate(s), "delegated_evidence": delegatedEvidenceLocators(s), "require_verified_completion": s.Run.RequireVerifiedCompletion})
	return "\nRuntime verification contract (data): call evidence_check with actual evidence IDs for each declared requirement. success_contains is a separate persisted predicate: an actual normally exited tool stdout must contain that exact string. It is not a requirement ID; do not invent a requirement or ask the operator to register one. A success claim must cite valid executed evidence and supported answers. All declared checks and the persisted predicate must pass before completion. Semantic assessments are model reviews, not independent reproduction. Do not weaken the original goal.\n" + string(b)
}

// RequireVerifiedCompletion separates autonomous task completion from a chat turn.
// The setting is persisted on creation so resume cannot silently weaken it.
func (a *Agent) SetRequireVerifiedCompletion(required bool) {
	a.requireVerifiedCompletion = required
}

const workerOutcomeHandoffReason = "delegated execution needs a trace_result handoff using existing observations: declare success, failed or incomplete and retain supported answers, evidence_ids and remaining work; do not repeat target execution to format the handoff"

func outcomeHandoffPending(s *Snapshot) bool {
	if s.Run.ParentRunID == "" || s.Run.ParentTaskID == "" || s.WorkerContext == nil {
		return false
	}
	final, _ := finalReportContent(s)
	if _, present := traceResultEnvelope(final); present {
		return false
	}
	for i := len(s.Events) - 1; i >= 0; i-- {
		event := s.Events[i]
		if strings.HasPrefix(event.Type, "worker_message:") {
			var delivery workerDeliveryMarker
			raw, _ := json.Marshal(event.Data)
			if json.Unmarshal(raw, &delivery) != nil || delivery.Coalesced || delivery.MessageID == "" || delivery.RepresentativeID != delivery.MessageID || event.Type != "worker_message:"+delivery.MessageID || delivery.InboxRunID == "" || delivery.HistoryMessageID != workerHistoryMessageID(delivery.InboxRunID, delivery.MessageID) {
				continue
			}
			if delivery.Receiver != nil && delivery.Receiver.RunID != s.Run.ID {
				continue
			}
			added := false
			for _, message := range s.Messages {
				added = added || message.ID == delivery.HistoryMessageID && message.Role == "user"
			}
			for _, previous := range s.Events[:i] {
				if previous.Type == event.Type {
					added = false // A retry marker cannot add the same history twice.
					break
				}
			}
			if added {
				return false
			}
		}
		switch event.Type {
		case "message_added", "message_consumed":
			return false // New business input supersedes a formatting-only turn.
		case "completion_deferred", "completion_stalled":
			var audit struct {
				Reasons []string `json:"reasons"`
			}
			raw, _ := json.Marshal(s.Events[i].Data)
			if json.Unmarshal(raw, &audit) != nil {
				return false
			}
			return len(audit.Reasons) == 1 && audit.Reasons[0] == workerOutcomeHandoffReason
		}
	}
	return false
}

func continueForEvidence(s *Snapshot) bool {
	refreshVerification(s)
	refreshResult(s)
	if reason := planDispositionFailure(s); reason != "" {
		// Business disposition is a terminal failure, not an acceptance waiver.
		// Persist the root outcome so resume and Result cannot reinterpret it.
		s.Run.Status, s.Run.Error = StatusFailed, reason
		refreshResult(s)
		addEvent(s, "plan_failed", "计划已终结，未满足的目标保持失败", map[string]string{"reason": reason})
		return true // Completion handled; finishPiTurn ends on StatusFailed.
	}
	var reasons []string
	if err := toolCleanupError(*s); err != nil {
		reasons = append(reasons, "local process cleanup remains unresolved: "+err.Error())
	}
	reasons = append(reasons, rootContractGaps(s)...)
	reasons = append(reasons, workProgressGaps(s)...)
	if s.Run.RequireVerifiedCompletion && s.Result.Status != "success" {
		reasons = append(reasons, "autonomous task has no verified final result: "+s.Result.TerminalReason)
	}
	if missing := unmetRequirements(s); len(missing) > 0 {
		reasons = append(reasons, "unmet verification requirements: "+strings.Join(missing, ", "))
	}
	for _, t := range s.Plan.Tasks {
		if !taskSatisfiedForRoot(s, t) {
			reasons = append(reasons, "unresolved planned task: "+t.ID)
		}
	}
	for _, w := range s.Workers {
		if workerUnsettled(s, w) {
			reasons = append(reasons, "unresolved worker or cleanup: "+w.ID)
		}
	}
	if s.Run.SuccessContains != "" && s.Result.Status != "success" {
		reasons = append(reasons, fmt.Sprintf("persisted stdout-contains predicate %q has not passed: %s; inspect actual tool stdout and the final claim's evidence_ids/answers, not evidence_check with an invented requirement ID", s.Run.SuccessContains, s.Result.TerminalReason))
	}
	if final, _ := finalReportContent(s); final != "" {
		claim, present := traceResultEnvelope(final)
		delegated := s.Run.ParentRunID != "" && s.Run.ParentTaskID != "" && s.WorkerContext != nil
		// Muteki a73535a worker_result.py: publish the outcome before concluding
		// its intent. Reuse pi's audit turn and the existing source ledger.
		if !present && delegated && s.Result.Status == "incomplete" {
			for _, action := range s.Actions {
				if action.Result != nil && (observationAction(action) || action.Name == "artifact_register") {
					reasons = append(reasons, workerOutcomeHandoffReason)
					break
				}
			}
		}
		// A non-strict worker may honestly hand partial work back to its parent.
		// It does not waive checks, invalid references or a persisted predicate.
		partialHandoff := delegated && !s.Run.RequireVerifiedCompletion && claim != nil && claim.Status == "incomplete" && len(reasons) == 0
		for _, issue := range s.Result.ValidationIssues {
			if issue.Blocking && issue.Code != "outcome_evidence_missing" {
				partialHandoff = false
			}
		}
		if present && (claim == nil || claim.Status != "failed") && s.Result.Status != "success" && !partialHandoff {
			reasons = append(reasons, "structured final result remains incomplete: "+s.Result.TerminalReason)
		}
	}
	if len(reasons) == 0 {
		return false
	}
	return deferIncompleteCompletion(s, reasons)
}
