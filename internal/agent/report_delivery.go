package agent

import (
	"encoding/json"
	"strings"
	"time"
)

type reportTranslation struct {
	ReportText
	Kind      string `json:"kind"`
	SubjectID string `json:"subject_id"`
}

type reportSubmission struct {
	ReportText
	Documents []reportTranslation `json:"documents,omitempty"`
}

type ReportDeliveryState struct {
	Pending         bool   `json:"pending"`
	DocumentsDigest string `json:"documents_digest,omitempty"`
}

func reportDeliveryPending(s *Snapshot) bool {
	return s.ReportDelivery != nil && s.ReportDelivery.Pending && s.Run.Status != StatusCancelled && s.Run.Status != StatusFailed
}

func needsReportDelivery(s *Snapshot) bool {
	if reportDeliveryPending(s) || s.Result.Report != nil || len(s.Requirements)+len(s.Findings)+len(s.ChildReports) > 0 || s.WorkerCheckpoint != nil || s.Run.SuccessContains != "" {
		return true
	}
	for _, action := range s.Actions {
		if observationAction(action) || action.Name == "artifact_register" || action.Name == "source_search" && action.Result != nil {
			return true
		}
	}
	return false
}

// Reports can accompany natural completion prose without a trace_result claim.
// Translation-only replies retain the preceding business result and bind to
// its exact source digest; they never replace evidence or execution state.
func finalReportContent(s *Snapshot) (string, *ReportText) {
	var body *ReportText
	fallback := ""
	for i := len(s.Messages) - 1; i >= 0; i-- {
		m := s.Messages[i]
		if m.Role != "assistant" || len(m.ToolCalls) > 0 {
			continue
		}
		text := m.Content
		if submission, _ := traceEnvelope[reportSubmission](text, "trace_report"); submission != nil {
			plain := stripReportEnvelope(text)
			if body == nil && (!m.ReportOnly || submission.SourceDigest != "") && (submission.ZhCN != "" || submission.En != "") {
				copy := submission.ReportText
				body = &copy
			}
			if !m.ReportOnly && plain == "" && submission.SourceDigest == "" {
				fallback = submission.ZhCN
				if fallback == "" {
					fallback = submission.En
				}
				if fallback != "" {
					return fallback, body
				}
			}
			text = plain
			if text == "" {
				continue
			}
		}
		if m.ReportOnly {
			continue
		}
		return text, body
	}
	return fallback, body
}

func stripReportEnvelope(text string) string {
	if _, present := traceEnvelopeJSON[reportSubmission](strings.TrimSpace(text), "trace_report"); present {
		if _, claim := traceEnvelopeJSON[Result](strings.TrimSpace(text), "trace_result"); !claim {
			return ""
		}
		return text
	}
	lines := strings.Split(text, "\n")
	var out []string
	for i := 0; i < len(lines); i++ {
		fence := strings.TrimSpace(lines[i])
		if fence != "```json" && fence != "~~~json" {
			out = append(out, lines[i])
			continue
		}
		end := i + 1
		for end < len(lines) && strings.TrimSpace(lines[end]) != fence[:3] {
			end++
		}
		if end == len(lines) {
			out = append(out, lines[i:]...)
			break
		}
		raw := strings.Join(lines[i+1:end], "\n")
		_, report := traceEnvelopeJSON[reportSubmission](raw, "trace_report")
		_, result := traceEnvelopeJSON[Result](raw, "trace_result")
		if !report || result {
			out = append(out, lines[i:end+1]...)
		}
		i = end
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func applyReportTranslations(s *Snapshot) {
	for i := len(s.Messages) - 1; i >= 0; i-- {
		if s.Messages[i].Role != "assistant" {
			continue
		}
		submission, _ := traceEnvelope[reportSubmission](s.Messages[i].Content, "trace_report")
		if submission == nil {
			return
		}
		candidates := reportCandidates(s)
		for _, doc := range submission.Documents {
			if doc.SourceDigest == "" {
				continue
			}
			for _, c := range candidates {
				if c.source.Kind != doc.Kind || c.source.SubjectID != doc.SubjectID || c.digest() != doc.SourceDigest {
					continue
				}
				body := doc.ReportText
				if doc.Kind == "finding" {
					if finding := findingByID(s, doc.SubjectID); finding != nil {
						finding.Report = &body
					}
				} else if doc.Kind == "stage" && s.WorkerCheckpoint != nil {
					s.WorkerCheckpoint.Report = &body
				}
			}
		}
		return
	}
}

// Use pi's existing continuation path for delivery. Every incomplete response
// receives source-bound correction feedback under the existing run budget.
func continueForReport(s *Snapshot, preparing ...bool) bool {
	refreshResult(s)
	if !needsReportDelivery(s) {
		return false
	}
	// Freeze automatic source references before issuing a translation digest.
	// The post-callback archive pass must not invalidate the request it just saved.
	archiveConclusionChains(s)
	applyReportTranslations(s)
	var missing []map[string]any
	for _, c := range reportCandidates(s) {
		_, languages := c.bodies()
		if len(languages) > 0 {
			missing = append(missing, map[string]any{"kind": c.source.Kind, "subject_id": c.source.SubjectID, "source_digest": c.digest(), "missing_languages": languages, "source": c.source})
		}
	}
	if len(missing) == 0 {
		if len(preparing) > 0 && preparing[0] {
			return false
		}
		if s.ReportDelivery != nil {
			s.ReportDelivery.Pending = false
		}
		return false
	}
	raw, _ := json.Marshal(missing)
	text := "Complete report delivery in this same pi conversation. Existing business checks and execution results are retained. Do not repeat tools, probes or finding submissions just to translate. Return one closed json fence containing trace_report with source_digest, zh_cn and en for the final report; include documents:[{kind,subject_id,source_digest,zh_cn,en}] for the listed finding/stage reports. Both languages need substantive factual prose covering summary, root cause/conditions where applicable, reproduction explanation, impact, remediation and unresolved items. Preserve commands, values, hashes, source IDs, business and cleanup states exactly. Do not copy Chinese into en or translate headings only. Translation is not new execution proof. Pending source-bound documents: " + string(raw)
	if reportDeliveryPending(s) {
		preparation := len(preparing) > 0 && preparing[0]
		if !preparation || s.ReportDelivery.DocumentsDigest != valueHash(missing) {
			s.Messages = append(s.Messages, Message{Role: "user", Content: text, CreatedAt: time.Now().UTC()})
			s.ReportDelivery.DocumentsDigest = valueHash(missing)
		}
		// Refresh source-bound delivery context before a resumed model request,
		// without another retry or another business/tool execution phase.
		if preparation {
			return false
		}
		s.Run.Status, s.Run.PauseReason = StatusRunning, ""
		addEvent(s, "report_translation_missing", "报告正文译文未齐；自动继续当前补译回合", map[string]any{"documents": missing})
		return true
	}
	s.Messages = append(s.Messages, Message{Role: "user", Content: text, CreatedAt: time.Now().UTC()})
	s.ReportDelivery = &ReportDeliveryState{Pending: true, DocumentsDigest: valueHash(missing)}
	s.Run.Status = StatusRunning
	addEvent(s, "report_translation_requested", "沿现有模型回合补齐中英报告正文", map[string]any{"documents": len(missing)})
	return true
}

func reportTextSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"zh_cn": map[string]string{"type": "string"}, "en": map[string]string{"type": "string"}}, "required": []string{"zh_cn", "en"}, "description": "Substantive Chinese and English report prose. Preserve command/value/hash/source/acceptance/cleanup fields; translation is not execution proof."}
}
