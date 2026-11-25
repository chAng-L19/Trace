package agent

import "context"

func (a *Agent) finishPiTurn(ctx context.Context, id string, request ModelRequest, response ModelResponse) (bool, error) {
	snap, err := a.store.Update(id, func(s *Snapshot) (updateErr error) {
		if isStopped(s.Run.Status) || s.Run.Status == StatusFailed || s.Run.Status == StatusCompleted {
			return nil
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		defer func() {
			if updateErr == nil && ctx.Err() != nil {
				updateErr = context.Cause(ctx)
			}
		}()
		if len(pendingToolCalls(s)) > 0 {
			return nil
		}
		if len(response.ToolCalls) > 0 {
			return nil
		}
		requestID := request.ID
		delivering := reportDeliveryPending(s)
		if !delivering {
			steered := consumeMailbox(s, "steer")
			followed := consumeMailbox(s, "follow_up")
			if steered || followed {
				s.Run.Status = StatusRunning
				return nil
			}
			pending, err := a.pendingWorkerInput(s)
			if err != nil {
				return err
			}
			if pending {
				s.Run.Status = StatusRunning
				addEvent(s, "completion_deferred", "Worker 消息待处理，继续运行", nil)
				return nil
			}
		}
		s.Run.Status = StatusCompleted
		s.Run.Error = ""
		if !reportDeliveryPending(s) && continueForEvidence(s) {
			return nil
		}
		if pending, err := a.deferForChildReportDelivery(s); err != nil || pending {
			return err
		}
		if continueForReport(s) {
			return nil
		}
		// Sources may have been revoked while translation was pending. Once
		// delivery clears, apply the original business audit before terminating.
		if delivering && continueForEvidence(s) {
			return nil
		}
		// A report-only turn may have just completed delivery. Deliver any
		// queued business input only after the report gate is gone.
		if delivering {
			if a.reportWorkerVisit(s) {
				addEvent(s, "run_completed", "报告补交已完成；保留排队业务输入供独立恢复处理", map[string]string{"request_id": requestID, "prompt_hash": s.Run.PromptHash})
				return nil
			}
			steered := consumeMailbox(s, "steer")
			followed := consumeMailbox(s, "follow_up")
			if steered || followed {
				s.Run.Status = StatusRunning
				return nil
			}
			pending, err := a.pendingWorkerInput(s)
			if err != nil {
				return err
			}
			if pending {
				s.Run.Status = StatusRunning
				addEvent(s, "completion_deferred", "报告交付完成，继续处理待投递 Worker 消息", nil)
				return nil
			}
		}
		addEvent(s, "run_completed", "模型回复已保存", map[string]string{"request_id": requestID, "prompt_hash": s.Run.PromptHash})
		return nil
	})
	if err != nil {
		return false, err
	}
	if err := a.publishWorkerResult(id); err != nil {
		return false, err
	}
	return isStopped(snap.Run.Status) || snap.Run.Status == StatusCompleted || snap.Run.Status == StatusFailed, nil
}
