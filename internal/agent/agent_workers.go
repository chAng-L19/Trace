package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"trace/internal/mcp"
	"trace/internal/worker"
)

type AgentWorker struct {
	ID            string    `json:"id"`
	TaskID        string    `json:"task_id"`
	RunID         string    `json:"run_id"`
	Status        string    `json:"status"`
	CleanupStatus string    `json:"cleanup_status"`
	Error         string    `json:"error,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at,omitempty"`
	Visit         uint64    `json:"visit,omitempty"`
	SettledVisit  *uint64   `json:"settled_visit,omitempty"`
}
type WorkerMessage struct {
	FromIdentity *MessageParticipant `json:"from_identity,omitempty"`
	ToIdentity   *MessageParticipant `json:"to_identity,omitempty"`
	OperationID  string              `json:"operation_id,omitempty"`
	RequestHash  string              `json:"request_hash,omitempty"`
	ID           string              `json:"id"`
	Kind         string              `json:"kind,omitempty"`
	ReplyTo      string              `json:"reply_to,omitempty"`
	From         string              `json:"from"`
	To           string              `json:"to"`
	Content      string              `json:"content"`
	Delivered    bool                `json:"delivered"`
	CreatedAt    time.Time           `json:"created_at"`
}

// DispatchTask persists a claim and child identity before invoking the child
// model loop. Separate child runs retain independent evidence and run locks.
func (a *Agent) DispatchTask(ctx context.Context, id, taskID string) (AgentWorker, error) {
	// Like ARTEX pumpLocked, a stopped dispatcher must not consume queued work.
	if err := ctx.Err(); err != nil {
		return AgentWorker{}, err
	}
	ctx, release, err := a.workers.begin(ctx)
	if err != nil {
		return AgentWorker{}, err
	}
	launched := false
	defer func() {
		if !launched {
			release()
		}
	}()
	wid, err := newID()
	if err != nil {
		return AgentWorker{}, err
	}
	snap, err := a.claimTask(ctx, id, taskID, wid, 30)
	if err != nil {
		return AgentWorker{}, err
	}
	task, _ := planTask(&snap, taskID)
	workerContext, err := a.newWorkerContext(&snap, *task)
	if err != nil {
		return AgentWorker{}, errors.Join(err, a.finishWorkerFailure(id, taskID, wid, err))
	}
	workerContext.WorkerID = wid
	for _, handoff := range workerContext.Handoffs {
		if handoff.Status != "available" {
			err := &HandoffNotReadyError{TaskID: taskID, Sources: []string{handoff.SourceTaskID}}
			return AgentWorker{}, errors.Join(err, a.finishWorkerFailure(id, taskID, wid, err))
		}
	}
	workerContext.MethodCandidates = a.recallWorkerMethods(task.Goal)
	inputs, err := taskInputAttachments(&snap, task.InputAttachmentIDs)
	if err != nil {
		return AgentWorker{}, errors.Join(err, a.finishWorkerFailure(id, taskID, wid, err))
	}
	stageModel, err := a.stageModelSelection(*task)
	if err != nil {
		return AgentWorker{}, errors.Join(err, a.finishWorkerFailure(id, taskID, wid, err))
	}
	child, err := a.CreateConfigured(task.Goal, RunConfiguration{Requirements: task.Requirements, ModelSelection: stageModel, inheritedRun: &snap.Run, TreeBudget: &TreeBudget{}, role: "worker"}, inputs...)
	if err != nil {
		return AgentWorker{}, errors.Join(err, a.finishWorkerFailure(id, taskID, wid, err))
	}
	workerContext.RunID = child.Run.ID
	unlock, err := a.store.AcquireRunLock(child.Run.ID)
	if err != nil {
		// A different holder may already be running this child. Cancellation
		// requests its stop but does not prove exit, so retain the claim.
		_, cancelErr := a.Cancel(child.Run.ID)
		return AgentWorker{}, errors.Join(err, cancelErr)
	}
	defer func() {
		if !launched {
			unlock()
		}
	}()
	// An unregistered child must be stopped before releasing its claim. A
	// failed cancellation leaves that claim occupied for explicit recovery.
	failChild := func(cause error) (AgentWorker, error) {
		if _, cancelErr := a.Cancel(child.Run.ID); cancelErr != nil {
			return AgentWorker{}, errors.Join(cause, cancelErr)
		}
		return AgentWorker{}, errors.Join(cause, a.finishWorkerFailure(id, taskID, wid, cause))
	}
	_, err = a.store.Update(child.Run.ID, func(s *Snapshot) error {
		s.Run.RequireVerifiedCompletion = snap.Run.RequireVerifiedCompletion
		s.Run.ParentRunID = id
		s.Run.ParentTaskID = taskID
		if s.Run.ToolSelection != nil && len(task.RequiredToolIDs) > 0 {
			selection := &ToolSelection{IDs: append([]string(nil), s.Run.ToolSelection.IDs...)}
			for _, toolID := range task.RequiredToolIDs {
				if tool, ok := a.registry.Lookup(toolID); ok && externalTool(tool) && !hasString(selection.IDs, toolID) {
					selection.IDs = append(selection.IDs, toolID)
				}
			}
			s.Run.ToolSelection = selection
		}
		s.Run.SuccessContains = task.SuccessContains
		s.WorkerContext = workerContext
		addEvent(s, "worker_context_created", "任务限定上下文与来源引用已保存", map[string]any{"parent_run_id": id, "task_id": taskID, "assets": len(workerContext.Assets), "dependencies": len(workerContext.Dependencies), "evidence_refs": len(workerContext.Evidence)})
		return nil
	})
	if err != nil {
		return failChild(err)
	}
	w := AgentWorker{ID: wid, TaskID: taskID, RunID: child.Run.ID, Status: "running", CleanupStatus: "pending", StartedAt: time.Now().UTC(), Visit: 1}
	_, err = a.store.Update(id, func(s *Snapshot) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := a.checkTaskAdmissionLocked(s); err != nil {
			return err
		}
		t, e := planTask(s, taskID)
		if e != nil {
			return e
		}
		if t.WorkerID != wid || t.Status != "running" || !t.LeaseUntil.After(time.Now()) {
			return fmt.Errorf("lease changed")
		}
		if err := a.checkWorkerCapacityLocked(s, 0); err != nil {
			return err
		}
		if err := a.checkTaskDispatchWindowLocked(s, *t); err != nil {
			return err
		}
		if err := a.validateHandoffManifestLocked(s, *t, workerContext.Handoffs); err != nil {
			return err
		}
		t.ChildRunID = child.Run.ID
		s.Workers = append(s.Workers, w)
		addEvent(s, "worker_started", "子智能 worker 已启动", w)
		return nil
	})
	if err != nil {
		return failChild(err)
	}
	launched = true
	go func() { defer release(); defer unlock(); a.runChild(ctx, id, w) }()
	return w, nil
}

// Like Muteki coordinator_schedule_dispatch.py:162-175, a failed terminal
// transition must remain visible rather than masquerading as settled work.
func (a *Agent) finishWorkerFailure(id, taskID, wid string, cause error) error {
	_, err := a.store.Update(id, func(s *Snapshot) error {
		t, err := planTask(s, taskID)
		if err != nil {
			return err
		}
		if t.WorkerID == wid {
			t.Status = "failed"
			t.Error = cause.Error()
			t.LeaseUntil = time.Time{}
			refreshFrontier(&s.Plan)
		}
		return nil
	})
	return err
}

// Caller owns the child's execution lock through final parent settlement.
func (a *Agent) runChild(parent context.Context, id string, w AgentWorker) (Snapshot, error) {
	return a.runChildOperation(parent, id, w, (*Agent).runLocked)
}

func (a *Agent) runChildOperation(parent context.Context, id string, w AgentWorker, operation func(*Agent, context.Context, string) (Snapshot, error)) (Snapshot, error) {
	ctx, cancelCause := context.WithCancelCause(parent)
	defer cancelCause(nil)
	stop := func(reason string, cause error) {
		cancelCause(fmt.Errorf("worker %s %s: %w", w.ID, reason, errors.Join(cause, context.Canceled)))
	}
	changed, unsubscribe := a.store.SubscribeScheduling(id)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer unsubscribe()
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			// Observe after subscribing, then wake only on committed hints or
			// the existing cross-process poll. A hint itself proves no state.
			if ctx.Err() != nil {
				return
			}
			snap, err := a.store.Observe(id)
			if err != nil {
				stop("parent observation failed", err)
				return
			}
			if err := a.checkTaskAdmissionLocked(&Snapshot{Run: snap.Run}); err != nil {
				stop("parent or ancestor stopped", err)
				return
			}
			t, e := planTask(&Snapshot{Plan: snap.Plan}, w.TaskID)
			currentVisit := false
			for _, current := range snap.Workers {
				currentVisit = currentVisit || current.ID == w.ID && current.TaskID == w.TaskID && current.RunID == w.RunID && current.Visit == w.Visit && current.Status == "running"
			}
			if e != nil || t.WorkerID != w.ID || t.Status != "running" && !taskDone(*t) || !currentVisit {
				stop("task ownership lost", fmt.Errorf("task %s is missing or no longer owned and running", w.TaskID))
				return
			}
			if taskDone(*t) {
				child, err := a.store.Load(w.RunID)
				if err != nil || !reportBusinessContinuing(id, w, &child) {
					stop("report continuation source changed", errors.Join(err, fmt.Errorf("business completion no longer matches its settled visit")))
					return
				}
			}
			if time.Until(t.LeaseUntil) < 15*time.Second {
				if err = a.renewActiveWorkerLease(ctx, id, w); err != nil {
					stop("lease renewal failed", err)
					return
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-changed:
			}
		}
	}()
	childAgent := *a
	childAgent.workerRunID = w.RunID
	childAgent.workerVisit = w.Visit
	childAgent.workers = newWorkerLifecycle()
	childAgent.mcpState = &mcpLifecycle{}
	childAgent.mcpClients = nil
	// Capture the already-settled business source before reconnecting tools:
	// a failed Clone must not revoke it when report-only delivery remains paused
	// and the failure's actual resource cleanup is independently complete.
	var reportSourceHash string
	if prior, loadErr := a.store.Load(w.RunID); loadErr == nil && reportBusinessContinuing(id, w, &prior) {
		reportSourceHash = workerBusinessSourceHash(&prior)
	}
	registry, clients, err := a.registry.Clone(ctx)
	// A failed Dial has already attempted cleanup and returns no client. Its
	// independent cleanup observation must still block a zero-action retry.
	var cloneCleanupErr error
	var connection *mcp.ConnectionError
	if errors.As(err, &connection) && len(connection.Status.Cleanup) > 0 {
		cloneCleanupErr = connection
	}
	childAgent.registry = registry
	childAgent.mcpClients = clients
	var child Snapshot
	if err == nil {
		child, err = a.store.Load(w.RunID)
		if err == nil {
			err = a.checkWorkerAncestors(&child)
		}
	}
	if err == nil {
		err = childAgent.resumeLocalTaskVisit(w.RunID, w)
	}
	if err == nil {
		child, err = operation(&childAgent, ctx, w.RunID)
	} else {
		cloneErr := err
		child, err = a.fail(w.RunID, err)
		if err == nil {
			err = cloneErr
		}
	}
	if err != nil && ctx.Err() != nil {
		err = errors.Join(err, context.Cause(ctx))
	}
	stopErr := childAgent.StopWorkers(context.Background())
	cleanupErr, pendingErr := splitWorkerSettlementErrors(errors.Join(stopErr, childAgent.CloseMCP()))
	cleanupErr = errors.Join(cloneCleanupErr, cleanupErr)
	// Read durable results even when operation returned an error/empty snapshot.
	latest, loadErr := a.store.Load(w.RunID)
	lifecycleErr, descendantToolErr := splitWorkerToolCleanupErrors(errors.Join(cleanupErr, loadErr))
	var ownToolErr error
	if loadErr == nil {
		ownToolErr = toolCleanupError(latest)
	}
	if ownToolErr != nil {
		ownToolErr = &workerToolCleanupError{runID: w.RunID, err: ownToolErr}
	}
	cleanupErr = errors.Join(lifecycleErr, descendantToolErr, ownToolErr)
	a.recordWorkerCleanupError(cleanupErr)
	cancelCause(nil)
	<-done
	receipt := WorkerSettlement{ParentRunID: id, TaskID: w.TaskID, WorkerID: w.ID, RunID: w.RunID, Visit: w.Visit, CleanupStatus: "completed", CleanupVersion: 1, ObservedAt: time.Now().UTC(), DescendantSettlementPending: pendingErr != nil, DescendantCleanupPending: descendantToolErr != nil}
	if loadErr == nil && businessResultReady(&latest) {
		receipt.BusinessSourceHash = workerBusinessSourceHash(&latest)
		receipt.ReportDeliveryPending = reportDeliveryPending(&latest)
		if reportSourceHash != "" && reportSourceHash == receipt.BusinessSourceHash && w.SettledVisit != nil {
			previous := *w.SettledVisit
			receipt.BusinessContinuationVisit = &previous
		}
	}
	if lifecycleErr != nil {
		receipt.LifecycleCleanupError = lifecycleErr.Error()
	}
	if ownToolErr != nil {
		receipt.ToolCleanupError = ownToolErr.Error()
	}
	if err != nil {
		receipt.RunError = err.Error()
	}
	if cleanupErr != nil {
		receipt.CleanupStatus, receipt.CleanupError = "uncertain", cleanupErr.Error()
	}
	_, settleErr := a.store.Update(w.RunID, func(s *Snapshot) error {
		if s.Run.ParentRunID != id || s.Run.ParentTaskID != w.TaskID {
			return fmt.Errorf("worker settlement child identity changed")
		}
		// The child owns its interruption cause even if its parent claim changed.
		if receipt.RunError != "" && s.Run.Status == StatusFailed && s.Run.Error != receipt.RunError {
			s.Run.Error = receipt.RunError
			addEvent(s, "worker_interrupted", "Worker 中断原因已保存", map[string]string{"worker_id": w.ID, "error": receipt.RunError})
		}
		s.WorkerSettlement = &receipt
		return nil
	})
	if settleErr == nil {
		_, settleErr = a.store.Update(id, func(s *Snapshot) error {
			if err := a.applyWorkerSettlementLocked(s, w); err != nil {
				return err
			}
			latest, err := a.store.loadLocked(w.RunID)
			if err == nil {
				child = latest
			}
			return err
		})
	}
	if settleErr != nil {
		settleErr = &workerSettlementPendingError{err: settleErr}
	}
	a.recordWorkerSettlementError(id, w, settleErr)
	return child, errors.Join(err, cleanupErr, settleErr)
}

// WaitWorkers keeps the parent model loop available to consume worker messages
// without inventing a successful task completion while children are active.
func (a *Agent) WaitWorkers(ctx context.Context, id string, seconds int) (Snapshot, error) {
	if seconds <= 0 {
		seconds = 300
	}
	timer := time.NewTimer(time.Duration(seconds) * time.Second)
	defer timer.Stop()
	changes, unsubscribe := a.store.SubscribeScheduling(id)
	defer unsubscribe()
	before, err := a.store.Observe(id)
	if err != nil {
		return Snapshot{}, err
	}
	var parentChanges <-chan struct{}
	if before.Run.ParentRunID != "" && before.Run.ParentTaskID != "" {
		var stop func()
		parentChanges, stop = a.store.SubscribeScheduling(before.Run.ParentRunID)
		defer stop()
	}
	// Notifications cover committed local changes; this bounded fallback keeps
	// writes by another CLI process observable without token/lease wakeups.
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	initial := map[string]string{}
	first := true
	for {
		if err := a.reconcileWorkerSettlements(id); err != nil {
			return Snapshot{}, err
		}
		snap, err := a.store.Observe(id)
		if err != nil {
			return Snapshot{}, err
		}
		active := false
		changed := isStopped(snap.Run.Status)
		changed = changed || snap.Run.BudgetCheckpoint != nil && (before.Run.BudgetCheckpoint == nil || !snap.Run.BudgetCheckpoint.RequestedAt.Equal(before.Run.BudgetCheckpoint.RequestedAt))
		for _, w := range snap.Workers {
			active = active || w.Status == "running"
			state := w.Status + ":" + w.CleanupStatus
			if first {
				initial[w.ID] = state
			} else if initial[w.ID] != state {
				changed = true
			}
		}
		first = false
		for _, m := range snap.WorkerMessages {
			changed = changed || (m.To == "parent" && !m.Delivered)
		}
		for _, m := range snap.Mailbox {
			// Follow-ups belong to the next idle conversation turn. Waking for
			// them here would repeat an input the active loop cannot yet consume.
			changed = changed || (m.Kind == "steer" && m.Pending())
		}
		if snap.Run.ParentRunID != "" {
			parent, err := a.store.Observe(snap.Run.ParentRunID)
			if err != nil {
				return Snapshot{}, err
			}
			for _, w := range parent.Workers {
				if w.RunID != id {
					continue
				}
				for _, m := range parent.WorkerMessages {
					changed = changed || (m.To == w.ID && !m.Delivered)
				}
			}
		}
		hasRunning := active
		if !active {
			for _, w := range snap.Workers {
				unlock, err := a.store.AcquireRunLock(w.RunID)
				if err != nil {
					if !errors.Is(err, ErrRunLockBusy) {
						return Snapshot{}, fmt.Errorf("wait worker %s: %w", w.ID, err)
					}
					active = true
					break
				}
				unlock()
			}
		}
		if !active || (changed && hasRunning) || isStopped(snap.Run.Status) {
			return a.store.Load(id)
		}
		select {
		case <-ctx.Done():
			return Snapshot{}, ctx.Err()
		case <-timer.C:
			return a.store.Load(id)
		case <-ticker.C:
		case <-changes:
		case <-parentChanges:
		}
	}
}

func workerRun(s *Snapshot, who string) (string, error) {
	if who == "parent" {
		return s.Run.ID, nil
	}
	for _, w := range s.Workers {
		if w.ID == who {
			return w.RunID, nil
		}
	}
	return "", fmt.Errorf("unknown worker %s", who)
}

func (a *Agent) SendWorkerMessage(id, from, to, content string) (Snapshot, error) {
	return a.SendWorkerMessageRelated(id, from, to, content, "", "")
}

// Related messages share the durable inbox; resolution is a notice, not a
// task transition or a request to resume a paused child.
func (a *Agent) SendWorkerMessageRelated(id, from, to, content, kind, replyTo string) (Snapshot, error) {
	s, _, _, err := a.SendWorkerMessageOperation(id, from, to, content, kind, replyTo, "")
	return s, err
}

func appendWorkerMessage(s *Snapshot, from, to, content string) error {
	return appendRelatedWorkerMessage(s, from, to, content, "", "")
}

func appendRelatedWorkerMessage(s *Snapshot, from, to, content, kind, replyTo string) error {
	if _, err := workerRun(s, from); err != nil {
		return err
	}
	if _, err := workerRun(s, to); err != nil {
		return err
	}
	if err := validateWorkerRelation(s.WorkerMessages, from, to, kind, replyTo); err != nil {
		return err
	}
	mid, err := newID()
	if err != nil {
		return err
	}
	m := WorkerMessage{ID: mid, From: from, To: to, Content: content, Kind: kind, ReplyTo: replyTo, CreatedAt: time.Now().UTC()}
	m.FromIdentity, m.ToIdentity = messageParticipant(s, from), messageParticipant(s, to)
	s.WorkerMessages = append(s.WorkerMessages, m)
	addEvent(s, "worker_message", "worker 消息已保存", m)
	return nil
}

// DeliverWorkerMessages runs only between complete tool batches. Receiver
// events deduplicate a crash between receiver append and sender acknowledgment.
func (a *Agent) DeliverWorkerMessages(id string) error {
	snap, err := a.store.Observe(id)
	if err != nil {
		return err
	}
	// A nested worker is both a child and a parent: drain its own children's
	// replies as well as the upstream parent/sibling inbox.
	if err := a.deliverWorkerInbox(id, id, "parent", snap); err != nil {
		return err
	}
	if snap.Run.ParentRunID != "" && snap.Run.ParentTaskID != "" {
		rootID := snap.Run.ParentRunID
		root, err := a.store.Observe(rootID)
		if err != nil {
			return err
		}
		to := ""
		for _, w := range root.Workers {
			if w.RunID == id {
				to = w.ID
			}
		}
		if to == "" {
			return fmt.Errorf("child has no registered worker identity")
		}
		return a.deliverWorkerInbox(id, rootID, to, root)
	}
	return nil
}

func (a *Agent) deliverWorkerInbox(id, rootID, to string, root Observation) error {
	pending := map[string]bool{}
	for _, m := range root.WorkerMessages {
		if m.To == to && !m.Delivered {
			pending[m.ID] = true
		}
	}
	if len(pending) == 0 {
		return nil
	}
	// pi session/jsonl/storage.ts applyCommit batches one complete transaction.
	// The projection only selects candidates; Update still verifies the receiver.
	committed := map[string]bool{}
	_, err := a.store.Update(id, func(s *Snapshot) error {
		// Recheck at the receiver commit boundary: the earlier observation may
		// predate report delivery. A deferred receiver must never ack its sender.
		if reportDeliveryPending(s) {
			return nil
		}
		delivered := map[string]bool{}
		for _, event := range s.Events {
			if strings.HasPrefix(event.Type, "worker_message:") {
				delivered[event.Type] = true
			}
		}
		projectionPending := map[string]bool{}
		for mid := range pending {
			projectionPending[mid] = !delivered["worker_message:"+mid]
		}
		duplicates := duplicateWorkerReports(root.WorkerMessages, projectionPending, to)
		receivedBy := messageParticipant(&Snapshot{Run: root.Run, Workers: root.Workers}, to)
		if receivedBy != nil && a.workerRunID == id {
			receivedBy.Visit = a.workerVisit
		}
		representative := ""
		for _, m := range root.WorkerMessages {
			marker := "worker_message:" + m.ID
			if !pending[m.ID] || delivered[marker] {
				continue
			}
			if !duplicates[m.ID] {
				representative = m.ID
				s.Messages = append(s.Messages, Message{ID: workerHistoryMessageID(rootID, m.ID), Role: "user", Content: workerMessageContent(m), CreatedAt: time.Now().UTC()})
			}
			addEvent(s, marker, "worker 消息已投递", workerDeliveryMarker{InboxRunID: rootID, MessageID: m.ID, HistoryMessageID: workerHistoryMessageID(rootID, representative), RepresentativeID: representative, Coalesced: duplicates[m.ID], Receiver: receivedBy})
			delivered[marker] = true
		}
		for mid := range pending {
			committed[mid] = delivered["worker_message:"+mid]
		}
		if id == rootID {
			for i := range s.WorkerMessages {
				if committed[s.WorkerMessages[i].ID] {
					s.WorkerMessages[i].Delivered = true
				}
			}
		}
		return nil
	})
	if err != nil || id == rootID || len(committed) == 0 {
		return err
	}
	// Cross-Run delivery remains receiver-commit first, then sender ack. A crash
	// between commits retries the same markers without appending messages twice.
	_, err = a.store.Update(rootID, func(s *Snapshot) error {
		for i := range s.WorkerMessages {
			if committed[s.WorkerMessages[i].ID] {
				s.WorkerMessages[i].Delivered = true
			}
		}
		return nil
	})
	return err
}

func (a *Agent) CancelWorker(id, workerID string) (Snapshot, error) {
	snap, err := a.store.Load(id)
	if err != nil {
		return snap, err
	}
	childID, err := workerRun(&snap, workerID)
	if err != nil {
		return snap, err
	}
	if workerID == "parent" {
		return snap, fmt.Errorf("use parent cancel")
	}
	child, loadErr := a.store.Load(childID)
	if loadErr != nil {
		return snap, loadErr
	}
	if child.Run.Status != StatusCancelled {
		if _, err = a.Cancel(childID); err != nil {
			return snap, err
		}
	}
	return a.settleCancelledChild(id, childID)
}

// A paused/uncertain child has no live goroutine to settle its parent after
// direct Web/CLI cancellation. Active workers still settle after cleanup.
func (a *Agent) settleCancelledChild(id, childID string) (Snapshot, error) {
	return a.store.Update(id, func(s *Snapshot) error {
		for i := range s.Workers {
			w := &s.Workers[i]
			if w.RunID != childID || w.Status == "running" || w.CleanupStatus != "completed" {
				continue
			}
			task, err := planTask(s, w.TaskID)
			if err != nil {
				return err
			}
			if taskDisposed(*task) {
				continue
			}
			if task.WorkerID == w.ID && task.ChildRunID == childID && (w.Status != "cancelled" || task.Status != "cancelled") {
				w.Status, task.Status = "cancelled", "cancelled"
				task.LeaseUntil = time.Time{}
				refreshFrontier(&s.Plan)
				addEvent(s, "worker_cancel_requested", "子 worker 取消已结算", map[string]string{"worker_id": w.ID})
			}
		}
		return nil
	})
}

// Expiry never requeues a task: a dead dispatcher may have already committed
// external effects. A recovered claim remains uncertain until inspected.
func (a *Agent) RecoverWorkers(id string) (Snapshot, error) {
	return a.recoverWorkersLocked(id, map[string]bool{})
}

// Locks follow the durable worker tree from parent to child. Recursion runs
// outside Store transactions; every application below rechecks current identity.
func (a *Agent) recoverWorkersLocked(id string, visiting map[string]bool) (Snapshot, error) {
	if visiting[id] {
		return Snapshot{}, fmt.Errorf("worker recovery ancestry contains a cycle")
	}
	visiting[id] = true
	defer delete(visiting, id)
	before, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	var releases []func()
	locked := map[string]bool{}
	defer func() {
		for _, release := range releases {
			release()
		}
	}()
	for _, task := range before.Plan.Tasks {
		if task.ChildRunID == "" || !taskAwaitingWorkerSettlement(&before, task) {
			continue
		}
		if visiting[task.ChildRunID] {
			return Snapshot{}, fmt.Errorf("worker recovery ancestry contains a cycle")
		}
		if locked[task.ChildRunID] {
			continue
		}
		release, lockErr := a.store.AcquireRunLock(task.ChildRunID)
		if lockErr != nil {
			if !errors.Is(lockErr, ErrRunLockBusy) {
				return Snapshot{}, fmt.Errorf("recover worker child %s: %w", task.ChildRunID, lockErr)
			}
			continue
		}
		locked[task.ChildRunID] = true
		releases = append(releases, release)
		child, loadErr := a.store.Load(task.ChildRunID)
		if loadErr != nil {
			return Snapshot{}, loadErr
		}
		if child.Run.ParentRunID != id || child.Run.ParentTaskID != task.ID {
			return Snapshot{}, fmt.Errorf("worker recovery child identity changed")
		}
		child, loadErr = a.reconcileExecResourcesLocked(task.ChildRunID, worker.ReconcileResourceOwner)
		if loadErr != nil {
			return Snapshot{}, loadErr
		}
		if child.WorkerSettlement != nil && (child.WorkerSettlement.DescendantSettlementPending || child.WorkerSettlement.DescendantCleanupPending) {
			if _, err := a.recoverWorkersLocked(task.ChildRunID, visiting); err != nil {
				return Snapshot{}, err
			}
			if _, err := a.commitExecResourceObservations(task.ChildRunID, nil); err != nil {
				return Snapshot{}, err
			}
		}
	}
	var attempted []AgentWorker
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		for i := range s.Plan.Tasks {
			t := &s.Plan.Tasks[i]
			if !taskAwaitingWorkerSettlement(s, *t) {
				continue
			}
			if active, err := a.localTaskActiveLocked(s, *t); err != nil {
				return err
			} else if active {
				continue // The same live executor renews its parent lease.
			}
			var current *AgentWorker
			for j := range s.Workers {
				w := &s.Workers[j]
				if w.ID == t.WorkerID && w.RunID == t.ChildRunID && w.TaskID == t.ID {
					current = w
					break
				}
			}
			if t.ChildRunID != "" {
				if !locked[t.ChildRunID] {
					continue
				}
				child, loadErr := a.store.loadLocked(t.ChildRunID)
				if loadErr != nil {
					return loadErr
				}
				if current != nil && child.WorkerSettlement.matches(id, *current) && child.WorkerSettlement.CleanupStatus == "completed" {
					attempted = append(attempted, *current)
					if err := a.applyWorkerSettlementLocked(s, *current); err != nil {
						return err
					}
					continue
				}
			}
			if (t.Status == "running" || taskDone(*t)) && !t.LeaseUntil.After(time.Now()) {
				t.Status = "uncertain"
				t.Error = "worker lease expired; inspect child evidence before retry"
				if current != nil {
					current.Status, current.CleanupStatus = "uncertain", "unknown"
				}
				addEvent(s, "worker_lease_expired", "过期 worker 待核对，未重放", map[string]string{"task_id": t.ID, "worker_id": t.WorkerID})
			}
		}
		refreshFrontier(&s.Plan)
		return nil
	})
	for _, w := range attempted {
		a.recordWorkerSettlementError(id, w, err)
	}
	return snap, err
}
