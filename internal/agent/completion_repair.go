package agent

import (
	"sort"
	"strings"
	"time"
)

// Reference correction is distinct from solving a task. New command timestamps
// cannot repair an unchanged bookkeeping reference or omitted answer reference.
func resultReferenceRepairFingerprint(s *Snapshot) string {
	if !resultFormatRepairOnly(s) {
		return ""
	}
	final, _ := finalReportContent(s)
	claim := traceResultClaim(final)
	if claim == nil || claim.Status != "success" {
		return ""
	}
	parts := map[string]bool{}
	for _, issue := range s.Result.ValidationIssues {
		if !issue.Blocking {
			continue
		}
		hash := ""
		for _, evidence := range s.Evidence {
			if evidence.ID == issue.EvidenceID {
				hash = evidence.ContentHash
				break
			}
		}
		parts[valueHash([]any{issue.Code, issue.Field, issue.EvidenceID, issue.AnswerIndex, hash})] = true
	}
	for _, message := range s.Messages {
		if message.Role == "user" && !strings.HasPrefix(message.Content, "Runtime completion audit:") {
			parts["input:"+valueHash([]any{message.Content, message.Attachments})] = true
		}
	}
	keys := make([]string, 0, len(parts))
	for key := range parts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return valueHash(keys)
}

func deferResultReferenceRepair(s *Snapshot, reasons []string) bool {
	fingerprint := resultReferenceRepairFingerprint(s)
	if fingerprint == "" {
		return false
	}
	if reason := budgetReason(s, "model"); reason != "" {
		pauseBudget(s, reason)
		return true
	}
	// Each repair uses the same pi conversation and explicit run budget. A
	// repeated formatting error is feedback, not an implicit retry limit.
	s.Run.Status, s.Run.PauseReason = StatusRunning, ""
	s.Messages = append(s.Messages, Message{Role: "user", Content: "Runtime completion audit: the required outcome observations and answer values already validate; only the listed result references need correction. Repair the result JSON using the existing evidence ledger. Do not repeat target requests to fix bookkeeping or omitted top-level references. Keep the original user requirements and required reports. " + resultValidationRepairContext(s), CreatedAt: time.Now().UTC()})
	addEvent(s, "completion_reference_repair", "复用已有观察修复结果引用", map[string]any{"fingerprint": fingerprint, "reasons": reasons, "validation_issues": s.Result.ValidationIssues})
	return true
}
