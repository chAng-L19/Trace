package agent

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Source and prose must independently match the receipt. A fixed report ID or
// a body bound to another source is not a delivery credential.
func (c *reportCandidate) matchesDocument(r ReportArchive) bool {
	if c == nil || r.FormatVersion != c.format || r.Kind != c.source.Kind || r.SubjectID != c.source.SubjectID || r.SourceDigest != c.digest() {
		return false
	}
	if c.text != nil && c.text.SourceDigest != "" && c.text.SourceDigest != r.SourceDigest {
		return false
	}
	body, _ := c.bodies()
	return r.DocumentDigest == valueHash(body)
}

// Use historical delivery rules only for an unchanged, completed Format0
// document. New documents and translation requests use the current projection.
func deliveryReportCandidates(s *Snapshot) []reportCandidate {
	out := reportCandidates(s)
	for i, c := range out {
		if c.legacy == nil {
			continue
		}
		for j := len(s.Reports) - 1; j >= 0; j-- {
			r := s.Reports[j]
			if c.matchesDocument(r) {
				break
			}
			if r.TranslationStatus == "complete" && c.legacy.matchesDocument(r) {
				if _, missing := c.legacy.bodies(); len(missing) == 0 {
					out[i] = *c.legacy
				}
				break
			}
		}
	}
	return out
}

func (c reportCandidate) missingImages(body ReportText) []string {
	if c.format == 0 { // The original manifest did not deliver inline attachments.
		return nil
	}
	return reportMissingImages(body, c.source.Attachments)
}

// Preserve the original archive bytes for receipts without a format version.
func renderLegacyArchivedReport(c reportCandidate, r ReportArchive, language, body string) string {
	var b strings.Builder
	if language == "zh-CN" {
		fmt.Fprintf(&b, "# 研究总结报告 · %s · v%d\n\n", c.source.Kind, r.Version)
		if body == "" {
			body = "本来源版本缺少实质中文译文，双语报告交付尚未齐备。"
		}
		fmt.Fprintf(&b, "%s\n\n## 来源与交付状态\n\n双语正文：%s；缺失：%s。正文由模型撰写，归档不新增执行证明，也不改变业务验收或清理结果。固定版本保留生成时状态，当前来源需另行核对。\n\n", body, r.TranslationStatus, strings.Join(r.MissingLanguages, ", "))
		fmt.Fprintf(&b, "来源运行 `%s`；报告类型 `%s`；业务状态 `%s`；清理状态 `%s`；引用证据 %d 条。\n\n", c.source.RunID, c.source.Kind, c.source.Business, c.source.Cleanup, len(c.source.Evidence))
	} else {
		fmt.Fprintf(&b, "# Research summary · %s · v%d\n\n", c.source.Kind, r.Version)
		if body == "" {
			body = "Substantive English translation is missing for this source version. Bilingual report delivery is incomplete."
		}
		fmt.Fprintf(&b, "%s\n\n## Source and delivery status\n\nBilingual content: %s; missing: %s. Model-authored prose and archiving do not prove execution, business acceptance or cleanup. This immutable version records the state at generation; current sources require a separate check.\n\n", body, r.TranslationStatus, strings.Join(r.MissingLanguages, ", "))
		fmt.Fprintf(&b, "Source run `%s`; report kind `%s`; business status `%s`; cleanup status `%s`; %d cited evidence records.\n\n", c.source.RunID, c.source.Kind, c.source.Business, c.source.Cleanup, len(c.source.Evidence))
	}
	// Machine values and original reproduction references are preserved in both
	// files. This is a citation manifest, not an archive of command output.
	manifest := map[string]any{"source_digest": r.SourceDigest, "document_digest": r.DocumentDigest, "run_id": c.source.RunID, "subject_id": c.source.SubjectID, "business_status": c.source.Business, "cleanup_status": c.source.Cleanup, "evidence": c.source.Evidence, "original_reproduction_steps": c.source.Reproduction}
	if len(c.source.Children) > 0 {
		refs := []map[string]any{}
		for _, child := range c.source.Children {
			refs = append(refs, map[string]any{"run_id": child.SourceRunID, "source_hash": child.SourceHash, "visit": child.Visit, "business_status": child.ResultStatus, "cleanup_status": child.CleanupStatus})
		}
		manifest["child_reports"] = refs
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	fmt.Fprintf(&b, "```json\n%s\n```\n", raw)
	return b.String()
}
