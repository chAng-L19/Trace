package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func verificationDefinitions() []ToolDefinition {
	str := map[string]string{"type": "string"}
	ids := map[string]any{"type": "array", "items": str}
	predicate := map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"contains", "equals", "regex", "json_equals"}}, "value": str, "pointer": str, "expected": map[string]any{}}, "required": []string{"kind"}}
	var out []ToolDefinition
	add := func(name, description string, props map[string]any, required ...string) {
		d := ToolDefinition{Type: "function"}
		d.Function.Name = name
		d.Function.Description = description
		if required == nil {
			required = []string{}
		}
		d.Function.Parameters = map[string]any{"type": "object", "properties": props, "required": required}
		out = append(out, d)
	}
	add("verification_status", "Inspect acceptance requirements, the separate persisted stdout-contains predicate, executed checks and findings. The persisted predicate is evaluated automatically, not by inventing a requirement ID. A model assessment is not reproduction.", map[string]any{})
	add("request_input", "Record a concrete missing input without pausing autonomous work. State the exact missing input in reason. Continue with available tools, existing evidence and independent work; do not invent the missing value or declare unmet requirements satisfied. Only an operator control or an explicit budget can pause the run.", map[string]any{"reason": str}, "reason")
	add("evidence_check", "Evaluate an acceptance requirement using actual evidence IDs. Verifies original action/events/messages/CAS, declared probe and stdout predicate. Optional independent model assessment is a separate request. Unknown/failure never counts as pass.", map[string]any{"requirement_id": str, "evidence_ids": ids}, "requirement_id", "evidence_ids")
	add("finding_submit", "Persist a non-CTF finding with root cause, impact, exact evidence, reproducible steps and remediation. Include report.zh_cn and report.en with substantive bilingual prose for automatic report files. Submission is distinct from semantic review and fresh execution retest; edits invalidate prior assessment.", map[string]any{"id": str, "title": str, "severity": map[string]any{"type": "string", "enum": []string{"critical", "high", "medium", "low", "info"}}, "root_cause": str, "impact": str, "remediation": str, "reproduction_steps": ids, "evidence_ids": ids, "predicate": predicate, "report": reportTextSchema()}, "id", "title", "severity", "root_cause", "impact", "remediation", "reproduction_steps", "evidence_ids", "predicate")
	add("finding_review", "Independently assess a submitted finding against exact execution evidence using the configured model. Records pass/fail/unknown with original request and response, without treating assessment as reproduction.", map[string]any{"finding_id": str}, "finding_id")
	add("finding_retest", "Associate a new, completed execution after the finding revision with its declared test predicate. Original evidence, manual observations and generated test suggestions are not retests.", map[string]any{"finding_id": str, "evidence_ids": ids}, "finding_id", "evidence_ids")
	return out
}

func (a *Agent) verificationBuiltin(ctx context.Context, id string, call ToolCall) (any, bool, error) {
	switch call.Name {
	case "verification_status", "evidence_check", "finding_submit", "finding_review", "finding_retest", "request_input":
	default:
		return nil, false, nil
	}
	if call.Name == "request_input" {
		var in struct {
			Reason string `json:"reason"`
		}
		if err := decodeBuiltin(call.Arguments, &in); err != nil {
			return nil, true, err
		}
		if strings.TrimSpace(in.Reason) == "" {
			return nil, true, fmt.Errorf("exact missing input is required")
		}
		_, err := a.store.Update(id, func(s *Snapshot) error {
			if isStopped(s.Run.Status) {
				return fmt.Errorf("run is already paused or cancelled")
			}
			addEvent(s, "input_requested", "已记录缺失输入；继续自动处理可执行工作", map[string]string{"reason": in.Reason})
			return nil
		})
		return map[string]string{"status": "input_unavailable", "reason": in.Reason, "next_action": "Continue autonomously using available tools and evidence. Keep unmet requirements and uncertainty explicit; do not invent the missing input or repeat uncertain actions."}, true, err
	}
	if call.Name == "verification_status" {
		s, e := a.store.Load(id)
		return map[string]any{"root_contract": s.RootContract, "requirements": s.Requirements, "checks": s.Checks, "findings": s.Findings, "unmet": unmetRequirements(&s), "success_predicate": successPredicate(&s)}, true, e
	}
	if call.Name == "finding_submit" {
		var f Finding
		if e := decodeBuiltin(call.Arguments, &f); e != nil {
			return nil, true, e
		}
		s, e := a.SubmitFinding(id, f)
		return s.Findings, true, e
	}
	var in struct {
		RequirementID string   `json:"requirement_id"`
		FindingID     string   `json:"finding_id"`
		EvidenceIDs   []string `json:"evidence_ids"`
	}
	if err := decodeBuiltin(call.Arguments, &in); err != nil {
		return nil, true, err
	}
	var s Snapshot
	var err error
	switch call.Name {
	case "evidence_check":
		s, err = a.CheckEvidence(ctx, id, in.RequirementID, in.EvidenceIDs)
	case "finding_review":
		s, err = a.ReviewFinding(ctx, id, in.FindingID)
	case "finding_retest":
		s, err = a.RetestFinding(id, in.FindingID, in.EvidenceIDs)
	default:
		err = fmt.Errorf("unknown verification operation")
	}
	if call.Name == "evidence_check" {
		return s.Checks, true, err
	}
	return s.Findings, true, err
}

// Decode requirements without losing JSON numbers in operator equality values.
func DecodeRequirements(data []byte) ([]VerificationRequirement, error) {
	var r []VerificationRequirement
	err := json.Unmarshal(data, &r)
	return r, err
}
