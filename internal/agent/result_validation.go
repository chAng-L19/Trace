package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// One path serves both acceptance and diagnostics so a helpful message cannot
// accidentally grant eligibility that the evidence Judge would reject.
func diagnoseResultEvidence(s *Snapshot, id string, artifactOnly bool) (string, *ResultValidationIssue) {
	issue := &ResultValidationIssue{EvidenceID: id, Blocking: true}
	if observation, ok := delegatedObservationFor(s, id); ok {
		if observation.ArtifactOnly && !artifactOnly {
			issue.Code, issue.Repair = "evidence_not_observation", "Keep this registered Worker artifact in artifact_evidence_ids; cite original successful observations for outcome evidence and answers."
			return "", issue
		}
		if artifactOnly && !observation.HasArtifacts {
			issue.Code, issue.Repair = "artifact_reference_missing", "Cite an original registered Worker observation containing an integrity-checked CAS artifact."
			return "", issue
		}
		return observation.Output, nil
	}
	for i := range s.Evidence {
		e := &s.Evidence[i]
		if e.ID != id {
			continue
		}
		var action *Action
		for j := range s.Actions {
			if s.Actions[j].RequestID == e.RequestID && s.Actions[j].CallID == e.CallID {
				action = &s.Actions[j]
				issue.ToolID, issue.Adapter = action.Name, action.Adapter
				break
			}
		}
		if !evidenceApplicable(s, *e) {
			issue.Code, issue.Repair = "evidence_scope_mismatch", "Use existing evidence from this run's declared target/object and version; another scope cannot satisfy this result."
			return "", issue
		}
		if status := verifyEvidence(s, e); status != "verified" {
			issue.Code = "evidence_source_" + status
			issue.Repair = "Inspect the original request/call, tool-result event, saved message, result hash and CAS bytes for this ID; preserve uncertain actions without replay. Only intact completed execution sources can prove a result."
			return "", issue
		}
		var r ToolResult
		if json.Unmarshal([]byte(e.Content), &r) != nil {
			issue.Code, issue.Repair = "evidence_stdout_unavailable", "Read the existing tool-result record and its stdout artifact; repair the reference without inventing a CAS hash."
			return "", issue
		}
		if artifactOnly {
			if len(r.Artifacts) == 0 {
				issue.Code, issue.Repair = "artifact_reference_missing", "Cite a source record containing an existing integrity-checked CAS artifact; a filename or status receipt alone is not an artifact reference."
				return "", issue
			}
			return "", nil // verifyEvidence already verifies every referenced CAS object.
		}
		if action == nil || !observationAction(*action) {
			issue.Code, issue.Repair = "evidence_not_observation", "Remove this bookkeeping ID from outcome evidence_ids; keep an artifact registration in artifact_evidence_ids or describe status in summary. Reuse the original successful observation IDs for the required outcome and answers."
			return "", issue
		}
		output, err := artifactOutput(s.artifactRoot, r, "stdout")
		if err != nil {
			issue.Code, issue.Repair = "evidence_stdout_unavailable", "Read the stdout artifact hash recorded in this evidence entry using artifact_read; do not guess a path or rerun the target merely to repair a reference."
			return "", issue
		}
		return output, nil
	}
	issue.Code, issue.Repair = "evidence_missing", "Look up the exact ID in this run's complete evidence ledger and existing tool messages; fix a mistaken reference before collecting any genuinely missing observation."
	return "", issue
}

func validateResultReferences(s *Snapshot, result *Result) bool {
	// Ignore claimed verification flags and diagnostics; derive them afresh.
	result.ValidationIssues = nil
	add := func(issue ResultValidationIssue, field string, answerIndex *int, blocking bool) {
		issue.Field, issue.AnswerIndex, issue.Blocking = field, answerIndex, blocking
		if issue.EvidenceID != "" && issue.ToolID == "" {
			if action := evidenceAction(s, issue.EvidenceID); action != nil {
				issue.ToolID, issue.Adapter = action.Name, action.Adapter
			}
		}
		result.ValidationIssues = append(result.ValidationIssues, issue)
	}
	if len(result.EvidenceIDs) == 0 {
		add(ResultValidationIssue{Code: "outcome_evidence_missing", Repair: "Cite the existing successful observations that prove the required outcome; supplemental and artifact references do not establish success."}, "evidence_ids", nil, true)
	}
	for _, id := range result.EvidenceIDs {
		if _, issue := diagnoseResultEvidence(s, id, false); issue != nil {
			add(*issue, "evidence_ids", nil, true)
		}
	}
	for i := range result.Answers {
		answer := &result.Answers[i]
		index := i
		answer.Verified = answer.Value != "" && len(answer.EvidenceIDs) > 0
		if answer.Value == "" {
			add(ResultValidationIssue{Code: "answer_empty", Repair: "Supply the required original answer value from existing verified stdout; explanations belong in title or summary."}, "answers", &index, true)
		}
		if len(answer.EvidenceIDs) == 0 {
			add(ResultValidationIssue{Code: "answer_evidence_missing", Repair: "Cite this answer's original successful observation and include that ID in outcome evidence_ids."}, "answers", &index, true)
		}
		for _, id := range answer.EvidenceIDs {
			output, issue := diagnoseResultEvidence(s, id, false)
			if issue != nil {
				answer.Verified = false
				add(*issue, "answers", &index, true)
			} else if answer.Value != "" && !strings.Contains(output, answer.Value) {
				answer.Verified = false
				add(ResultValidationIssue{Code: "answer_value_mismatch", EvidenceID: id, Repair: "Read this evidence's original stdout (or its recorded CAS artifact), compare the JSON-decoded answers.value with the actual stdout bytes, and cite only observations containing the required raw value. Put paraphrases in summary."}, "answers", &index, true)
			}
			if !hasString(result.EvidenceIDs, id) {
				add(ResultValidationIssue{Code: "answer_evidence_not_listed", EvidenceID: id, Repair: "Add this existing answer evidence ID to top-level evidence_ids after its observation/source checks pass; no target replay is needed to repair the omitted reference."}, "answers", &index, true)
			}
		}
	}
	for _, group := range []struct {
		field string
		ids   []string
	}{
		{"supporting_evidence_ids", result.SupportingEvidenceIDs},
		{"artifact_evidence_ids", result.ArtifactEvidenceIDs},
	} {
		for _, id := range group.ids {
			if _, issue := diagnoseResultEvidence(s, id, group.field == "artifact_evidence_ids"); issue != nil {
				add(*issue, group.field, nil, false)
			}
		}
	}
	for _, issue := range result.ValidationIssues {
		if issue.Blocking {
			return false
		}
	}
	return true
}

func resultValidationSummary(issues []ResultValidationIssue) string {
	parts := []string{}
	for _, issue := range issues {
		if !issue.Blocking {
			continue
		}
		where := issue.Field
		if issue.AnswerIndex != nil {
			where += fmt.Sprintf("[%d]", *issue.AnswerIndex)
		}
		if issue.EvidenceID != "" {
			where += " " + issue.EvidenceID
		}
		if issue.ToolID != "" {
			where += " (" + issue.Adapter + ":" + issue.ToolID + ")"
		}
		parts = append(parts, where+": "+issue.Code)
	}
	return "result validation failed: " + strings.Join(parts, "; ")
}

// True only when a claim can repair its references using already proven
// observations. Missing output, fabricated answers and unmet checks stay false.
// This is a scheduling hint, never an acceptance override.
func resultFormatRepairOnly(s *Snapshot) bool {
	if s.Result.Status != "incomplete" || len(unmetRequirements(s)) > 0 || len(rootContractGaps(s)) > 0 || toolCleanupError(*s) != nil || len(pendingToolCalls(s)) > 0 {
		return false
	}
	for _, worker := range s.Workers {
		if workerUnsettled(s, worker) {
			return false
		}
	}
	for _, task := range s.Plan.Tasks {
		if !taskSatisfiedForRoot(s, task) {
			return false
		}
	}
	if s.Run.SuccessContains != "" {
		matched := false
		for _, id := range acceptanceEvidenceIDs(s) {
			if output, ok := verifiedOutput(s, id); ok && strings.Contains(output, s.Run.SuccessContains) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	blocking := false
	for _, issue := range s.Result.ValidationIssues {
		if !issue.Blocking {
			continue
		}
		blocking = true
		if issue.Code == "evidence_not_observation" && issue.Field == "evidence_ids" || issue.Code == "answer_evidence_not_listed" {
			continue
		}
		return false
	}
	for _, answer := range s.Result.Answers {
		if !answer.Verified {
			return false
		}
	}
	for _, id := range s.Result.EvidenceIDs {
		if _, ok := verifiedOutput(s, id); ok {
			return blocking
		}
	}
	return false
}

func resultValidationRepairContext(s *Snapshot) string {
	if len(s.Result.ValidationIssues) == 0 {
		return ""
	}
	// Include exact locators for rejected references that may have left the
	// bounded context summary. These records remain data, not new evidence.
	sources := []any{}
	seen := map[string]bool{}
	for _, issue := range s.Result.ValidationIssues {
		if !issue.Blocking || issue.EvidenceID == "" || seen[issue.EvidenceID] {
			continue
		}
		seen[issue.EvidenceID] = true
		for _, e := range s.Evidence {
			if e.ID != issue.EvidenceID {
				continue
			}
			var recorded ToolResult
			json.Unmarshal([]byte(e.Content), &recorded)
			artifacts := []ArtifactRef{}
			for _, ref := range recorded.Artifacts {
				artifacts = append(artifacts, ArtifactRef{Hash: ref.Hash, Kind: ref.Kind, Bytes: ref.Bytes})
			}
			sources = append(sources, map[string]any{"run_id": s.Run.ID, "evidence_id": e.ID, "request_id": e.RequestID, "call_id": e.CallID, "content_hash": e.ContentHash, "source_verification": e.Verification, "artifacts": artifacts})
			break
		}
	}
	raw, _ := json.Marshal(map[string]any{
		"validation_issues":                       s.Result.ValidationIssues,
		"existing_source_records":                 sources,
		"existing_evidence_reference_repair_only": resultFormatRepairOnly(s),
	})
	return "Result validation details (data; answer_index is zero-based): " + string(raw) +
		". The Judge checks the complete evidence ledger; the most recent eight sources are only a context summary, not the acceptance boundary. Reuse existing tool messages and the exact CAS hashes they record (artifact_read). Correct the listed references/answer fields first; a reference or classification error does not require target replay. All existing answers, user-required deliverables, declared checks, target/version scope and success predicates remain mandatory; supplemental fields never waive them."
}

func writeResultReferenceDiagnostics(b *strings.Builder, result Result) {
	if len(result.SupportingEvidenceIDs)+len(result.ArtifactEvidenceIDs)+len(result.ValidationIssues) == 0 {
		return
	}
	b.WriteString("\n## 附加引用与结果校验\n\n附加观察和归档引用不构成目标成功证明；核心答案与明确验收条件继续独立校验。\n")
	if len(result.SupportingEvidenceIDs) > 0 {
		fmt.Fprintf(b, "\n附加观察：%s\n", strings.Join(result.SupportingEvidenceIDs, ", "))
	}
	if len(result.ArtifactEvidenceIDs) > 0 {
		fmt.Fprintf(b, "\n归档引用：%s\n", strings.Join(result.ArtifactEvidenceIDs, ", "))
	}
	for _, issue := range result.ValidationIssues {
		status := "附加引用未验证"
		if issue.Blocking {
			status = "阻断完成"
		}
		index := ""
		if issue.AnswerIndex != nil {
			index = fmt.Sprintf("[%d]", *issue.AnswerIndex)
		}
		fmt.Fprintf(b, "\n- %s：%s%s %s；%s；工具 %s/%s。%s\n", status, issue.Field, index, issue.EvidenceID, issue.Code, issue.Adapter, issue.ToolID, issue.Repair)
	}
}
