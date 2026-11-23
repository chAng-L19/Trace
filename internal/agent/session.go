package agent

import (
	"fmt"
	"strings"
	"time"
)

type QueuedMessage struct {
	OperationID string            `json:"operation_id,omitempty"`
	RequestHash string            `json:"request_hash,omitempty"`
	Revision    int               `json:"revision,omitempty"`
	CancelledAt time.Time         `json:"cancelled_at,omitempty"`
	Attachments []InputAttachment `json:"attachments,omitempty"`
	ID          string            `json:"id"`
	Kind        string            `json:"kind"`
	Content     string            `json:"content"`
	CreatedAt   time.Time         `json:"created_at"`
	ConsumedAt  time.Time         `json:"consumed_at,omitempty"`
	MessageID   string            `json:"message_id,omitempty"`
}

func (q QueuedMessage) Pending() bool { return q.ConsumedAt.IsZero() && q.CancelledAt.IsZero() }

func assignMessageIDs(s *Snapshot) {
	parent := ""
	for i := range s.Messages {
		m := &s.Messages[i]
		if m.ID == "" {
			m.ID = fmt.Sprintf("%s-m%06d", s.Run.ID, i+1)
		}
		if m.ParentID == "" {
			m.ParentID = parent
		}
		parent = m.ID
	}
}

// QueueMessage persists without acquiring the execution lock; another process can steer a running model.
func (a *Agent) QueueMessage(id, kind, content string, attachments ...InputAttachment) (Snapshot, error) {
	s, _, _, err := a.QueueMessageOperation(id, kind, "", content, attachments...)
	return s, err
}

// QueueMessageOperation commits the operation identity with its input. A repeated
// identity returns the receipt, even after delivery/edit/cancel, never replays it.
// Reference: Muteki platform/outbox.py:34 (144df766, AGPL-3.0), independently
// implemented with Trace's existing atomic Store instead of a second outbox.
func (a *Agent) QueueMessageOperation(id, kind, operationID, content string, attachments ...InputAttachment) (Snapshot, QueuedMessage, bool, error) {
	operationKind := kind
	if kind == "message" {
		kind = "steer"
	}
	if kind != "steer" && kind != "follow_up" {
		return Snapshot{}, QueuedMessage{}, false, fmt.Errorf("message kind must be steer or follow_up")
	}
	if strings.TrimSpace(content) == "" {
		return Snapshot{}, QueuedMessage{}, false, fmt.Errorf("message is required")
	}
	if len(operationID) > 200 || strings.ContainsAny(operationID, "/\\\r\n") {
		return Snapshot{}, QueuedMessage{}, false, fmt.Errorf("invalid operation_id")
	}
	attachments, err := a.resolveInputAttachments(attachments, false)
	if err != nil {
		return Snapshot{}, QueuedMessage{}, false, err
	}
	attachmentIDs := make([]string, len(attachments))
	for i, attachment := range attachments {
		attachmentIDs[i] = attachment.ID
	}
	hash := valueHash(struct {
		Kind, Content string
		Attachments   []string
	}{operationKind, content, attachmentIDs})
	mid, err := newID()
	if err != nil {
		return Snapshot{}, QueuedMessage{}, false, err
	}
	var receipt QueuedMessage
	created := false
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		if operationID != "" {
			for _, q := range s.Mailbox {
				if q.OperationID != operationID {
					continue
				}
				if q.RequestHash != hash {
					return fmt.Errorf("operation_id already belongs to different input")
				}
				receipt = q
				return nil
			}
		}
		if s.Run.Status == StatusCancelled {
			return fmt.Errorf("run is cancelled")
		}
		if operationKind == "message" && (s.Run.Status == StatusPaused || s.Run.Status == StatusRunning || s.Run.Status == StatusWaitingTool) {
			return fmt.Errorf("run is %s; use a queued steer or follow-up", s.Run.Status)
		}
		for i := range attachments {
			attachments[i].SourceRunID = id
			attachments[i].SourceMessageID = mid
		}
		receipt = QueuedMessage{ID: mid, OperationID: operationID, RequestHash: hash, Revision: 1, Kind: kind, Content: content, Attachments: attachments, CreatedAt: time.Now().UTC()}
		s.Mailbox = append(s.Mailbox, receipt)
		created = true
		// Idle queues are durable and become runnable via Run; active queues need no second owner.
		if s.Run.Status == StatusCompleted || s.Run.Status == StatusFailed {
			s.Run.Status = StatusCreated
			s.Run.Error = ""
		}
		addEvent(s, "message_queued", "指令已加入持久队列", map[string]string{"id": mid, "kind": kind})
		return nil
	})
	return snap, receipt, created, err
}

func consumeMailbox(s *Snapshot, kind string) bool {
	consumed := false
	for i := range s.Mailbox {
		q := &s.Mailbox[i]
		if q.Kind != kind || !q.Pending() {
			continue
		}
		q.ConsumedAt = time.Now().UTC()
		q.MessageID = q.ID
		s.Messages = append(s.Messages, Message{ID: q.ID, Role: "user", Content: q.Content, Attachments: q.Attachments, CreatedAt: q.ConsumedAt})
		addEvent(s, "message_consumed", "队列指令已加入接收方历史，模型请求归档另行记录", map[string]string{"id": q.ID, "kind": kind})
		consumed = true
	}
	return consumed
}

// Branch copies a complete message prefix into a new independent run. Original history is untouched.
func (a *Agent) Branch(id, messageID string) (Snapshot, error) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	release, err := a.store.acquireStoreLock()
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	source, err := a.store.loadLocked(id)
	if err != nil {
		return Snapshot{}, err
	}
	if source.Run.Deleted {
		return Snapshot{}, fmt.Errorf("会话已隐藏")
	}
	assignMessageIDs(&source)
	end := -1
	for i, m := range source.Messages {
		if m.ID == messageID {
			end = i + 1
			break
		}
	}
	if end < 0 {
		return Snapshot{}, fmt.Errorf("message not found: %s", messageID)
	}
	prefix := source.Messages[:end]
	if len(pendingToolCalls(&Snapshot{Messages: prefix})) > 0 {
		return Snapshot{}, fmt.Errorf("branch point has unresolved tool calls; select the final tool result")
	}
	branch, err := a.store.createLocked(source.Run.Goal, source.Prompt, source.Run.PromptHash)
	if err != nil {
		return Snapshot{}, err
	}
	return a.store.updateLocked(branch.Run.ID, func(s *Snapshot) error {
		// An independent branch is a root. Historical sources retain legacy
		// semantics; branches of role-aware runs must not shed admission.
		s.Run.Responsibility = source.Run.Responsibility
		if s.Run.Responsibility != "" {
			s.Run.Responsibility = ResponsibilityCoordinator
		}
		s.Run.ModelSelection = source.Run.ModelSelection
		s.Run.ParentRunID = id
		s.Run.ParentMessageID = messageID
		s.Run.Budget = source.Run.Budget
		s.Run.BudgetConfigured = source.Run.BudgetConfigured
		s.Run.Context = source.Run.Context
		s.Run.Exec, s.Run.ToolSchema = source.Run.Exec, source.Run.ToolSchema
		s.Run.ToolSelection = source.Run.ToolSelection
		s.Run.SuccessContains = source.Run.SuccessContains
		s.Requirements = append([]VerificationRequirement(nil), source.Requirements...)
		s.Messages = append([]Message(nil), prefix...)
		// Inherited observations remain in the messages. Their original evidence
		// provenance belongs to ParentRunID, never to newly executed branch actions.
		addEvent(s, "session_branched", "已从消息创建分支", map[string]string{"parent_run_id": id, "parent_message_id": messageID})
		return nil
	})
}

func (a *Agent) ReadMessage(id, messageID string) (Message, error) {
	s, err := a.store.Load(id)
	if err != nil {
		return Message{}, err
	}
	assignMessageIDs(&s)
	for _, message := range s.Messages {
		if message.ID == messageID {
			return message, nil
		}
	}
	return Message{}, fmt.Errorf("message not found: %s", messageID)
}
