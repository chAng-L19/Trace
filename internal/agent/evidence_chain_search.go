package agent

// sapper-v2 ba3d3d49 (MIT), lib/sapper.py recall and lib/kb.py _brief:
// rank summary fields and fetch full detail on demand. Trace retains its
// existing Store, source verification and pinned immutable chain identities.
import (
	"fmt"
	"sort"
	"strings"
	"unicode"
)

type EvidenceChainSearchHit struct {
	SourceRunID  string `json:"source_run_id"`
	ChainID      string `json:"chain_id"`
	ChainHash    string `json:"chain_hash"`
	Name         string `json:"name"`
	Summary      string `json:"summary"`
	MethodID     string `json:"method_id"`
	Variant      string `json:"variant"`
	Verification string `json:"verification"`
	StepCount    int    `json:"step_count"`
}

func chainSearchHit(entry EvidenceChainEntry) EvidenceChainSearchHit {
	c := entry.Chain
	return EvidenceChainSearchHit{SourceRunID: entry.RunID, ChainID: c.ID, ChainHash: c.ContentHash, Name: chainSearchPreview(c.Name, 120), Summary: chainSearchPreview(c.Summary, 400), MethodID: c.MethodID, Variant: chainSearchPreview(c.Variant, 120), Verification: entry.Verification, StepCount: len(c.Steps)}
}

func chainSearchPreview(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit-1]) + "…"
	}
	return text
}

// Han characters remain individually searchable without a tokenizer dependency.
// Tokens are a set: repeated query words must not alter term scores.
func chainSearchTerms(text string) map[string]bool {
	terms := map[string]bool{}
	var word strings.Builder
	flush := func() {
		if word.Len() > 0 {
			terms[word.String()] = true
			word.Reset()
		}
	}
	for _, r := range strings.ToLower(text) {
		switch {
		case unicode.Is(unicode.Han, r):
			flush()
			terms[string(r)] = true
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
			word.WriteRune(r)
		default:
			flush()
		}
	}
	flush()
	return terms
}

func rankedEvidenceChains(entries []EvidenceChainEntry, query string) []EvidenceChainSearchHit {
	terms := chainSearchTerms(query)
	phrase := strings.ToLower(strings.TrimSpace(query))
	empty := phrase == ""
	type match struct {
		entry    EvidenceChainEntry
		priority int
		score    int
	}
	matched := []match{}
	for _, entry := range entries {
		c := entry.Chain
		name := strings.ToLower(strings.TrimSpace(c.Name))
		priority := 0
		if !empty {
			switch {
			case name == phrase:
				priority = 3
			case strings.Contains(name, phrase):
				priority = 2
			default:
				for _, field := range []string{c.Summary, c.Applicability, c.Lesson, c.Variant} {
					if strings.Contains(strings.ToLower(field), phrase) {
						priority = 1
						break
					}
				}
			}
		}
		title := chainSearchTerms(c.Name)
		body := chainSearchTerms(c.Summary + " " + c.Applicability + " " + c.Lesson + " " + c.Variant)
		score := 0
		for term := range terms {
			if title[term] {
				score += 2 // Explicit title weight, not duplication before set conversion.
			} else if body[term] {
				score++
			}
		}
		if empty || priority > 0 || score > 0 {
			matched = append(matched, match{entry: entry, priority: priority, score: score})
		}
	}
	sort.Slice(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if a.priority != b.priority {
			return a.priority > b.priority
		}
		if a.score != b.score {
			return a.score > b.score
		}
		if !a.entry.Chain.CreatedAt.Equal(b.entry.Chain.CreatedAt) {
			return a.entry.Chain.CreatedAt.After(b.entry.Chain.CreatedAt)
		}
		if a.entry.RunID != b.entry.RunID {
			return a.entry.RunID < b.entry.RunID
		}
		return a.entry.Chain.ID < b.entry.Chain.ID
	})
	hits := make([]EvidenceChainSearchHit, 0, len(matched))
	for _, m := range matched {
		hits = append(hits, chainSearchHit(m.entry))
	}
	return hits
}

func (s *Store) SearchEvidenceChains(query string, limit int) ([]EvidenceChainSearchHit, int, error) {
	return s.searchEvidenceChains(query, limit, false)
}

func (s *Store) searchEvidenceChains(query string, limit int, workerOnly bool) ([]EvidenceChainSearchHit, int, error) {
	if limit == 0 {
		limit = 10
	}
	if limit < 1 || limit > 50 {
		return nil, 0, fmt.Errorf("limit must be 1..50")
	}
	unlock, err := s.acquireStoreLock()
	if err != nil {
		return nil, 0, err
	}
	defer unlock()
	ids, err := s.snapshotIDsLocked()
	if err != nil {
		return nil, 0, err
	}
	// ponytail: one metadata scan; bound expensive source verification to hits.
	// A same-Store rebuildable directory is warranted only by measured scale.
	entries := []EvidenceChainEntry{}
	for _, id := range ids {
		owner, err := s.loadRawLocked(id)
		if err != nil {
			return nil, 0, err
		}
		for _, c := range owner.EvidenceChains {
			if workerOnly && !workerMethodRelevant(query, c.Name+" "+c.Summary+" "+c.Applicability+" "+c.Lesson+" "+c.Variant) {
				continue
			}
			entries = append(entries, EvidenceChainEntry{RunID: id, Chain: c})
		}
	}
	hits := rankedEvidenceChains(entries, query)
	total := len(hits)
	hits = hits[:min(limit, total)]
	for i, hit := range hits {
		entry, err := s.chainLocked(hit.SourceRunID, hit.ChainID, false)
		if err != nil {
			return nil, 0, err
		}
		hits[i] = chainSearchHit(entry)
	}
	return hits, total, nil
}
