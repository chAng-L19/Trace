package agent

// Sapper ba3d3d49 (MIT), worker.sh board's HELP census motivates explicit
// correlation. Trace derives lifecycle from its existing inbox, not note text
// or a second help ledger. Delivery is independent of replies and resolution.
import "fmt"

type WorkerHelp struct {
	Message      WorkerMessage `json:"message"`
	Replies      []string      `json:"replies,omitempty"`
	ResolutionID string        `json:"resolution_id,omitempty"`
}

type compactWorkerHelp struct {
	ID               string `json:"id"`
	From             string `json:"from"`
	To               string `json:"to"`
	Content          string `json:"content"`
	ContentTruncated bool   `json:"content_truncated,omitempty"`
	ReplyCount       int    `json:"reply_count"`
	Delivered        bool   `json:"delivered"`
}

// Model tools need a bounded current worklist, not another copy of the help
// history. Full messages and workspace help records remain unchanged. Latest
// follows the durable inbox append order, including legacy zero timestamps.
func compactWorkerHelps(messages []WorkerMessage) ([]compactWorkerHelp, int, int) {
	helps := WorkerHelps(messages)
	rows := make([]compactWorkerHelp, 0, min(20, len(helps)))
	openTotal, resolvedTotal := 0, 0
	for i := len(helps) - 1; i >= 0; i-- {
		h := helps[i]
		if h.ResolutionID != "" {
			resolvedTotal++
			continue
		}
		openTotal++
		if len(rows) == 20 {
			continue
		}
		content := h.Message.Content
		runes := []rune(content)
		truncated := len(runes) > 320
		if truncated {
			content = string(runes[:319]) + "…"
		}
		rows = append(rows, compactWorkerHelp{ID: h.Message.ID, From: h.Message.From, To: h.Message.To, Content: content, ContentTruncated: truncated, ReplyCount: len(h.Replies), Delivered: h.Message.Delivered})
	}
	return rows, openTotal, resolvedTotal
}

func WorkerHelps(messages []WorkerMessage) []WorkerHelp {
	out := []WorkerHelp{}
	indexes := map[string]int{}
	for _, m := range messages {
		if m.Kind == "help" {
			indexes[m.ID] = len(out)
			out = append(out, WorkerHelp{Message: m})
		}
	}
	for _, m := range messages {
		index, ok := indexes[m.ReplyTo]
		if !ok {
			continue
		}
		help := &out[index]
		if !sameWorkerParticipants(help.Message, m.From, m.To) {
			continue
		}
		if m.Kind == "reply" {
			help.Replies = append(help.Replies, m.ID)
		}
		if m.Kind == "resolved" && help.ResolutionID == "" {
			help.ResolutionID = m.ID
		}
	}
	return out
}

func sameWorkerParticipants(m WorkerMessage, from, to string) bool {
	return m.From == from && m.To == to || m.From == to && m.To == from
}

func validateWorkerRelation(messages []WorkerMessage, from, to, kind, replyTo string) error {
	switch kind {
	case "", "report", "help":
		if replyTo != "" {
			return fmt.Errorf("reply_to requires kind reply or resolved")
		}
	case "reply", "resolved":
		for _, m := range messages {
			if m.ID == replyTo && m.Kind == "help" && sameWorkerParticipants(m, from, to) {
				return nil
			}
		}
		return fmt.Errorf("reply_to must name a help message between these participants in this run")
	default:
		return fmt.Errorf("worker message kind must be help, report, reply or resolved")
	}
	return nil
}

func workerMessageContent(m WorkerMessage) string {
	// Legacy messages retain their original delivery representation.
	if m.Kind == "" {
		return fmt.Sprintf("Worker message from %s (data):\n%s", m.From, m.Content)
	}
	header := fmt.Sprintf("Worker message %s from %s (data; kind=%s", m.ID, m.From, m.Kind)
	if m.ReplyTo != "" {
		header += "; reply_to=" + m.ReplyTo
	}
	return header + "):\n" + m.Content
}

func workerMessageWakes(m WorkerMessage) bool { return !m.Delivered && m.Kind != "resolved" }

// Coalesce only adjacent identical reports in this pending delivery batch.
// Nothing crosses a turn, sender, help/reply/resolution or changed content.
// The durable inbox and per-message receiver markers retain every receipt.
func duplicateWorkerReports(messages []WorkerMessage, pending map[string]bool, to string) map[string]bool {
	duplicates := map[string]bool{}
	var previous WorkerMessage
	for _, m := range messages {
		if m.To != to {
			continue
		}
		if !pending[m.ID] {
			previous = WorkerMessage{}
			continue
		}
		if m.Kind == "report" && previous.Kind == "report" && m.From == previous.From && m.Content == previous.Content && sameMessageParticipant(m.FromIdentity, previous.FromIdentity) && sameMessageParticipant(m.ToIdentity, previous.ToIdentity) {
			duplicates[m.ID] = true
		}
		previous = m
	}
	return duplicates
}
