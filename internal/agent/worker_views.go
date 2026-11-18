package agent

import "fmt"

const workerViewLimit = 20

type WorkerHistoryQuery struct {
	Kind     string `json:"kind"`
	Offset   int    `json:"offset"`
	Limit    int    `json:"limit"`
	OpenHelp bool   `json:"open_help,omitempty"`
	Parent   bool   `json:"parent,omitempty"`
}

type WorkerHistoryPage struct {
	Kind       string `json:"kind"`
	Total      int    `json:"total"`
	NextOffset int    `json:"next_offset"`
	Items      any    `json:"items"`
}

// History is a read-only page over this Run, never a second inbox or queue.
func (a *Agent) workerHistory(id string, query WorkerHistoryQuery) (WorkerHistoryPage, error) {
	if query.Offset < 0 || query.Limit < 0 || query.Limit > 50 {
		return WorkerHistoryPage{}, fmt.Errorf("offset must be nonnegative; limit must be 0..50")
	}
	if query.Limit == 0 {
		query.Limit = workerViewLimit
	}
	if query.OpenHelp && query.Kind != "messages" {
		return WorkerHistoryPage{}, fmt.Errorf("open_help requires messages history")
	}
	s, err := a.store.Load(id)
	if err != nil {
		return WorkerHistoryPage{}, err
	}
	if query.Parent {
		if s.Run.ParentRunID == "" || s.Run.ParentTaskID == "" {
			return WorkerHistoryPage{}, fmt.Errorf("run has no registered parent")
		}
		parent, err := a.store.Load(s.Run.ParentRunID)
		if err != nil {
			return WorkerHistoryPage{}, err
		}
		registered := false
		for _, w := range parent.Workers {
			if w.RunID == id && w.TaskID == s.Run.ParentTaskID {
				registered = true
			}
		}
		if !registered || parent.Run.Deleted {
			return WorkerHistoryPage{}, fmt.Errorf("parent worker identity is unavailable")
		}
		s = parent
	}
	page := WorkerHistoryPage{Kind: query.Kind}
	bounds := func(total int) (int, int) {
		page.Total = total
		start := min(query.Offset, total)
		end := start + min(query.Limit, total-start)
		page.NextOffset = end
		return start, end
	}
	switch query.Kind {
	case "messages":
		messages := s.WorkerMessages
		if query.OpenHelp {
			messages = []WorkerMessage{}
			for _, help := range WorkerHelps(s.WorkerMessages) {
				if help.ResolutionID == "" {
					messages = append(messages, help.Message)
				}
			}
		}
		start, end := bounds(len(messages))
		page.Items = messages[start:end]
	case "workers":
		start, end := bounds(len(s.Workers))
		page.Items = s.Workers[start:end]
	case "tasks":
		start, end := bounds(len(s.Plan.Tasks))
		page.Items = s.Plan.Tasks[start:end]
	default:
		return WorkerHistoryPage{}, fmt.Errorf("kind must be messages, workers or tasks")
	}
	return page, nil
}

// Prefer live/unsettled workers, then recent history. Counts and history keep
// every omitted record reachable without repeating the entire run each turn.
func currentWorkerView(workers []AgentWorker) []AgentWorker {
	out := make([]AgentWorker, 0, min(workerViewLimit, len(workers)))
	selected := map[string]bool{}
	for i := len(workers) - 1; i >= 0 && len(out) < workerViewLimit; i-- {
		w := workers[i]
		if w.Status == "running" || w.Status == "uncertain" || w.CleanupStatus != "completed" {
			out = append(out, w)
			selected[w.ID] = true
		}
	}
	for i := len(workers) - 1; i >= 0 && len(out) < workerViewLimit; i-- {
		if !selected[workers[i].ID] {
			out = append(out, workers[i])
		}
	}
	return out
}
