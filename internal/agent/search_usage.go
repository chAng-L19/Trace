package agent

import "fmt"

// Conservative reservations survive restart and an unknown upstream outcome.
// ResultSlots bounds the requested upstream rows, not just normalized survivors.
type SearchSourceUsage struct {
	Requests    int `json:"requests"`
	ResultSlots int `json:"result_slots"`
}

func (a *Agent) reserveSearchUsage(runID string, request SourceSearchRequest, config SearchSourceConfig) (bool, error) {
	if runID == "" {
		return false, fmt.Errorf("search acquisition requires a persisted run")
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	unlock, err := a.store.acquireStoreLock()
	if err != nil {
		return false, err
	}
	defer unlock()
	source, err := a.store.loadRawLocked(runID)
	if err != nil {
		return false, err
	}
	if source.Run.Deleted {
		return false, fmt.Errorf("search quota run is deleted")
	}
	root, _, err := a.resolveTreeRootLocked(&source)
	if err != nil {
		return false, err
	}
	admitted := false
	_, err = a.store.updateLocked(root.Run.ID, func(s *Snapshot) error {
		if s.Run.Deleted {
			return fmt.Errorf("search quota root is deleted")
		}
		usage := s.SearchUsage[request.Source]
		if usage.Requests >= config.MaxRequests || usage.ResultSlots+request.PageSize > config.MaxResults {
			return nil
		}
		if s.SearchUsage == nil {
			s.SearchUsage = map[string]SearchSourceUsage{}
		}
		usage.Requests++
		usage.ResultSlots += request.PageSize
		s.SearchUsage[request.Source] = usage
		addEvent(s, "search_quota_reserved", "数据源调用额度已预留", map[string]any{"source": request.Source, "run_id": runID, "requests": usage.Requests, "result_slots": usage.ResultSlots})
		admitted = true
		return nil
	})
	return admitted && err == nil, err
}
