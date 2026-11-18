package agent

import (
	"fmt"
	"strings"
	"trace/internal/worker"
)

// ToolDiagnostic explains a result; it never resolves outcomes or proves cleanup.
// Append-only optional metadata keeps legacy ToolResult evidence bytes intact.
type ToolDiagnostic struct {
	Code                   string                `json:"code"`
	Summary                string                `json:"summary"`
	NextAction             string                `json:"next_action"`
	ResourceMode           string                `json:"resource_mode,omitempty"`
	PIDsLimit              int64                 `json:"pids_limit,omitempty"`
	MemoryLimitBytes       int64                 `json:"memory_limit_bytes,omitempty"`
	OutputLimitBytes       int64                 `json:"output_limit_bytes,omitempty"`
	RecommendedConcurrency int64                 `json:"recommended_concurrency,omitempty"`
	ControlRunID           string                `json:"control_run_id,omitempty"`
	ControlEventSequence   int                   `json:"control_event_sequence,omitempty"`
	TriggerRunID           string                `json:"trigger_run_id,omitempty"`
	TriggerEventSequence   int                   `json:"trigger_event_sequence,omitempty"`
	Blockers               *TaskDispositionError `json:"blockers,omitempty"`
}

func recommendedExecConcurrency(limits *worker.ResourceLimits) int64 {
	if limits.Mode != worker.ResourceModeCgroup {
		return 8
	}
	return max(int64(1), min(int64(16), (limits.PIDs-2)/2))
}

func execResourceEnvironment(limits *worker.ResourceLimits) []string {
	return []string{fmt.Sprintf("TRACE_EXEC_RECOMMENDED_CONCURRENCY=%d", recommendedExecConcurrency(limits))}
}

func execResultDiagnostic(result ToolResult, limits *worker.ResourceLimits) *ToolDiagnostic {
	if result.WorkerResourceLimit == "" {
		return nil
	}
	d := &ToolDiagnostic{Code: "exec_resource_limit", Summary: "命令达到执行资源上限。", NextAction: "读取已保存的输出并用独立观测核对实际副作用，自动调整后续操作；保留原调用未知状态，不重放原调用，也不要求人工确认。", ResourceMode: limits.Mode, OutputLimitBytes: limits.OutputBytes, RecommendedConcurrency: recommendedExecConcurrency(limits)}
	if limits.Mode == worker.ResourceModeCgroup {
		d.PIDsLimit, d.MemoryLimitBytes = limits.PIDs, limits.MemoryBytes
	}
	if limits.Mode == worker.ResourceModeCgroup && strings.Contains(result.WorkerResourceLimit, "pids.events") {
		d.Summary = fmt.Sprintf("进程和线程达到单命令上限 %d；Python 线程池同样计数。", limits.PIDs)
		d.NextAction = fmt.Sprintf("自动核对本次部分结果；后续脚本使用 TRACE_EXEC_RECOMMENDED_CONCURRENCY（本次建议 %d），分批保存进度，跳过已确认完成项；不重放未知调用。", d.RecommendedConcurrency)
	}
	if limits.Mode == worker.ResourceModeCgroup && strings.Contains(result.WorkerResourceLimit, "memory.events") {
		d.Summary += fmt.Sprintf(" 记录到内存 OOM 事件；本次内存上限 %d 字节。", limits.MemoryBytes)
	}
	if strings.Contains(result.WorkerResourceLimit, "output_bytes") {
		d.Summary += fmt.Sprintf(" 标准输出和标准错误达到合计 %d 字节上限。", limits.OutputBytes)
	}
	return d
}

func scriptResultDiagnostic(result ToolResult) *ToolDiagnostic {
	if result.WorkerStatus != worker.StatusFailed || !strings.Contains(result.Stderr, "TypeError:") {
		return nil
	}
	d := &ToolDiagnostic{Code: "script_type_error", Summary: "脚本类型错误，查看 traceback 中的具体表达式。", NextAction: "先用小输入核对出错表达式和参数类型；修正后验证相关函数，并核对之前已执行的操作。"}
	if strings.Contains(result.Stderr, "must be real number, not str") {
		d.NextAction += " 若表达式对含百分号编码的 URL 使用 % 插值，改用拼接或 f-string，避免 %3F 被当作浮点格式符；不要仅据错误文字转换参数类型。"
	}
	return d
}

// Correlate only an observed cancelled command with a durable control event
// inside this action's execution window. This is correlation, not a process cause.
func controlResultDiagnostic(s *Snapshot, action Action) *ToolDiagnostic {
	if action.Result == nil || action.Result.WorkerStatus != worker.StatusCancelled || action.StartedAt.IsZero() || action.Result.executionEndedAt.IsZero() {
		return nil
	}
	for i := len(s.Events) - 1; i >= 0; i-- {
		e := s.Events[i]
		if e.Sequence <= 0 || e.CreatedAt.Before(action.StartedAt) || e.CreatedAt.After(action.Result.executionEndedAt) || e.Type != "run_paused" && e.Type != "run_cancelled" {
			continue
		}
		d := &ToolDiagnostic{Code: "control_paused", Summary: "命令中断，执行窗口内存在暂停事件。", NextAction: "核对保留的输出与外部状态；清理完成后按原 Worker 身份继续，未知调用不自动重放。", ControlRunID: s.Run.ID, ControlEventSequence: e.Sequence}
		if e.Type == "run_cancelled" {
			d.Code, d.Summary = "control_cancelled", "命令中断，执行窗口内存在取消事件。"
		}
		// Event data is typed in-memory and generic after persistence.
		if data, ok := e.Data.(map[string]any); ok {
			d.TriggerRunID, _ = data["parent_run_id"].(string)
			switch seq := data["parent_event_sequence"].(type) {
			case float64:
				d.TriggerEventSequence = int(seq)
			case int:
				d.TriggerEventSequence = seq
			}
		}
		if d.TriggerRunID != "" {
			d.Summary = "命令中断，关联到父会话暂停事件。"
		}
		return d
	}
	return nil
}

func parentPauseEventData(parent Snapshot) map[string]any {
	data := map[string]any{"parent_run_id": parent.Run.ID}
	if parent.Run.Status != StatusPaused {
		return data
	}
	var eventType string
	switch parent.Run.PauseReason {
	case "operator_pause", "parent_pause":
		eventType = "run_paused"
	case "tool outcome uncertain; inspect side effects and submit its result manually":
		eventType = "action_uncertain"
	default:
		return data
	}
	for i := len(parent.Events) - 1; i >= 0; i-- {
		if parent.Events[i].Type == eventType {
			data["parent_event_sequence"] = parent.Events[i].Sequence
			break
		}
		switch parent.Events[i].Type {
		case "run_started", "run_cancelled", "run_paused", "action_uncertain", "budget_paused", "context_paused", "completion_validation_blocked":
			return data
		}
	}
	return data
}
