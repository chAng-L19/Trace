package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"trace/internal/agent"
)

type workspaceMessage struct {
	agent.Message
	Preview       bool `json:"preview,omitempty"`
	ContentBytes  int  `json:"content_bytes,omitempty"`
	ThinkingBytes int  `json:"thinking_bytes,omitempty"`
}

type workspaceView struct {
	agent.Snapshot
	Messages       []workspaceMessage     `json:"messages"`
	Coverage       *agent.CoverageSummary `json:"coverage,omitempty"`
	Result         *agent.Result          `json:"result,omitempty"`
	EventTotal     int                    `json:"event_total"`
	MessagesPage   agent.MessagePage      `json:"messages_page"`
	WorkerActivity []agent.WorkerActivity `json:"worker_activity,omitempty"`
	HelpRequests   []agent.WorkerHelp     `json:"help_requests,omitempty"`
	Totals         map[string]int         `json:"totals,omitempty"`
	Projection     string                 `json:"projection,omitempty"`
}

func workspaceText(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit]
}

func conversationRequest(r agent.ModelRequest) bool {
	return r.Purpose == "" || r.Purpose == "conversation"
}

func sameAssistant(m agent.Message, r agent.ResponseRecord) bool {
	if r.Error != "" || m.Content != r.Response.Content || m.Thinking != r.Response.Thinking || len(m.ToolCalls) != len(r.Response.ToolCalls) {
		return false
	}
	for i, call := range m.ToolCalls {
		if call != r.Response.ToolCalls[i] {
			return false
		}
	}
	return true
}

// Old ledgers lack message.request_id. Bind only a unique ordinary response
// consistent with the request's history boundary and recorded timestamps.
func historicalRequest(snap agent.Snapshot, requests map[string]agent.ModelRequest, index int, used map[string]bool) string {
	m := snap.Messages[index]
	found := ""
	for _, response := range snap.Responses {
		request, ok := requests[response.RequestID]
		if !ok || !conversationRequest(request) || used[request.ID] || !sameAssistant(m, response) {
			continue
		}
		if !m.CreatedAt.IsZero() && ((!request.CreatedAt.IsZero() && m.CreatedAt.Before(request.CreatedAt)) || (!response.CreatedAt.IsZero() && m.CreatedAt.Before(response.CreatedAt))) {
			continue
		}
		boundary := -1
		for _, prior := range request.Messages {
			if prior.ID == "" {
				continue
			}
			for j, original := range snap.Messages {
				if original.ID == prior.ID && j > boundary {
					boundary = j
				}
			}
		}
		if boundary >= index {
			continue
		}
		if boundary >= 0 {
			first := boundary + 1
			for first < len(snap.Messages) && snap.Messages[first].Role != "assistant" {
				first++
			}
			if first != index {
				continue
			}
		}
		if found != "" {
			return ""
		}
		found = request.ID
	}
	return found
}

// Keep the authoritative snapshot and evidence contract intact. Only this
// browser projection omits duplicated request histories and archived streams.
func workspaceSnapshot(snap agent.Snapshot, activity ...[]agent.WorkerActivity) any {
	view, _ := workspaceSnapshotPage(snap, agent.MessagePageQuery{}, activity...)
	return view
}

func workspaceSnapshotPage(snap agent.Snapshot, query agent.MessagePageQuery, activity ...[]agent.WorkerActivity) (any, error) {
	return projectWorkspacePage(snap, query, false, activity...)
}

func projectWorkspacePage(snap agent.Snapshot, query agent.MessagePageQuery, bounded bool, activity ...[]agent.WorkerActivity) (any, error) {
	if !bounded {
		snap.Reports = agent.ReportReceipts(snap)
	}
	var workers []agent.WorkerActivity
	if len(activity) > 0 {
		workers = activity[0]
	}
	requests := append([]agent.ModelRequest(nil), snap.Requests...)
	summaries := map[string]bool{}
	for _, c := range snap.Compactions {
		if c.RequestID != "" {
			summaries[c.RequestID] = true
		}
	}
	byID := map[string]agent.ModelRequest{}
	for i := range requests {
		if summaries[requests[i].ID] {
			requests[i].Purpose = "context_summary"
		}
		byID[requests[i].ID] = requests[i]
	}
	snap.Messages = append([]agent.Message(nil), snap.Messages...)
	archived := map[string]bool{}
	owner := ""
	for i, m := range snap.Messages {
		if !bounded && m.Role == "assistant" && m.RequestID == "" {
			m.RequestID = historicalRequest(snap, byID, i, archived)
		}
		if m.Role == "assistant" {
			owner = m.RequestID
		}
		if m.Role == "user" {
			owner = ""
		}
		if m.Role == "tool" && m.RequestID == "" {
			m.RequestID = owner
		}
		snap.Messages[i] = m
		if m.Role == "assistant" && m.RequestID != "" {
			archived[m.RequestID] = true
		}
	}
	selected, page, err := agent.PageMessages(snap, query)
	if err != nil {
		return nil, err
	}
	messages := make([]workspaceMessage, len(selected))
	pageRequests := map[string]bool{}
	toolBudget := 80
	for i, m := range selected {
		messages[i] = workspaceMessage{Message: m, ContentBytes: len(m.Content), ThinkingBytes: len(m.Thinking)}
		messages[i].Content = workspaceText(m.Content, 4096)
		messages[i].Thinking = workspaceText(m.Thinking, 4096)
		messages[i].ToolCalls = append([]agent.ToolCall(nil), m.ToolCalls...)
		messages[i].Preview = messages[i].Content != m.Content || messages[i].Thinking != m.Thinking
		if len(messages[i].ToolCalls) > toolBudget {
			messages[i].ToolCalls = messages[i].ToolCalls[:toolBudget]
			messages[i].Preview = true
		}
		toolBudget -= len(messages[i].ToolCalls)
		for j := range messages[i].ToolCalls {
			arguments := messages[i].ToolCalls[j].Arguments
			messages[i].ToolCalls[j].Arguments = workspaceText(arguments, 2048)
			messages[i].Preview = messages[i].Preview || messages[i].ToolCalls[j].Arguments != arguments
		}
		if m.RequestID != "" {
			pageRequests[m.RequestID] = true
		}
	}
	for i := range requests {
		requests[i].Messages = nil
		requests[i].Tools = nil
	}
	if bounded {
		latest := ""
		if len(requests) > 0 {
			latest = requests[len(requests)-1].ID
		}
		kept := make([]agent.ModelRequest, 0, len(pageRequests)+1)
		for _, request := range requests {
			if pageRequests[request.ID] || request.ID == latest {
				kept = append(kept, request)
			}
		}
		requests = kept
	}
	responses := append([]agent.ResponseRecord(nil), snap.Responses...)
	settled := map[string]bool{}
	for i := range responses {
		settled[responses[i].RequestID] = responses[i].Error == ""
		if archived[responses[i].RequestID] {
			responses[i].Response.Content = ""
			responses[i].Response.Thinking = ""
			responses[i].Response.ToolCalls = nil
		}
		responses[i].Response.Content = workspaceText(responses[i].Response.Content, 4096)
		responses[i].Response.Thinking = workspaceText(responses[i].Response.Thinking, 4096)
		if len(snap.Messages) > 80 && !pageRequests[responses[i].RequestID] && i < len(responses)-1 {
			responses[i].Response.Content = ""
			responses[i].Response.Thinking = ""
			responses[i].Response.ToolCalls = nil
		}
	}
	if bounded {
		kept := make([]agent.ResponseRecord, 0, len(pageRequests)+1)
		for i, response := range responses {
			if pageRequests[response.RequestID] || i == len(responses)-1 {
				kept = append(kept, response)
			}
		}
		responses = kept
	}
	events := agent.WorkspaceEvents(snap, settled, archived)
	// Actions retain complete identity/artifact metadata for the exploration and
	// artifact tabs. Only the current message window carries bounded text. Full
	// source detail and all verification still read the canonical Store.Load.
	actions := make([]agent.Action, 0)
	for _, action := range snap.Actions {
		if !bounded || pageRequests[action.RequestID] || action.Result == nil || action.Status == "uncertain" {
			actions = append(actions, action)
		}
	}
	for i := range actions {
		a := &actions[i]
		a.Arguments = workspaceText(a.Arguments, 2048)
		if !pageRequests[a.RequestID] && a.Result != nil {
			a.Arguments = ""
		}
		if a.Result != nil {
			r := *a.Result
			if pageRequests[a.RequestID] {
				r.Stdout = workspaceText(r.Stdout, 4096)
				r.Stderr = workspaceText(r.Stderr, 4096)
			} else {
				r.Stdout = ""
				r.Stderr = ""
			}
			r.MCPResult = nil
			r.LegacyREASession = nil
			r.LegacyREACheckpoint = nil
			r.LegacyREARestore = nil
			r.Error = workspaceText(r.Error, 2048)
			a.Result = &r
		}
	}
	snap.Prompt = ""
	snap.Messages = nil
	snap.Requests = requests
	snap.Responses = responses
	snap.Events = events
	snap.Actions = actions
	view := workspaceView{Snapshot: snap, Messages: messages, EventTotal: snap.Run.EventSeq, MessagesPage: page, WorkerActivity: workers, HelpRequests: agent.WorkerHelps(snap.WorkerMessages)}
	if !bounded {
		coverage := agent.Coverage(snap)
		view.Coverage = &coverage
		result := snap.Result
		view.Result = &result
	}
	return view, nil
}

func (s *Server) messageHistory(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	query := agent.MessagePageQuery{Before: q.Get("before"), After: q.Get("after"), Around: q.Get("around"), Request: q.Get("request"), Call: q.Get("call")}
	if q.Get("limit") != "" {
		n, err := strconv.Atoi(q.Get("limit"))
		if err != nil || n < 1 {
			writeError(w, 400, errors.New("invalid limit"))
			return
		}
		query.Limit = n
	}
	modes := 0
	for _, value := range []string{query.Before, query.After, query.Around} {
		if value != "" {
			modes++
		}
	}
	if modes > 1 || modes > 0 && (query.Request != "" || query.Call != "") {
		writeError(w, 400, errors.New("choose one message cursor"))
		return
	}
	projection, err := s.store.MessageWorkspace(r.Context(), r.PathValue("id"), query)
	if err != nil {
		if errors.Is(err, os.ErrInvalid) {
			writeError(w, 400, err)
			return
		}
		respond(w, nil, err)
		return
	}
	view, err := s.activityWorkspaceContext(r.Context(), projection, false)
	if errors.Is(err, os.ErrInvalid) {
		writeError(w, 400, err)
		return
	}
	if err == nil {
		view = s.executionWorkspace(view)
	}
	respond(w, view, err)
}

func (s *Server) getWorkspaceAction(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.Load(r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if snap.Run.Deleted {
		respond(w, nil, os.ErrNotExist)
		return
	}
	request, call := r.URL.Query().Get("request"), r.PathValue("call")
	var found *agent.Action
	for i := range snap.Actions {
		a := &snap.Actions[i]
		if a.CallID != call || request != "" && a.RequestID != request {
			continue
		}
		if found != nil {
			writeError(w, 400, errors.New("call ID is reused; request is required"))
			return
		}
		found = a
	}
	if found == nil {
		requests := map[string]agent.ModelRequest{}
		used := map[string]bool{}
		for _, record := range snap.Requests {
			requests[record.ID] = record
		}
		for i, m := range snap.Messages {
			if m.Role != "assistant" {
				continue
			}
			owner := m.RequestID
			if owner == "" {
				owner = historicalRequest(snap, requests, i, used)
			}
			if owner != "" {
				used[owner] = true
			} else {
				owner = agent.DisplayMessageID(snap.Run.ID, m, i)
			}
			if request != "" && owner != request {
				continue
			}
			for _, tool := range m.ToolCalls {
				if tool.ID != call {
					continue
				}
				if found != nil {
					writeError(w, 400, errors.New("call ID is reused; request is required"))
					return
				}
				action := agent.Action{RequestID: owner, CallID: call, Name: tool.Name, Arguments: tool.Arguments, Status: "waiting_tool"}
				for _, reply := range snap.Messages[i+1:] {
					if reply.Role == "assistant" || reply.Role == "user" {
						break
					}
					if reply.Role != "tool" || reply.ToolCallID != call {
						continue
					}
					result := agent.ToolResult{}
					if json.Unmarshal([]byte(reply.Content), &result) != nil {
						result.Stdout = reply.Content
					}
					action.Result = &result
					action.Status = "completed"
					break
				}
				found = &action
			}
		}
	}
	if found == nil {
		respond(w, nil, os.ErrNotExist)
		return
	}
	respond(w, found, nil)
}

func (s *Server) getModelRequest(w http.ResponseWriter, r *http.Request) {
	snap, err := s.store.Load(r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	if snap.Run.Deleted {
		respond(w, nil, os.ErrNotExist)
		return
	}
	for _, request := range snap.Requests {
		if request.ID == r.PathValue("request") {
			respond(w, request, nil)
			return
		}
	}
	respond(w, nil, os.ErrNotExist)
}

func (s *Server) eventHistory(w http.ResponseWriter, r *http.Request) {
	limit := 100
	before := int(^uint(0) >> 1)
	for name, dest := range map[string]*int{"limit": &limit, "before": &before} {
		if text := r.URL.Query().Get(name); text != "" {
			n, err := strconv.Atoi(text)
			if err != nil || n < 1 {
				writeError(w, 400, errors.New("invalid "+name))
				return
			}
			*dest = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	_, raw, err := s.store.EventBatch(r.PathValue("id"), 0)
	if err != nil {
		respond(w, nil, err)
		return
	}
	defer s.store.ReleaseEventView(r.PathValue("id"))
	typ := r.URL.Query().Get("type")
	query := strings.ToLower(r.URL.Query().Get("query"))
	summary := r.URL.Query().Get("summary") == "true"
	events := []agent.Event{}
	hasMore := false
	next := 0
	for i := len(raw) - 1; i >= 0; i-- {
		var event agent.Event
		if err = json.Unmarshal(raw[i], &event); err != nil {
			respond(w, nil, err)
			return
		}
		if event.Sequence >= before || (typ != "" && event.Type != typ) {
			continue
		}
		if summary && (event.Type == "model_stream" || event.Type == "tool_output" || event.Type == "task_claimed") {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(string(raw[i])), query) {
			continue
		}
		if len(events) == limit {
			hasMore = true
			break
		}
		events = append(events, event)
		next = event.Sequence
	}
	respond(w, map[string]any{"events": events, "next_before": next, "has_more": hasMore}, nil)
}
