package agent

import "trace/internal/textutil"

// Original lengths survive the detached preview so a subsequent HTTP render
// never mistakes already-trimmed text for the complete authoritative message.
type DisplayMessagePreview struct {
	Preview       bool
	ContentBytes  int
	ThinkingBytes int
}

// These are the existing workspace HTTP preview limits, applied before cache
// admission. Identity, lifecycle and artifact metadata stay intact; full text
// and duplicated provider payloads remain available through record details.
func trimActivityDisplay(p *ActivityProjection) {
	s := &p.Snapshot
	if p.MessagePreviews == nil {
		p.MessagePreviews = make(map[string]DisplayMessagePreview, len(s.Messages))
	}
	pageRequests, archived := map[string]bool{}, map[string]bool{}
	toolBudget := 80
	for i := range s.Messages {
		m := &s.Messages[i]
		meta, indexed := p.MessagePreviews[m.ID]
		if !indexed {
			meta = DisplayMessagePreview{ContentBytes: len(m.Content), ThinkingBytes: len(m.Thinking)}
		}
		m.Content, m.Thinking = textutil.TruncateUTF8(m.Content, 4096), textutil.TruncateUTF8(m.Thinking, 4096)
		meta.Preview = meta.Preview || len(m.Content) != meta.ContentBytes || len(m.Thinking) != meta.ThinkingBytes
		if len(m.ToolCalls) > toolBudget {
			m.ToolCalls = m.ToolCalls[:toolBudget]
			meta.Preview = true
		}
		toolBudget -= len(m.ToolCalls)
		for j := range m.ToolCalls {
			args := m.ToolCalls[j].Arguments
			m.ToolCalls[j].Arguments = textutil.TruncateUTF8(args, 2048)
			meta.Preview = meta.Preview || m.ToolCalls[j].Arguments != args
		}
		p.MessagePreviews[m.ID] = meta
		if m.RequestID != "" {
			pageRequests[m.RequestID] = true
			if m.Role == "assistant" {
				archived[m.RequestID] = true
			}
		}
	}
	for i := range s.Responses {
		r := &s.Responses[i]
		if archived[r.RequestID] {
			r.Response.Content, r.Response.Thinking, r.Response.ToolCalls = "", "", nil
		}
		r.Response.Content = textutil.TruncateUTF8(r.Response.Content, 4096)
		r.Response.Thinking = textutil.TruncateUTF8(r.Response.Thinking, 4096)
	}
	for i := range s.Actions {
		a := &s.Actions[i]
		a.Arguments = textutil.TruncateUTF8(a.Arguments, 2048)
		if !pageRequests[a.RequestID] && a.Result != nil {
			a.Arguments = ""
		}
		if a.Result != nil {
			r := a.Result
			if pageRequests[a.RequestID] {
				r.Stdout, r.Stderr = textutil.TruncateUTF8(r.Stdout, 4096), textutil.TruncateUTF8(r.Stderr, 4096)
			} else {
				r.Stdout, r.Stderr = "", ""
			}
			r.MCPResult, r.LegacyREASession, r.LegacyREACheckpoint, r.LegacyREARestore = nil, nil, nil, nil
			r.Error = textutil.TruncateUTF8(r.Error, 2048)
		}
	}
	for i := range s.WorkerMessages {
		s.WorkerMessages[i].Content = textutil.TruncateUTF8(s.WorkerMessages[i].Content, 2048)
	}
}
