package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type automaticConclusion struct {
	Kind, Subject, Conclusion, Condition, Status string
	IDs                                          []string
	References                                   []EvidenceReference
}

// Automatic organization projects explicit conclusions and references; it
// never promotes prose, tool output or a chain into new execution proof.
func archiveConclusionChains(s *Snapshot) {
	if s.Run.Deleted {
		return
	}
	candidates := []automaticConclusion{}
	add := func(c automaticConclusion) {
		if strings.TrimSpace(c.Conclusion) != "" && len(c.IDs) > 0 {
			candidates = append(candidates, c)
		}
	}
	// ponytail: latest 32 findings are automatic candidates; older findings and
	// full paths remain available through explicit organization.
	for _, f := range s.Findings[max(0, len(s.Findings)-32):] {
		condition := ""
		if f.Predicate.Kind != "" {
			raw, _ := json.Marshal(f.Predicate)
			condition = string(raw)
		}
		add(automaticConclusion{"finding", f.ID, f.Title + "\n" + f.RootCause + "\n" + f.Impact, condition, f.Status, f.EvidenceIDs, f.References})
	}
	if cp := s.WorkerCheckpoint; cp != nil {
		refs := []EvidenceReference{}
		for _, id := range cp.EvidenceIDs {
			if h := cp.EvidenceHashes[id]; h != "" {
				refs = append(refs, EvidenceReference{ID: id, Hash: h})
			}
		}
		progress := cp.Progress
		if strings.TrimSpace(progress) == "" {
			progress = cp.Summary
		}
		add(automaticConclusion{"stage", "checkpoint", progress, strings.TrimSpace(cp.Subject + " " + cp.Instance), "checkpoint", cp.EvidenceIDs, refs})
	}
	if s.Run.Status == StatusCompleted || s.Run.Status == StatusFailed || s.Run.Status == StatusCancelled || reportDeliveryPending(s) || s.Run.PauseReason == "report_translation_missing" {
		add(automaticConclusion{"conclusion", "result", s.Result.Summary, "", s.Result.Status, s.Result.EvidenceIDs, nil})
	}
	if len(candidates) == 0 {
		return
	}
	// Unchanged milestones need no event/evidence scan. A Result carries only
	// IDs: freeze its first hashes instead of rebinding it to rewritten evidence.
	known := map[string]bool{}
	for _, c := range s.EvidenceChains {
		if strings.HasPrefix(c.MethodID, "auto-") {
			known[c.Variant] = true
		}
	}
	pending := candidates[:0]
	for _, c := range candidates {
		if !known["conclusion-"+valueHash(c)] {
			pending = append(pending, c)
		}
	}
	if len(pending) == 0 {
		return
	}
	// Creation receipts survive deletion and suppress recreation after restart.
	// Only previously unseen conclusions pay for this event-history scan.
	for _, e := range s.Events {
		if e.Type != "evidence_chain_created" {
			continue
		}
		raw, _ := json.Marshal(e.Data)
		var receipt struct {
			Automatic      bool   `json:"automatic"`
			ConclusionHash string `json:"conclusion_hash"`
		}
		if json.Unmarshal(raw, &receipt) == nil && receipt.Automatic && receipt.ConclusionHash != "" {
			known["conclusion-"+receipt.ConclusionHash] = true
		}
	}
	needed := map[string]bool{}
	for _, c := range pending {
		if !known["conclusion-"+valueHash(c)] {
			for _, id := range c.IDs {
				needed[id] = true
			}
		}
	}
	if len(needed) == 0 {
		return
	}
	evidence := map[string]Evidence{}
	for _, e := range s.Evidence {
		if needed[e.ID] {
			evidence[e.ID] = e
		}
	}
	for _, candidate := range pending {
		semanticHash := valueHash(candidate)
		variant := "conclusion-" + semanticHash
		if known[variant] {
			continue
		}
		pinned := map[string]string{}
		for _, ref := range candidate.References {
			pinned[ref.ID] = ref.Hash
		}
		steps := []EvidenceChainStep{}
		seen := map[string]bool{}
		component, valid, total := "", true, 0
		for _, id := range candidate.IDs {
			if seen[id] {
				continue
			}
			seen[id] = true
			e, ok := evidence[id]
			// Missing and changed pinned sources are not replaced or silently dropped.
			if !ok || e.ContentHash == "" || verifyEvidence(s, &e) == "invalid" || len(candidate.References) > 0 && pinned[id] != e.ContentHash {
				valid = false
				break
			}
			total++
			if e.Scope != nil {
				parts := []string{}
				for _, part := range []string{e.Scope.ObjectID, e.Scope.Version} {
					if part = strings.TrimSpace(part); part != "" {
						parts = append(parts, part)
					}
				}
				if scope := strings.Join(parts, " @ "); scope != "" {
					if component == "" {
						component = scope
					} else if component != scope {
						component = s.Run.Goal
					}
				}
			}
			if len(steps) < 64 {
				steps = append(steps, EvidenceChainStep{RunID: s.Run.ID, EvidenceID: id, ContentHash: e.ContentHash, Label: "引用来源 · " + e.Type})
			}
		}
		if !valid || len(steps) == 0 {
			continue
		}
		if component == "" {
			component = s.Run.Goal
		}
		condition, status := candidate.Condition, candidate.Status
		if condition == "" {
			condition = "本次运行 " + s.Run.ID
		}
		if status == "" {
			status = "unknown"
		}
		c := EvidenceChain{
			Name:    chainSearchPreview(component, 56) + " · " + chainSearchPreview(condition, 40) + " · " + chainSearchPreview(candidate.Conclusion, 80) + " · " + status,
			Summary: chainSearchPreview(candidate.Conclusion, 1600) + fmt.Sprintf("\n固定引用 %d/%d；仅整理原始来源，来源验证、业务结论与清理分别判定。", len(steps), total),
			Steps:   steps, MethodID: "auto-" + valueHash([]string{s.Run.ID, candidate.Kind, candidate.Subject}),
			Variant: variant, Applicability: chainSearchPreview(condition, 800), Lesson: "结论类型：" + candidate.Kind + "；记录状态：" + status,
		}
		c.ContentHash, c.CreatedAt = chainHash(c), time.Now().UTC()
		c.ID = "chain-" + c.ContentHash
		s.EvidenceChains = append(s.EvidenceChains, c)
		known[variant] = true
		addEvent(s, "evidence_chain_created", "已自动整理结论的固定引用链", map[string]any{"chain_id": c.ID, "automatic": true, "conclusion_hash": semanticHash, "subject_kind": candidate.Kind, "subject_id": candidate.Subject, "steps": len(steps), "reference_count": total})
	}
}
