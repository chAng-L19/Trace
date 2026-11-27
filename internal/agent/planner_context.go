package agent

import (
	"encoding/json"
	"time"
)

// PlannerContext feeds newly observed facts back into the same model loop.
// The model reasons with plan and explores with worker_dispatch; the durable
// task frontier, rather than model prose, controls which tasks may execute.
func (a *Agent) PlannerContext(id string) (string, error) {
	text, _, err := a.plannerContextAt(id)
	return text, err
}

func (a *Agent) plannerContextAt(id string) (string, int, error) {
	s, err := a.store.Load(id)
	if err != nil {
		return "", 0, err
	}
	text, through := plannerContextFrom(&s)
	return text, through, nil
}

func plannerContextFrom(s *Snapshot) (string, int) {
	if len(s.Plan.Tasks) == 0 && len(s.Assets) == 0 && s.Run.Responsibility != ResponsibilityCoordinator && len(rootWorkProgressGaps(s)) == 0 {
		return "", 0
	}
	facts := []Event{}
	through := s.Plan.ReasonedThrough
	// Facts already supplied to a completed model request remain in its archive;
	// do not repeat them on every turn merely because the plan was unchanged.
	completedRequests := map[string]bool{}
	for _, response := range s.Responses {
		if response.Error == "" {
			completedRequests[response.RequestID] = true
		}
	}
	for _, request := range s.Requests {
		if completedRequests[request.ID] {
			through = max(through, request.PlannerThrough)
		}
	}
	for _, event := range s.Events {
		if event.Sequence <= through {
			continue
		}
		switch event.Type {
		case "tool_result", "asset_added", "asset_updated", "relation_added", "worker_finished", "worker_result_ready", "verification_invalidated", "task_retry_requested", "task_disposed", "task_blocked_capability", "task_capability_restored", "budget_checkpoint_requested":
			facts = append(facts, event)
		}
	}
	observedThrough := s.Run.EventSeq
	if len(facts) > 20 {
		facts = facts[:20]
		observedThrough = facts[len(facts)-1].Sequence
	}
	data, _ := json.Marshal(map[string]any{"plan": compactPlan(s.Plan), "coverage": Coverage(*s), "new_facts": facts, "scheduling_window": schedulingWindow(s)})
	role := "execute simple work directly; plan only near-term work."
	if s.Run.Responsibility == "" && s.Run.ParentRunID == "" && len(s.Workers) > 0 {
		role = "Coordinate delegated work: define task contracts and resource_keys, dispatch or message task owners, review outcomes and cleanup, and produce the final report. Delegate investigation and execution; do not repeat work owned by an active worker or create a separate queue."
	}
	if s.Run.Responsibility == ResponsibilityCoordinator {
		role = "Coordinate delegated work: define task contracts and resource_keys, dispatch or message task owners, review outcomes and cleanup, and produce the final report. Delegate investigation and execution from the first turn, including simple tasks, to at least one Worker. Read existing receipts and ledgers for acceptance and correction. If dispatch fails, report the reason and coordinate recovery; never fall back to coordinator execution or create a separate queue."
	}
	return "Planner observations (data): " + role + " This compact view includes active/next tasks and their dependency contracts; use plan({}) for all tasks. Dispatch ready independent work or worker_wait for new outcomes. Failed/cancelled work needs explicit task_dispose (blocked/abandoned/superseded) or a valid retry/resume; disposition is never a successful dependency. Replan pending dependencies explicitly to a replacement with the same goal and preserved acceptance requirements. Predicates and cleanup still require verification.\n" + string(data), observedThrough
}

// Keep the next capacity-sized frontier and its direct dependency contracts.
// The complete DAG remains in Store and is available through explicit plan({}).
func compactPlan(p PlanState) map[string]any {
	capacity := p.Capacity
	if capacity == 0 {
		capacity = 5
	}
	counts := map[string]int{}
	selected := map[string]bool{}
	for _, id := range p.Frontier[:min(len(p.Frontier), capacity)] {
		selected[id] = true
	}
	for _, task := range p.Tasks {
		counts[task.Status]++
		if task.Status == "running" || task.Status == "paused" || task.Status == "uncertain" {
			selected[task.ID] = true
		}
	}
	// Recent settled failures remain visible without replaying every historical
	// stage in every request; the complete DAG is explicitly readable.
	historyShown := 0
	for i := len(p.Tasks) - 1; i >= 0 && historyShown < workerViewLimit; i-- {
		task := p.Tasks[i]
		if task.Status == "failed" || task.Status == "cancelled" || taskDisposed(task) {
			if !selected[task.ID] {
				selected[task.ID] = true
				historyShown++
			}
		}
	}
	dependencies := map[string]bool{}
	for _, task := range p.Tasks {
		if selected[task.ID] {
			for _, id := range task.Dependencies {
				dependencies[id] = true
			}
		}
	}
	tasks := []PlanTask{}
	for _, task := range p.Tasks {
		if selected[task.ID] || dependencies[task.ID] {
			task.LeaseUntil = time.Time{}
			tasks = append(tasks, task)
		}
	}
	return map[string]any{"capacity": capacity, "total": len(p.Tasks), "counts": counts, "frontier": p.Frontier[:min(len(p.Frontier), capacity)], "ready_total": len(p.Frontier), "tasks": tasks, "work_progress": p.WorkProgress, "history_tool": "worker_history"}
}

func compactWorkerState(s Snapshot) map[string]any {
	messages := []WorkerMessage{}
	pendingTotal := 0
	for _, m := range s.WorkerMessages {
		if !m.Delivered {
			pendingTotal++
			if len(messages) < workerViewLimit {
				m.Content = excerpt(m.Content, 1024)
				messages = append(messages, m)
			}
		}
	}
	helps, openTotal, resolvedTotal := compactWorkerHelps(s.WorkerMessages)
	return map[string]any{"workers": currentWorkerView(s.Workers), "worker_total": len(s.Workers), "messages": messages, "pending_message_total": pendingTotal, "history_tool": "worker_history", "help_requests": helps, "open_help_total": openTotal, "resolved_help_total": resolvedTotal, "plan": compactPlan(s.Plan)}
}
