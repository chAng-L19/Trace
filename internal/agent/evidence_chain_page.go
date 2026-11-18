package agent

import (
	"fmt"
	"sort"
	"strings"
)

type EvidenceChainPage struct {
	Items      []EvidenceChainEntry `json:"items"`
	Total      int                  `json:"total"`
	NextBefore string               `json:"next_before,omitempty"`
}

// EvidenceChainPage verifies only the selected page. Metadata is a locator,
// never a verification hint, and ReferencedBy is reserved for explicit detail.
// ponytail: scan existing snapshots until measurements justify a rebuildable
// same-Store directory; no new persistent index or competing source of truth.
func (s *Store) EvidenceChainPage(runID string, scoped bool, query, before string, limit int) (EvidenceChainPage, error) {
	if limit < 1 || limit > 100 {
		return EvidenceChainPage{}, fmt.Errorf("limit must be 1..100")
	}
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return EvidenceChainPage{}, err
	}
	defer unlock()
	rows := []EvidenceChainEntry{}
	seen := map[string]bool{}
	add := func(owner Snapshot, chain EvidenceChain, ref *EvidenceChainReference) {
		row := EvidenceChainEntry{RunID: owner.Run.ID, RunGoal: owner.Run.Goal, RunDeleted: owner.Run.Deleted, Chain: chain}
		row.Key = row.RunID + "/" + chain.ID + "/" + chain.ContentHash
		if ref != nil {
			row.RunID, row.Reference, row.ExpectedHash, row.Purpose = ref.SourceRunID, true, ref.ChainHash, ref.Purpose
			row.Key = ref.SourceRunID + "/" + ref.ChainID + "/" + ref.ChainHash
		}
		if !seen[row.Key] {
			rows = append(rows, row)
			seen[row.Key] = true
		}
	}
	if scoped {
		owner, err := s.loadRawLocked(runID)
		if err != nil {
			return EvidenceChainPage{}, err
		}
		for _, c := range owner.EvidenceChains {
			add(owner, c, nil)
		}
		for _, ref := range owner.EvidenceChainRefs {
			source, err := s.loadRawLocked(ref.SourceRunID)
			if err == nil {
				found := false
				for _, c := range source.EvidenceChains {
					if c.ID == ref.ChainID {
						add(source, c, &ref)
						found = true
						break
					}
				}
				if found {
					continue
				}
			}
			add(Snapshot{Run: Run{ID: ref.SourceRunID}}, EvidenceChain{ID: ref.ChainID, Name: "来源链路缺失", Summary: "固定引用保留，来源记录当前不可读取。", Steps: []EvidenceChainStep{}}, &ref)
		}
	} else {
		ids, err := s.snapshotIDsLocked()
		if err != nil {
			return EvidenceChainPage{}, err
		}
		for _, id := range ids {
			owner, err := s.loadRawLocked(id)
			if err != nil {
				return EvidenceChainPage{}, err
			}
			for _, c := range owner.EvidenceChains {
				add(owner, c, nil)
			}
		}
	}
	phrase := strings.ToLower(strings.TrimSpace(query))
	filtered := rows[:0]
	for _, row := range rows {
		c := row.Chain
		if phrase == "" || strings.Contains(strings.ToLower(strings.Join([]string{c.Name, c.Summary, c.MethodID, c.Variant, c.Applicability, c.Lesson, row.RunGoal, row.Purpose}, " ")), phrase) {
			filtered = append(filtered, row)
		}
	}
	rows = filtered
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].Chain.CreatedAt.Equal(rows[j].Chain.CreatedAt) {
			return rows[i].Chain.CreatedAt.After(rows[j].Chain.CreatedAt)
		}
		return rows[i].Key > rows[j].Key
	})
	start := 0
	if before != "" {
		start = -1
		for i := range rows {
			if rows[i].Key == before {
				start = i + 1
				break
			}
		}
		if start < 0 {
			return EvidenceChainPage{}, fmt.Errorf("unknown before cursor")
		}
	}
	page := EvidenceChainPage{Items: []EvidenceChainEntry{}, Total: len(rows)}
	end := min(len(rows), start+limit)
	for _, row := range rows[start:end] {
		entry, err := s.chainLocked(row.RunID, row.Chain.ID, false)
		if err != nil {
			entry = row
			entry.Verification, entry.SourceStatus, entry.SourceReason = "invalid", "missing", "固定来源会话或链路缺失；未替换为最新版本"
		}
		entry.Key, entry.Reference, entry.ExpectedHash, entry.Purpose = row.Key, row.Reference, row.ExpectedHash, row.Purpose
		if err == nil && row.Reference && entry.Chain.ContentHash != row.ExpectedHash {
			entry.Verification, entry.SourceStatus, entry.SourceReason = "invalid", "changed", "链路与固定引用 hash 不一致；未替换为最新版本"
		}
		page.Items = append(page.Items, entry)
	}
	if end < len(rows) && end > start {
		page.NextBefore = rows[end-1].Key
	}
	return page, nil
}
