package agent

import (
	"fmt"
	"strings"
)

type Asset struct {
	ParentID     string   `json:"parent_id,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	SourceRunID  string   `json:"source_run_id,omitempty"`
	SourceTaskID string   `json:"source_task_id,omitempty"`
	ID           string   `json:"id"`
	Kind         string   `json:"kind"`
	Value        string   `json:"value"`
	Coverage     string   `json:"coverage"`
	EvidenceIDs  []string `json:"evidence_ids,omitempty"`
	Predicate    string   `json:"predicate,omitempty"`
}
type AssetRelation struct {
	From        string   `json:"from"`
	To          string   `json:"to"`
	Kind        string   `json:"kind"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

func (a *Agent) UpsertAsset(id string, asset Asset) (Snapshot, error) {
	var source *Snapshot
	if asset.SourceRunID != "" && asset.SourceRunID != id {
		child, err := a.store.Load(asset.SourceRunID)
		if err != nil {
			return Snapshot{}, err
		}
		source = &child
	}

	return a.store.Update(id, func(s *Snapshot) error {
		evidenceSource := s
		if source != nil {
			registered := false
			for _, w := range s.Workers {
				if w.RunID == asset.SourceRunID {
					registered = true
					asset.SourceTaskID = w.TaskID
				}
			}
			if !registered {
				return fmt.Errorf("evidence source is not a registered child run")
			}
			evidenceSource = source
		}
		if asset.ID == "" || asset.Kind == "" || asset.Value == "" {
			return fmt.Errorf("asset id, kind and value required")
		}
		if asset.Coverage == "" {
			asset.Coverage = "discovered"
		}
		switch asset.Coverage {
		case "discovered", "in_progress", "observed", "verified", "negative", "uncertain", "failed":
		default:
			return fmt.Errorf("invalid coverage")
		}
		if asset.Coverage == "observed" || asset.Coverage == "verified" || asset.Coverage == "negative" {
			if len(asset.EvidenceIDs) == 0 {
				return fmt.Errorf("coverage needs evidence")
			}
			matched := false
			for _, ev := range asset.EvidenceIDs {
				out, ok := verifiedOutput(evidenceSource, ev)
				if !ok {
					return fmt.Errorf("invalid execution evidence %s", ev)
				}
				if asset.Predicate != "" && strings.Contains(out, asset.Predicate) {
					matched = true
				}
			}
			if (asset.Coverage == "verified" || asset.Coverage == "negative") && !matched {
				return fmt.Errorf("verified coverage needs matching business predicate")
			}
		}
		if asset.Scope == "" {
			asset.Scope = "included"
		}
		if asset.Scope != "included" && asset.Scope != "excluded" {
			return fmt.Errorf("scope must be included or excluded")
		}
		if asset.SourceRunID == "" {
			asset.SourceRunID = id
		}
		if asset.ParentID != "" {
			parents := map[string]string{}
			for _, a := range s.Assets {
				parents[a.ID] = a.ParentID
			}
			if _, ok := parents[asset.ParentID]; !ok {
				return fmt.Errorf("unknown parent asset")
			}
			for parent := asset.ParentID; parent != ""; parent = parents[parent] {
				if parent == asset.ID {
					return fmt.Errorf("asset parent cycle")
				}
			}
		}
		for i := range s.Assets {
			if s.Assets[i].ID == asset.ID {
				s.Assets[i] = asset
				addEvent(s, "asset_updated", "资产覆盖已更新", asset)
				return nil
			}
		}
		s.Assets = append(s.Assets, asset)
		addEvent(s, "asset_added", "资产已保存", asset)
		return nil
	})
}
func (a *Agent) RelateAssets(id string, r AssetRelation) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		found := map[string]bool{}
		for _, asset := range s.Assets {
			found[asset.ID] = true
		}
		if !found[r.From] || !found[r.To] || r.Kind == "" {
			return fmt.Errorf("relation requires existing assets and kind")
		}
		for _, ev := range r.EvidenceIDs {
			if _, ok := verifiedOutput(s, ev); !ok {
				return fmt.Errorf("invalid relation evidence %s", ev)
			}
		}
		for i := range s.Relations {
			old := s.Relations[i]
			if old.From == r.From && old.To == r.To && old.Kind == r.Kind {
				s.Relations[i] = r
				addEvent(s, "relation_updated", "资产关系已更新", r)
				return nil
			}
		}
		s.Relations = append(s.Relations, r)
		addEvent(s, "relation_added", "资产关系已保存", r)
		return nil
	})
}

// Denominator counts explicitly included assets, including untested discoveries.
// Verified and negative require execution evidence; observed alone is not tested.
type CoverageCount struct {
	Total     int            `json:"total"`
	Tested    int            `json:"tested"`
	Verified  int            `json:"verified"`
	Negative  int            `json:"negative"`
	Uncovered int            `json:"uncovered"`
	ByStatus  map[string]int `json:"by_status"`
	Backlog   []string       `json:"backlog"`
}
type CoverageSummary struct {
	CoverageCount
	ByParent map[string]*CoverageCount `json:"by_parent"`
	Excluded int                       `json:"excluded"`
}

func Coverage(s Snapshot) CoverageSummary {
	result := CoverageSummary{CoverageCount: CoverageCount{ByStatus: map[string]int{}, Backlog: []string{}}, ByParent: map[string]*CoverageCount{}}
	assets := map[string]Asset{}
	for _, a := range s.Assets {
		assets[a.ID] = a
	}
	add := func(c *CoverageCount, a Asset) {
		c.Total++
		c.ByStatus[a.Coverage]++
		switch a.Coverage {
		case "verified":
			c.Verified++
			c.Tested++
		case "negative":
			c.Negative++
			c.Tested++
		default:
			c.Uncovered++
			c.Backlog = append(c.Backlog, a.ID)
		}
	}
	for _, asset := range s.Assets {
		if asset.Scope == "excluded" {
			result.Excluded++
			continue
		}
		add(&result.CoverageCount, asset)
		seen := map[string]bool{}
		for parent := asset.ID; parent != "" && !seen[parent]; parent = assets[parent].ParentID {
			seen[parent] = true
			if result.ByParent[parent] == nil {
				result.ByParent[parent] = &CoverageCount{ByStatus: map[string]int{}, Backlog: []string{}}
			}
			add(result.ByParent[parent], asset)
		}
	}
	return result
}
