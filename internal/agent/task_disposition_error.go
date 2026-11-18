package agent

import (
	"encoding/json"
	"unicode/utf8"
)

// TaskDispositionError is a read-only, bounded explanation of a rejected
// disposition. It never authorizes recovery, replay, cleanup or cancellation.
type TaskDispositionError struct {
	Code                    string               `json:"code"`
	Reason                  string               `json:"reason"`
	RunID                   string               `json:"run_id"`
	TaskID                  string               `json:"task_id"`
	TaskStatus              string               `json:"task_status"`
	WorkerID                string               `json:"worker_id,omitempty"`
	WorkerStatus            string               `json:"worker_status,omitempty"`
	ChildRunID              string               `json:"child_run_id,omitempty"`
	ChildStatus             Status               `json:"child_status,omitempty"`
	ChildIdentityMatches    bool                 `json:"child_identity_matches"`
	Visit                   uint64               `json:"visit"`
	SettledVisit            *uint64              `json:"settled_visit,omitempty"`
	CleanupStatus           string               `json:"cleanup_status"`
	SettlementVisit         *uint64              `json:"settlement_visit,omitempty"`
	SettlementCleanupStatus string               `json:"settlement_cleanup_status,omitempty"`
	RequestID               string               `json:"request_id,omitempty"`
	Calls                   []WorkerCallActivity `json:"calls,omitempty"`
	CallsTotal              int                  `json:"calls_total"`
	NextAction              string               `json:"next_action"`
	cause                   error
}

func (e *TaskDispositionError) Error() string {
	data, _ := json.Marshal(e)
	return "task disposition blocked: " + string(data)
}

func (e *TaskDispositionError) Unwrap() error { return e.cause }

func dispositionText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit - len("[truncated]")
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "[truncated]"
}

func newTaskDispositionError(parent *Snapshot, task PlanTask, child *Snapshot, blockedWorker *AgentWorker, code string, cause error) *TaskDispositionError {
	e := &TaskDispositionError{
		Code: code, Reason: dispositionText(cause.Error(), 1024), cause: cause,
		RunID: dispositionText(parent.Run.ID, 256), TaskID: dispositionText(task.ID, 256),
		TaskStatus: dispositionText(task.Status, 256), WorkerID: dispositionText(task.WorkerID, 256),
		ChildRunID: dispositionText(task.ChildRunID, 256), CleanupStatus: "unknown",
		NextAction: dispositionNextAction(code),
	}
	if blockedWorker == nil {
		for i := range parent.Workers {
			w := &parent.Workers[i]
			if w.ID == task.WorkerID && w.TaskID == task.ID && w.RunID == task.ChildRunID {
				blockedWorker = w
				break
			}
		}
	}
	if blockedWorker != nil {
		w := blockedWorker
		e.WorkerID, e.WorkerStatus = dispositionText(w.ID, 256), dispositionText(w.Status, 256)
		e.Visit, e.CleanupStatus = w.Visit, dispositionText(w.CleanupStatus, 256)
		if w.SettledVisit != nil {
			visit := *w.SettledVisit
			e.SettledVisit = &visit
		}
	}
	if child != nil {
		e.ChildStatus = Status(dispositionText(string(child.Run.Status), 256))
		if child.Run.Status == StatusCancelled && (code == "child_action_uncertain" || code == "child_active" || code == "task_running" || code == "child_execution_open") {
			e.NextAction = "子运行已取消，保留原始 request_id/call_id、未知结果及副作用事实。使用 worker_wait 等待仍持锁的执行退出，使用 worker_recover 核对原执行的清理回执；started 动作、身份或当前 visit 不符、未完成清理及未结算后代仍阻断处置。确认当前 visit 的执行与清理均已结算后，可用 task_dispose 显式记录 blocked 或 abandoned 并说明未完成目标。未知结果仍阻断 superseded，不将取消或清理完成计为成功；禁止重放历史动作。"
		}
		e.ChildIdentityMatches = child.Run.ID == task.ChildRunID && child.Run.ParentRunID == parent.Run.ID && child.Run.ParentTaskID == task.ID
		if !e.ChildIdentityMatches {
			return e
		}
		e.RequestID = dispositionText(currentRequestID(child), 256)
		if receipt := child.WorkerSettlement; receipt != nil {
			visit := receipt.Visit
			e.SettlementVisit, e.SettlementCleanupStatus = &visit, dispositionText(receipt.CleanupStatus, 256)
		}
		e.Calls, e.CallsTotal = projectToolActivity(child.Actions)
		// Include not-yet-started calls from the current durable assistant batch.
		// Existing activity rows retain their original request IDs, including old
		// uncertain actions; a display projection never rebases their identity.
		pending := pendingToolCalls(child)
		for _, action := range child.Actions {
			if len(pending) == 0 {
				break
			}
			if actionRequestMatchesCurrent(child, action) {
				delete(pending, action.CallID)
			}
		}
		if batch := latestToolBatchIndex(child); batch >= 0 {
			message := child.Messages[batch]
			requestID := message.RequestID
			if requestID == "" && message.ID != "" {
				requestID = "manual-batch:" + message.ID
			}
			for _, call := range message.ToolCalls {
				if _, ok := pending[call.ID]; !ok {
					continue
				}
				e.CallsTotal++
				if len(e.Calls) < 20 {
					e.Calls = append(e.Calls, WorkerCallActivity{CallID: call.ID, RequestID: requestID, Name: call.Name, Status: "pending"})
				}
			}
		}
		for i := range e.Calls {
			call := &e.Calls[i]
			call.Diagnostic = nil // A call can itself contain a disposition error.
			call.CallID, call.RequestID, call.Name = dispositionText(call.CallID, 256), dispositionText(call.RequestID, 256), dispositionText(call.Name, 256)
			call.Status, call.WorkerStatus = dispositionText(call.Status, 256), dispositionText(call.WorkerStatus, 256)
			// Cleanup text can contain unbounded tool output. Keep the status
			// diagnostics here; the original ledger remains the inspection source.
			if call.CleanupError != "" {
				call.CleanupError = "inspect original child cleanup record"
			}
		}
	}
	return e
}

func dispositionNextAction(code string) string {
	switch code {
	case "child_active", "task_running", "child_execution_open":
		return "检查 child_run_id 及其当前 request_id/calls。任务仍在执行时使用 worker_wait；决定停止本次尝试时使用 worker_cancel。处于 waiting_tool 时，读取原始结果并独立核对实际副作用，使用 worker_recover 核对清理回执，清理结算后通过 worker_resume 交回原 Worker 继续处理未知结果。仅在当前 visit 的执行与清理均结算后重试 task_dispose；禁止重放 uncertain 调用。"
	case "manual_outcome_uncertain":
		return "使用 plan 和 worker_history 检查原始人工认领及父运行的动作记录，向原认领者核实执行结果与清理观测。此认领没有子运行，worker_resume、worker_cancel 和 task_retry 均不会结算该认领。结果仍未知时继续阻断处置；禁止重置认领或重放副作用。"
	case "child_action_uncertain":
		return "读取子运行的原始结果及 request_id/call_id，独立核对实际副作用；保留原未知结果，不将新观测当作清理证明。使用 worker_recover 核对清理回执，清理结算后通过 worker_resume 交回原 Worker 自动处理；待执行与清理均结算后重试 task_dispose。禁止重放 uncertain 调用。"
	case "worker_unsettled", "child_settlement_uncommitted", "child_cleanup_unresolved", "descendants_unsettled":
		return "检查 worker_status、visit、settled_visit 及原始子运行和后代运行的清理回执。任务仍在执行时等待结束，或在明确决定停止后取消；使用 worker_recover 核对已有清理观测，该操作不重放任务。先解决剩余清理失败，确认回执匹配当前 visit 且清理完成后，再重试 task_dispose。仅取消任务或记录人工观测，均不足以结算未知副作用。"
	case "child_identity_mismatch", "task_attempt_changed":
		return "重新加载父任务，检查已登记的 worker_id/child_run_id 及当前 visit。使用身份匹配的原始执行尝试，禁止替换 ID 或复用旧清理回执。仅在身份与结算记录一致后重试 task_dispose。"
	case "task_finished", "disposition_immutable":
		return "检查任务现有结果，保留已完成的工作及已记录的处置。只有与已有处置完全相同的请求才可幂等重试。"
	case "replacement_invalid", "replacement_cycle":
		return "检查替代任务及其依赖，保留原始目标和验收约定。选择不会形成依赖或处置循环的替代任务后，再重试 task_dispose。"
	default:
		return "检查原始错误及父子运行记录，解决已报告的访问或输入问题后，再重试 task_dispose。禁止重放动作或绕过执行、身份及清理检查。"
	}
}
