package agent

import (
	"fmt"
	"strings"
	"unicode"
)

// Presentation contracts consulted: Muteki EvidenceChain.tsx (144df766,
// AGPL-3.0: concise summary with original text disclosure), ARTEX
// db/exploration.go, db/findings.go and report/findings.go (e6ec5699,
// AGPL-3.0: activity remains separate from explicitly bound finding evidence).
// Independent adaptation to Trace's existing snapshot; no verifier is added.

// ReportEvidenceLink identifies a report section citing this execution record.
// Status describes that section, not semantic proof or evidence integrity.
// IDs are local to the catalog entry's RunID, as are all report evidence refs.
type ReportEvidenceLink struct {
	Kind            string `json:"kind"`
	ID              string `json:"id"`
	Title           string `json:"title"`
	Summary         string `json:"summary"`
	Status          string `json:"status"`
	OriginalTitle   string `json:"original_title,omitempty"`
	OriginalSummary string `json:"original_summary,omitempty"`
}

func reportEvidenceLinks(s *Snapshot, evidenceID string) []ReportEvidenceLink {
	links := []ReportEvidenceLink{}
	if evidenceID == "" {
		return links
	}
	seen := map[string]bool{}
	add := func(kind, id, title, summary, fallbackTitle, fallbackSummary, status string, ids []string, refs []EvidenceReference) {
		linked := hasString(ids, evidenceID)
		for _, ref := range refs {
			linked = linked || ref.ID == evidenceID
		}
		key := kind + "\x00" + id
		if !linked || seen[key] {
			return
		}
		seen[key] = true
		link := ReportEvidenceLink{Kind: kind, ID: id, Status: status}
		link.Title, link.OriginalTitle = reportEvidenceText(title, fallbackTitle, 80)
		link.Summary, link.OriginalSummary = reportEvidenceText(summary, fallbackSummary, 180)
		links = append(links, link)
	}
	add("result", "result", "", s.Result.Summary, "研究结论", "此记录被研究结论直接引用；引用关系不代表验证通过。", s.Result.Status, s.Result.EvidenceIDs, nil)
	for i, answer := range s.Result.Answers {
		status := "unverified"
		if answer.Verified {
			status = "verified"
		}
		title, summary := answer.Title, answer.Summary
		if title == "" {
			title = answer.Kind
		}
		if summary == "" {
			summary = answer.Value
		}
		fallbackTitle := fmt.Sprintf("结论条目 %d", i+1)
		if answer.Kind == "flag" {
			fallbackTitle = "目标结果标记"
		}
		fallbackSummary, _ := reportEvidenceText(s.Result.Summary, "此记录用于支撑报告中的一个结论条目，原文可在详情查看。", 180)
		add("answer", fmt.Sprintf("answer:%d", i+1), title, summary, fallbackTitle, fallbackSummary, status, answer.EvidenceIDs, nil)
	}
	for _, finding := range s.Findings {
		add("finding", finding.ID, finding.Title, finding.RootCause, "发现："+finding.ID, "此记录关联发现的原始观察；审阅与复测状态分别记录。", finding.Status, finding.EvidenceIDs, finding.References)
		for i, retest := range finding.Retests {
			add("retest", fmt.Sprintf("%s:retest:%d", finding.ID, i+1), "", retest.Reason, fmt.Sprintf("发现复测：%s · %d", finding.ID, i+1), "此记录关联一次实际复测，保留当次结果及失败原因。", retest.Status, retest.EvidenceIDs, retest.References)
		}
	}
	// Match the report's latest check for each currently declared requirement.
	// Historical checks remain in the ledger, not in the current check links.
	for _, requirement := range s.Requirements {
		if check := latestCheck(s, requirement.ID); check != nil {
			add("check", requirement.ID, requirement.Description, check.Reason, "验收检查："+requirement.ID, "此记录关联该验收项的最近一次检查，检查状态以当前记录为准。", check.Status, check.EvidenceIDs, check.References)
		}
	}
	// PlanTask.EvidenceRunID and Asset.SourceRunID are execution dependencies,
	// not report citations. Never join their bare IDs to this run's evidence.
	return links
}

// English/raw text stays available verbatim; the fallback is a description of
// the citation's role, not an invented translation or a new scientific claim.
func reportEvidenceText(raw, fallback string, limit int) (string, string) {
	text := strings.Join(strings.Fields(raw), " ")
	if !strings.ContainsFunc(text, func(r rune) bool { return unicode.Is(unicode.Han, r) }) {
		text = fallback
	}
	if runes := []rune(text); len(runes) > limit {
		text = string(runes[:limit]) + "…"
	}
	if text != raw {
		return text, raw
	}
	return text, ""
}

func reportStatus(status string) string {
	labels := map[string]string{
		"success": "成功", "incomplete": "未完成", "failed": "失败", "cancelled": "已取消",
		"completed": "已完成", "unknown": "未知", "pass": "通过", "fail": "未通过",
		"verified": "来源校验通过", "unverified": "未通过来源校验", "invalid": "已失效",
		"submitted": "已提交", "reviewed": "已审阅", "reproduced": "已复现",
		"rejected": "已驳回", "needs_evidence": "待补证据",
		"open": "待处理", "accepted": "已接受", "fixed": "已修复", "dismissed": "已忽略",
	}
	if label, ok := labels[status]; ok {
		return label + "（" + status + "）"
	}
	if status == "" {
		return "未记录"
	}
	return "其他状态（" + status + "）"
}
