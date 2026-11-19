package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

type PlanTask struct {
	// LocalExecution is server-owned: one task may reuse its live child executor.
	LocalExecution     bool     `json:"local_execution,omitempty"`
	LocalWorkerVisit   uint64   `json:"local_worker_visit,omitempty"`
	ExecutionRole      string   `json:"execution_role,omitempty"`
	ContextEvidenceIDs []string `json:"context_evidence_ids,omitempty"`
	CompletionCriteria string   `json:"completion_criteria,omitempty"`
	// ModelBinding is the server-owned effective route frozen at first claim.
	ModelBinding       *ModelSelection           `json:"model_binding,omitempty"`
	Stage              bool                      `json:"stage,omitempty"`
	EvidenceScope      *EvidenceScope            `json:"evidence_scope,omitempty"`
	ModelSelection     *ModelSelection           `json:"model_selection,omitempty"`
	RequiredImageInput bool                      `json:"required_image_input,omitempty"`
	HandoffTaskIDs     []string                  `json:"handoff_task_ids,omitempty"`
	ResourceKeys       []string                  `json:"resource_keys,omitempty"`
	RequiredToolIDs    []string                  `json:"required_tool_ids,omitempty"`
	RequiredAssetIDs   []string                  `json:"required_asset_ids,omitempty"`
	ExpectedArtifacts  []string                  `json:"expected_artifacts,omitempty"`
	BlockedReason      *TaskBlockedError         `json:"blocked_reason,omitempty"`
	InputAttachmentIDs []string                  `json:"input_attachment_ids,omitempty"`
	Priority           int                       `json:"priority,omitempty"`
	EstimatedSeconds   int64                     `json:"estimated_seconds,omitempty"`
	ExpectedProgress   string                    `json:"expected_progress,omitempty"`
	Mode               string                    `json:"mode,omitempty"`
	ID                 string                    `json:"id"`
	Goal               string                    `json:"goal"`
	Dependencies       []string                  `json:"dependencies,omitempty"`
	AssetIDs           []string                  `json:"asset_ids,omitempty"`
	SuccessContains    string                    `json:"success_contains,omitempty"`
	Requirements       []VerificationRequirement `json:"requirements,omitempty"`
	Started            bool                      `json:"started,omitempty"`
	DispositionReason  string                    `json:"disposition_reason,omitempty"`
	ReplacementTaskID  string                    `json:"replacement_task_id,omitempty"`
	PreviousStatus     string                    `json:"previous_status,omitempty"`
	Status             string                    `json:"status"`
	WorkerID           string                    `json:"worker_id,omitempty"`
	ChildRunID         string                    `json:"child_run_id,omitempty"`
	LeaseUntil         time.Time                 `json:"lease_until,omitempty"`
	EvidenceIDs        []string                  `json:"evidence_ids,omitempty"`
	EvidenceRunID      string                    `json:"evidence_run_id,omitempty"`
	Error              string                    `json:"error,omitempty"`
}
type PlanState struct {
	Capacity        int        `json:"capacity"`
	ReasonedThrough int        `json:"reasoned_through"`
	Tasks           []PlanTask `json:"tasks"`
	Frontier        []string   `json:"frontier"`
}

var errWorkerCapacity = errors.New("worker capacity reached")

// Capacity and completion share the same lifecycle boundary. A disposition
// retires a settled attempt without rewriting its historical uncertain result.
func workerUnsettled(s *Snapshot, w AgentWorker) bool {
	if w.Status == "running" || w.Status == "starting" || w.Status == "cancelling" || w.CleanupStatus != "completed" {
		return true
	}
	if w.Status != "uncertain" {
		return false
	}
	for _, task := range s.Plan.Tasks {
		if task.ID == w.TaskID && task.WorkerID == w.ID && task.ChildRunID == w.RunID && taskDisposed(task) && strings.TrimSpace(task.DispositionReason) != "" && (w.Visit == 0 || w.SettledVisit != nil && *w.SettledVisit == w.Visit) {
			return false
		}
	}
	return true
}

// Local occupancy also tracks incomplete manual tasks. Tree admission dedupes
// a current executor's one local task in workerCapacityLocked.
func occupiedWorkerSlots(s *Snapshot) int {
	active := 0
	for _, w := range s.Workers {
		if workerUnsettled(s, w) {
			active++
		}
	}
	for _, task := range s.Plan.Tasks {
		if task.Status == "running" || task.Status == "uncertain" {
			counted := false
			for _, w := range s.Workers {
				counted = counted || w.ID == task.WorkerID && w.TaskID == task.ID && w.RunID == task.ChildRunID && workerUnsettled(s, w)
			}
			if !counted {
				active++
			}
		}
	}
	return active
}

func refreshFrontier(p *PlanState) {
	p.Frontier = []string{}
	done := map[string]bool{}
	for _, t := range p.Tasks {
		done[t.ID] = taskDone(t)
	}
	ordered := append([]PlanTask(nil), p.Tasks...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Priority > ordered[j].Priority })
	for _, t := range ordered {
		ready := t.Status == "pending"
		for _, dep := range t.Dependencies {
			ready = ready && done[dep]
		}
		if ready {
			p.Frontier = append(p.Frontier, t.ID)
		}
	}
}

func (a *Agent) SetPlan(id string, tasks []PlanTask) (Snapshot, error) {
	return a.UpdatePlan(id, tasks, "replace")
}

// UpdatePlan applies append/patch atomically using the same frozen-contract checks.
func (a *Agent) UpdatePlan(id string, tasks []PlanTask, mode string) (Snapshot, error) {
	return a.updatePlan(id, tasks, mode, 0, false)
}

// ARTEX e6ec569 server/conversations.go:622 reserves admission under its queue
// lock before launch. Trace publishes the DAG and ceiling under its Store lock.
func (a *Agent) updatePlan(id string, tasks []PlanTask, mode string, capacity int, modelCall bool) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		if modelCall && s.Run.ParentTaskID == "" && len(tasks) > 0 && len(s.Plan.Tasks) == 0 && s.Plan.Capacity == 0 && capacity == 0 {
			return fmt.Errorf("first task plan requires explicit capacity: use the observed shared resource limit; five is the default ceiling, not a dispatch target")
		}
		if capacity != 0 {
			if err := setPlanCapacity(s, capacity); err != nil {
				return err
			}
		}
		tasks = append([]PlanTask(nil), tasks...)
		switch mode {
		case "", "replace":
		case "append", "patch":
			merged := append([]PlanTask(nil), s.Plan.Tasks...)
			seen := map[string]bool{}
			for _, t := range tasks {
				if seen[t.ID] {
					return fmt.Errorf("duplicate task %s", t.ID)
				}
				seen[t.ID] = true
				found := false
				for i := range merged {
					if merged[i].ID == t.ID {
						if mode == "append" {
							return fmt.Errorf("append task already exists: %s", t.ID)
						}
						merged[i] = t
						found = true
						break
					}
				}
				if !found {
					merged = append(merged, t)
				}
			}
			tasks = merged
		default:
			return fmt.Errorf("plan mode must be replace, append or patch")
		}
		old := map[string]PlanTask{}
		for _, t := range s.Plan.Tasks {
			old[t.ID] = t
		}
		byID := map[string]PlanTask{}
		for i := range tasks {
			t := tasks[i]
			if len(t.ContextEvidenceIDs) > 8 {
				return fmt.Errorf("task context_evidence_ids is limited to eight explicit parent sources")
			}
			if previous, ok := old[t.ID]; !ok || !taskStarted(s, previous) {
				for _, eid := range t.ContextEvidenceIDs {
					if _, ok := verifiedOutput(s, eid); !ok {
						return fmt.Errorf("task context source is not verified parent evidence: %s", eid)
					}
				}
			}
			if err := validateEvidenceScope(t.EvidenceScope); err != nil {
				return err
			}
			if t.Stage && s.RootContract == nil {
				return fmt.Errorf("stage tasks require an explicit root contract")
			}
			if t.EvidenceScope != nil {
				copy := *t.EvidenceScope
				t.EvidenceScope = &copy
			}
			if t.EstimatedSeconds < 0 || t.EstimatedSeconds > maxBudgetSeconds {
				return fmt.Errorf("task %s estimated_seconds must be 0..%d", t.ID, maxBudgetSeconds)
			}
			if err := ValidateRequirements(t.Requirements); err != nil {
				return fmt.Errorf("task %s requirements: %w", t.ID, err)
			}
			inputs, err := taskInputAttachments(s, t.InputAttachmentIDs)
			if err != nil {
				return err
			}
			// Persist the inferred native-image requirement so a replacement cannot
			// silently weaken it by dropping an explicit route for a text default.
			if t.ModelSelection != nil {
				for _, input := range inputs {
					if nativeImageMIME(input.MIMEType) {
						t.RequiredImageInput = true
					}
				}
			}
			if t.ID == "" || strings.TrimSpace(t.Goal) == "" {
				return fmt.Errorf("task id and goal required")
			}
			for _, ids := range [][]string{t.ResourceKeys, t.RequiredToolIDs, t.RequiredAssetIDs, t.ExpectedArtifacts, t.HandoffTaskIDs} {
				seen := map[string]bool{}
				for _, value := range ids {
					if strings.TrimSpace(value) == "" || seen[value] {
						return fmt.Errorf("task %s has empty or duplicate scheduling metadata", t.ID)
					}
					seen[value] = true
				}
			}
			if t.Mode != "" && t.Mode != "reason" && t.Mode != "explore" {
				return fmt.Errorf("invalid task mode %s", t.Mode)
			}
			if _, ok := byID[t.ID]; ok {
				return fmt.Errorf("duplicate task %s", t.ID)
			}
			if previous, ok := old[t.ID]; ok && taskStarted(s, previous) {
				if t.LocalExecution && !previous.LocalExecution || t.LocalWorkerVisit != 0 && t.LocalWorkerVisit != previous.LocalWorkerVisit {
					return fmt.Errorf("task %s local execution identity is server-owned", t.ID)
				}
				if t.ModelBinding != nil && (previous.ModelBinding == nil || *t.ModelBinding != *previous.ModelBinding) {
					return fmt.Errorf("task %s model_binding is server-owned", t.ID)
				}
				if strings.Join(previous.AssetIDs, "\x00") != strings.Join(t.AssetIDs, "\x00") {
					return fmt.Errorf("started task assets immutable: %s", t.ID)
				}
				if previous.CompletionCriteria != t.CompletionCriteria || !sameTaskSchedulingContract(previous, t) || !sameStageModelContract(previous, t) {
					return fmt.Errorf("started task scheduling contract immutable: %s", previous.ID)
				}
				if previous.Stage != t.Stage || !sameEvidenceScope(previous.EvidenceScope, t.EvidenceScope) || previous.Goal != t.Goal || strings.Join(previous.Dependencies, "\x00") != strings.Join(t.Dependencies, "\x00") || strings.Join(previous.InputAttachmentIDs, "\x00") != strings.Join(t.InputAttachmentIDs, "\x00") || previous.SuccessContains != t.SuccessContains || !sameTaskRequirements(previous.Requirements, t.Requirements) {
					return fmt.Errorf("started task immutable: %s", previous.ID)
				}
				t = previous
			} else {
				if t.LocalExecution || t.LocalWorkerVisit != 0 {
					return fmt.Errorf("task %s local execution identity is server-owned", t.ID)
				}
				if t.ModelBinding != nil {
					return fmt.Errorf("task %s model_binding is server-owned", t.ID)
				}
				if err := a.preflightStageModel(s, t); err != nil {
					return err
				}
				if t.ModelSelection != nil {
					copy := *t.ModelSelection
					t.ModelSelection = &copy
				}
				t.Status, t.WorkerID, t.ChildRunID = "pending", "", ""
				t.BlockedReason = nil
				t.Started = false
				t.DispositionReason, t.ReplacementTaskID, t.PreviousStatus, t.Error = "", "", "", ""
				t.EvidenceIDs, t.EvidenceRunID, t.LeaseUntil = nil, "", time.Time{}
			}
			tasks[i] = t
			byID[t.ID] = t
		}
		for _, task := range tasks {
			if len(task.HandoffTaskIDs) > 4 {
				return fmt.Errorf("task %s handoff_task_ids is limited to four explicit sources", task.ID)
			}
			for _, source := range task.HandoffTaskIDs {
				if _, ok := byID[source]; !ok || source == task.ID {
					return fmt.Errorf("task %s has invalid handoff source %s", task.ID, source)
				}
			}
		}
		visiting, done := map[string]bool{}, map[string]bool{}
		includeHandoffs := true
		var visit func(string) error
		visit = func(id string) error {
			if visiting[id] {
				return fmt.Errorf("dependency cycle at %s", id)
			}
			if done[id] {
				return nil
			}
			t, ok := byID[id]
			if !ok {
				return fmt.Errorf("unknown dependency %s", id)
			}
			visiting[id] = true
			for _, dep := range t.Dependencies {
				if err := visit(dep); err != nil {
					return err
				}
			}
			for _, source := range t.HandoffTaskIDs {
				if !includeHandoffs {
					continue
				}
				if err := visit(source); err != nil {
					return err
				}
			}
			if t.Status == "superseded" {
				replacement, ok := byID[t.ReplacementTaskID]
				if !ok || !taskContractCovers(replacement, t) {
					return fmt.Errorf("replacement for task %s must preserve its acceptance contract", t.ID)
				}
				if !includeHandoffs {
					if err := visit(t.ReplacementTaskID); err != nil {
						return err
					}
				}
			}
			visiting[id] = false
			done[id] = true
			return nil
		}
		for id := range byID {
			if err := visit(id); err != nil {
				return err
			}
		}
		// Historical handoffs and aggregate replacement links point in opposite
		// directions. Check each graph with dependencies, not their union.
		includeHandoffs = false
		visiting, done = map[string]bool{}, map[string]bool{}
		for id := range byID {
			if err := visit(id); err != nil {
				return err
			}
		}
		for _, t := range s.Plan.Tasks {
			if taskStarted(s, t) {
				if _, ok := byID[t.ID]; !ok {
					return fmt.Errorf("cannot remove started task %s", t.ID)
				}
			}
		}
		s.Plan.Tasks = tasks
		if s.Plan.Capacity == 0 && (!modelCall || len(tasks) > 0) {
			s.Plan.Capacity = 5
		}
		// Updating a plan does not acknowledge observations. Only a successful
		// model request's archived PlannerThrough consumes its exact fact batch.
		refreshFrontier(&s.Plan)
		addEvent(s, "plan_updated", "计划与依赖已保存", s.Plan)
		return nil
	})
}

func planTask(s *Snapshot, id string) (*PlanTask, error) {
	for i := range s.Plan.Tasks {
		if s.Plan.Tasks[i].ID == id {
			return &s.Plan.Tasks[i], nil
		}
	}
	return nil, fmt.Errorf("unknown task %s", id)
}

func (a *Agent) ClaimTask(id, taskID, workerID string, leaseSeconds int) (Snapshot, error) {
	if workerID == "" || leaseSeconds <= 0 {
		return Snapshot{}, fmt.Errorf("worker and positive lease required")
	}
	var blocked *TaskBlockedError
	var unchangedBlocked *Snapshot
	snapshot, err := a.store.Update(id, func(s *Snapshot) error {
		if isStopped(s.Run.Status) {
			return fmt.Errorf("parent run is stopped")
		}
		if err := a.checkWorkerAncestors(s); err != nil {
			return err
		}
		t, err := planTask(s, taskID)
		if err != nil {
			return err
		}
		local, err := a.localWorkerClaimLocked(s, taskID, workerID)
		if err != nil {
			return err
		}
		if t.LocalExecution && !local {
			return fmt.Errorf("local task execution identity changed")
		}
		if t.Status != "running" {
			missing, err := a.taskHandoffPrerequisitesLocked(s, *t)
			if err != nil {
				return err
			}
			if len(missing) != 0 {
				return &HandoffNotReadyError{TaskID: taskID, Sources: missing}
			}
		}
		firstClaim := t.Status != "running"
		if firstClaim {
			if err := a.preflightStageModel(s, *t); err != nil {
				return err
			}
		}
		if !firstClaim {
			if t.WorkerID != workerID || !local && !t.LeaseUntil.After(time.Now()) {
				return fmt.Errorf("task lease unavailable; recover expired worker explicitly")
			}
		} else {
			if t.Status == "pending" || t.Status == "blocked_capability" {
				blocked, err = a.taskPrerequisitesLocked(s, *t)
				if err != nil {
					return err
				}
				if blocked != nil {
					// Rechecking an unchanged prerequisite is not a state transition.
					// Update already verified sources (and persisted any revocation)
					// before this callback; retain that complete snapshot for callers.
					if t.Status == "blocked_capability" && t.Error == blocked.Error() && valueHash(t.BlockedReason) == valueHash(blocked) {
						unchangedBlocked = s
						return blocked
					}
					if valueHash(t.BlockedReason) != valueHash(blocked) {
						t.Status, t.BlockedReason, t.Error = "blocked_capability", blocked, blocked.Error()
						addEvent(s, "task_blocked_capability", "任务缺少显式必需能力或资产，未创建 Worker", blocked)
					}
					refreshFrontier(&s.Plan)
					return nil
				}
				if t.Status == "blocked_capability" {
					t.Status, t.BlockedReason, t.Error = "pending", nil, ""
					addEvent(s, "task_capability_restored", "任务必需能力已恢复", map[string]string{"task_id": t.ID})
				}
			}
			refreshFrontier(&s.Plan)
			if !hasString(s.Plan.Frontier, taskID) {
				return fmt.Errorf("task is not on frontier")
			}
			if !local {
				if err := a.checkWorkerCapacityLocked(s, 1); err != nil {
					return err
				}
			}
			if err := a.taskResourcesLocked(s, *t); err != nil {
				return err
			}
			if err := a.checkTaskDispatchWindowLocked(s, *t); err != nil {
				return err
			}
			admission := "worker"
			if local {
				admission = "local"
			}
			if err := a.checkTreeBudgetLocked(s, admission); err != nil {
				return err
			}
			binding, err := a.stageModelSelection(*t)
			if err != nil {
				return err
			}
			if local && !sameWorkerModel(binding, s.Run.ModelSelection) {
				return fmt.Errorf("local task model route differs from its current worker; dispatch after capacity is available")
			}
			t.ModelBinding = binding
			t.LocalExecution, t.LocalWorkerVisit = local, 0
			if local {
				t.LocalWorkerVisit = a.workerVisit
			}
			t.Status = "running"
			t.Started = true
			t.WorkerID = workerID
		}
		t.LeaseUntil = time.Now().UTC().Add(time.Duration(leaseSeconds) * time.Second)
		refreshFrontier(&s.Plan)
		if firstClaim {
			addEvent(s, "task_claimed", "任务 lease 已保存", *t)
		}
		return nil
	})
	if err != nil {
		if unchangedBlocked != nil && err == blocked {
			return *unchangedBlocked, err
		}
		return snapshot, err
	}
	if blocked != nil {
		return snapshot, blocked
	}
	return snapshot, nil
}

// Only the active child lifecycle calls this while holding its child Run lock.
// Waiting for a Store transaction must not revoke the same live executor.
// Public ClaimTask remains strict for expired/manual claims.
func (a *Agent) renewActiveWorkerLease(ctx context.Context, id string, w AgentWorker) error {
	_, err := a.store.Update(id, func(s *Snapshot) error {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if isStopped(s.Run.Status) {
			return fmt.Errorf("parent run is stopped")
		}
		t, err := planTask(s, w.TaskID)
		if err != nil {
			return err
		}
		if t.Status != "running" && !taskDone(*t) || t.WorkerID != w.ID || t.ChildRunID != w.RunID {
			return fmt.Errorf("worker ownership changed")
		}
		for _, current := range s.Workers {
			if current.ID == w.ID && current.RunID == w.RunID && current.TaskID == w.TaskID && current.Visit == w.Visit && current.Status == "running" {
				if taskDone(*t) {
					child, err := a.store.loadLocked(w.RunID)
					if err != nil || !reportBusinessContinuing(s.Run.ID, current, &child) {
						return fmt.Errorf("report continuation source or visit changed")
					}
				}
				t.LeaseUntil = time.Now().UTC().Add(30 * time.Second)
				return nil
			}
		}
		return fmt.Errorf("active worker identity is missing")
	})
	return err
}

func (a *Agent) CompleteTask(id, taskID, workerID, status string, evidenceIDs []string) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		t, err := planTask(s, taskID)
		if err != nil {
			return err
		}
		if t.Status != "running" || t.WorkerID != workerID || !t.LocalExecution && !t.LeaseUntil.After(time.Now()) {
			return fmt.Errorf("task lease not held")
		}
		if t.LocalExecution {
			if local, err := a.localWorkerClaimLocked(s, taskID, workerID); err != nil || !local {
				return fmt.Errorf("local task execution identity changed: %v", err)
			}
		}
		if t.ChildRunID != "" {
			return fmt.Errorf("dispatched child tasks are settled by their worker lifecycle")
		}
		if status != "completed" && status != "verified" && status != "failed" {
			return fmt.Errorf("invalid completion status")
		}
		if status == "verified" {
			if !taskScopeMatches(s, *t) {
				return fmt.Errorf("scoped task requires matching execution context")
			}
			if t.SuccessContains == "" && len(t.Requirements) == 0 {
				return fmt.Errorf("task has no business verification predicate")
			}
			if len(t.Requirements) > 0 && !taskRequirementsSatisfied(s, *t) {
				return fmt.Errorf("task structured requirements have not passed in this run")
			}
			matched := t.SuccessContains == ""
			for _, ev := range evidenceIDs {
				output, ok := verifiedOutput(s, ev)
				if !ok {
					return fmt.Errorf("invalid execution evidence %s", ev)
				}
				matched = matched || strings.Contains(output, t.SuccessContains)
			}
			if !matched {
				return fmt.Errorf("task predicate not satisfied")
			}
		}
		t.Status = status
		t.EvidenceIDs = evidenceIDs
		t.EvidenceRunID = id
		t.LeaseUntil = time.Time{}
		refreshFrontier(&s.Plan)
		addEvent(s, "task_completed", "任务结果已保存", *t)
		return nil
	})
}

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
	if len(s.Plan.Tasks) == 0 && len(s.Assets) == 0 {
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
		case "tool_result", "asset_added", "asset_updated", "relation_added", "worker_finished", "verification_invalidated", "task_retry_requested", "task_disposed", "task_blocked_capability", "task_capability_restored", "budget_checkpoint_requested":
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
	if s.Run.ParentRunID == "" && len(s.Workers) > 0 {
		role = "Coordinate delegated work: define task contracts and resource_keys, dispatch or message task owners, review outcomes and cleanup, and produce the final report. Delegate investigation and execution; do not repeat work owned by an active worker or create a separate queue."
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
	return map[string]any{"capacity": capacity, "total": len(p.Tasks), "counts": counts, "frontier": p.Frontier[:min(len(p.Frontier), capacity)], "ready_total": len(p.Frontier), "tasks": tasks, "history_tool": "worker_history"}
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

func (a *Agent) SetPlanCapacity(id string, capacity int) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error { return setPlanCapacity(s, capacity) })
}

func setPlanCapacity(s *Snapshot, capacity int) error {
	if capacity < 1 || capacity > 32 {
		return fmt.Errorf("capacity must be 1..32")
	}
	if s.Run.ParentTaskID != "" {
		return fmt.Errorf("worker capacity belongs to the root conversation")
	}
	s.Plan.Capacity = capacity
	addEvent(s, "worker_capacity_updated", "根会话整树 Worker 并发容量已更新", map[string]int{"capacity": capacity})
	return nil
}

func (a *Agent) DispatchFrontier(ctx context.Context, id string) ([]AgentWorker, error) {
	s, err := a.store.Load(id)
	if err != nil {
		return nil, err
	}
	workers := []AgentWorker{}
	frontier := append([]string(nil), s.Plan.Frontier...)
	for _, task := range s.Plan.Tasks {
		if task.Status != "blocked_capability" {
			continue
		}
		ready := true
		for _, dep := range task.Dependencies {
			previous, err := planTask(&s, dep)
			ready = ready && err == nil && taskDone(*previous)
		}
		if ready {
			frontier = append(frontier, task.ID)
		}
	}
	// Restored capability candidates compete at their declared priority, not
	// after every pending task merely because their prerequisite was missing.
	priorities := map[string]int{}
	for _, task := range s.Plan.Tasks {
		priorities[task.ID] = task.Priority
	}
	sort.SliceStable(frontier, func(i, j int) bool { return priorities[frontier[i]] > priorities[frontier[j]] })
	for _, taskID := range frontier {
		w, err := a.DispatchTask(ctx, id, taskID)
		if err != nil {
			if dispatchPrerequisiteWait(err) {
				continue
			}
			if errors.Is(err, errWorkerCapacity) {
				if len(workers) == 0 && s.Run.ParentTaskID != "" {
					return workers, err // Nested callers need the local-progress hint.
				}
				break
			}
			return workers, err
		}
		workers = append(workers, w)
	}
	return workers, nil
}

// A wait joined with a failed cleanup/settlement is a real failure, not a
// skippable prerequisite. Inspect all leaves rather than matching one cause.
func dispatchPrerequisiteWait(err error) bool {
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !dispatchPrerequisiteWait(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return dispatchPrerequisiteWait(wrapped.Unwrap())
	}
	switch err.(type) {
	case *TaskBlockedError, *ResourceBusyError, *HandoffNotReadyError, *TaskWindowError, *CheckpointWindowError:
		return true
	}
	return false
}
