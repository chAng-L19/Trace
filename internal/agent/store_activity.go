package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"
)

// ActivityProjection is a detached display window, never evidence acceptance.
type ActivityProjection struct {
	Snapshot        Snapshot
	MessagesPage    MessagePage
	Totals          map[string]int
	WorkerInbox     map[string]activityWorkerInbox
	HelpRequests    []WorkerHelp
	MessagePreviews map[string]DisplayMessagePreview
}

type activityWorkerInbox struct {
	PendingMessages int
	OpenHelpTotal   int
	OpenHelpIDs     []string
}

func (s *Store) displayWorkspace(ctx context.Context, id string, query MessagePageQuery, mode string) (ActivityProjection, error) {
	// Admission precedes the cache lookup and JSON allocation. Waiting reads
	// recheck the committed generation and reuse its detached projection.
	if err := s.lockDisplayRead(ctx); err != nil {
		return ActivityProjection{}, err
	}
	defer s.activityReadMu.Unlock()
	cache := mode == "activity"
	var v *storedView
	var records *activityRecords
	var err error
	var key activityCacheKey
	var previous *activityCacheEntry
	if s.sqlite {
		tx, run, generation, e := s.indexedSQLiteTransaction(ctx, id)
		if e != nil {
			return ActivityProjection{}, e
		}
		defer tx.Rollback()
		key = generation
		if cache {
			s.eventViewMu.Lock()
			previous = s.activityCache.entries[id]
			hit := previous != nil && (previous.key.version == key.version || previous.key.state == key.state)
			if hit {
				s.activityCache.clock++
				previous.used = s.activityCache.clock
				previous.key.version = key.version
			}
			s.eventViewMu.Unlock()
			if hit {
				return decodeActivity(id, previous.data, run)
			}
		}
		index, selected, e := readSQLiteDisplayIndex(ctx, tx, id)
		if e != nil {
			return ActivityProjection{}, e
		}
		v, records = index.view(mode), selected
		v.fields["run"] = run
	} else {
		cached, prior, e := s.cachedJSONActivity(id)
		if e != nil {
			return ActivityProjection{}, e
		}
		if cache && cached != nil {
			return decodeActivity(id, cached.data, cached.run)
		}
		previous = prior
		f, e := os.Open(filepath.Join(s.root, id+".json"))
		if e != nil {
			return ActivityProjection{}, e
		}
		defer f.Close()
		key.info, err = f.Stat()
		if err != nil {
			return ActivityProjection{}, err
		}
		index, selected, e := s.jsonDisplayIndex(ctx, id, f, false)
		if e != nil {
			return ActivityProjection{}, e
		}
		v, records = index.view(mode), selected
	}
	p, err := buildActivityPage(v.fields, records, query, mode)
	if err != nil {
		return ActivityProjection{}, err
	}
	if mode != "planner" {
		trimActivityDisplay(&p)
	}
	data, err := json.Marshal(p)
	if len(data) > activityProjectionMaxBytes {
		return ActivityProjection{}, errActivityTooLarge
	}
	if err != nil {
		return ActivityProjection{}, err
	}
	out, err := decodeActivity(id, data, v.fields["run"])
	if err != nil {
		return out, err
	}
	if err := ctx.Err(); err != nil {
		return ActivityProjection{}, err
	}
	if cache {
		s.cacheActivity(id, key, previous, data, v.fields["run"])
	}
	return out, nil
}

func (s *Store) lockDisplayRead(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.activityReadMu.TryLock() {
		return nil
	}
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if s.activityReadMu.TryLock() {
				return nil
			}
		}
	}
}

func decodeActivity(id string, data, run []byte) (ActivityProjection, error) {
	var out ActivityProjection
	if err := json.Unmarshal(data, &out); err != nil {
		return out, err
	}
	// Telemetry generations share only the immutable semantic projection.
	out.Snapshot.Run = Run{}
	if err := json.Unmarshal(run, &out.Snapshot.Run); err != nil {
		return out, err
	}
	if out.Snapshot.Run.ID != id {
		return out, fmt.Errorf("snapshot identity mismatch: %s", id)
	}
	if out.Snapshot.Run.Deleted {
		return out, os.ErrNotExist
	}
	return out, nil
}

type activityMessage struct {
	ID          string                `json:"id"`
	Role        string                `json:"role"`
	RequestID   string                `json:"request_id"`
	CreatedAt   time.Time             `json:"created_at"`
	ToolCallID  string                `json:"tool_call_id"`
	ContentHash string                `json:"display_content_hash,omitempty"`
	Preview     DisplayMessagePreview `json:"display_preview,omitempty"`
	ToolCalls   []struct {
		ID string `json:"id"`
	} `json:"tool_calls"`
}

func activityArray(fields map[string]json.RawMessage, name string) ([]snapshotJSON, error) {
	var rows []snapshotJSON
	if len(fields[name]) == 0 {
		return rows, nil
	}
	err := json.Unmarshal(fields[name], &rows)
	return rows, err
}

func buildActivityRecords(fields map[string]json.RawMessage, records *activityRecords) (ActivityProjection, error) {
	return buildActivityPage(fields, records, MessagePageQuery{}, "activity")
}

func buildActivityPage(fields map[string]json.RawMessage, records *activityRecords, query MessagePageQuery, mode string) (ActivityProjection, error) {
	if mode == "planner" {
		return buildPlannerDisplay(fields)
	}
	p := ActivityProjection{Totals: map[string]int{}, WorkerInbox: map[string]activityWorkerInbox{}}
	s := &p.Snapshot
	for _, item := range []struct {
		name   string
		target any
	}{{"run", &s.Run}, {"plan", &s.Plan}, {"workers", &s.Workers}, {"mailbox", &s.Mailbox}, {"root_contract", &s.RootContract}} {
		if len(fields[item.name]) > 0 {
			if err := json.Unmarshal(fields[item.name], item.target); err != nil {
				return p, err
			}
		}
	}
	arrays := map[string][]snapshotJSON{}
	for _, name := range []string{"messages", "requests", "responses", "actions", "assets", "child_reports", "reports", "worker_messages", "evidence_chains", "evidence_chain_refs", "compactions"} {
		rows, err := activityArray(fields, name)
		if err != nil {
			return p, err
		}
		arrays[name] = rows
	}
	for _, name := range []string{"assets", "actions", "requests", "messages", "child_reports"} {
		p.Totals[name] = len(arrays[name])
	}
	p.Totals["tasks"] = len(s.Plan.Tasks)
	p.Totals["workers"] = len(s.Workers)
	p.Totals["workers_occupying"] = occupiedWorkerSlots(s)
	p.Totals["artifacts"] = 0
	p.Totals["archives"] = 0
	reportSubjects := map[string]bool{}
	for i := len(arrays["reports"]) - 1; i >= 0; i-- {
		var report ReportArchive
		if err := json.Unmarshal(arrays["reports"][i], &report); err != nil {
			return p, err
		}
		key := report.Kind + "/" + report.SubjectID
		if reportSubjects[key] {
			continue
		}
		reportSubjects[key] = true
		if len(s.Reports) < 12 {
			report.SourceStatus = "not_checked"
			s.Reports = append([]ReportArchive{report}, s.Reports...)
		}
	}
	p.Totals["reports"] = len(reportSubjects)
	chains := map[string]bool{}
	for _, raw := range arrays["evidence_chains"] {
		var h struct {
			ID   string `json:"id"`
			Hash string `json:"content_hash"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return p, e
		}
		chains[s.Run.ID+"/"+h.ID+"/"+h.Hash] = true
	}
	for _, raw := range arrays["evidence_chain_refs"] {
		var h struct {
			Run  string `json:"source_run_id"`
			ID   string `json:"chain_id"`
			Hash string `json:"chain_hash"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return p, e
		}
		chains[h.Run+"/"+h.ID+"/"+h.Hash] = true
	}
	p.Totals["evidence_chains"] = len(chains)
	// Count the complete inbox before taking the body window. Help correlation
	// needs identities/participants, not every historical message's content.
	inboxHeaders := make([]WorkerMessage, 0, len(arrays["worker_messages"]))
	inboxRaw := map[string]snapshotJSON{}
	for _, raw := range arrays["worker_messages"] {
		var h struct {
			WorkerMessage
			Content snapshotJSON `json:"content"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return p, e
		}
		m := h.WorkerMessage
		inboxHeaders = append(inboxHeaders, m)
		inboxRaw[m.ID] = raw
		if !m.Delivered {
			row := p.WorkerInbox[m.To]
			row.PendingMessages++
			p.WorkerInbox[m.To] = row
		}
	}
	helps := WorkerHelps(inboxHeaders)
	for _, help := range helps {
		if help.ResolutionID != "" {
			continue
		}
		p.Totals["open_help"]++
		for _, who := range []string{help.Message.From, help.Message.To} {
			row := p.WorkerInbox[who]
			row.OpenHelpTotal++
			row.OpenHelpIDs = append(row.OpenHelpIDs, help.Message.ID)
			if len(row.OpenHelpIDs) > 20 {
				row.OpenHelpIDs = row.OpenHelpIDs[len(row.OpenHelpIDs)-20:]
			}
			p.WorkerInbox[who] = row
		}
	}
	// Keep the latest open help bodies separately from the latest inbox window.
	for i := len(helps) - 1; i >= 0 && len(p.HelpRequests) < 20; i-- {
		help := helps[i]
		if help.ResolutionID != "" {
			continue
		}
		if e := json.Unmarshal(inboxRaw[help.Message.ID], &help.Message); e != nil {
			return p, e
		}
		help.Message.Content = excerpt(help.Message.Content, 2048)
		p.HelpRequests = append(p.HelpRequests, help)
	}
	for _, raw := range arrays["worker_messages"][max(0, len(arrays["worker_messages"])-40):] {
		var m WorkerMessage
		if e := json.Unmarshal(raw, &m); e != nil {
			return p, e
		}
		s.WorkerMessages = append(s.WorkerMessages, m)
	}
	for _, raw := range arrays["child_reports"][max(0, len(arrays["child_reports"])-12):] {
		var r ChildReport
		if e := json.Unmarshal(raw, &r); e != nil {
			return p, e
		}
		s.ChildReports = append(s.ChildReports, r)
	}
	s.ChildReports = ChildReportReceipts(*s, 12)
	workers := s.Workers
	s.Workers = nil
	for _, w := range workers {
		if w.Status == "running" || w.Status == "starting" || w.Status == "cancelling" || w.Status == "paused" || w.Status == "uncertain" || w.Status == "failed" || w.CleanupStatus != "completed" {
			s.Workers = append(s.Workers, w)
		}
	}
	headers := make([]activityMessage, len(arrays["messages"]))
	legacy := false
	for i, raw := range arrays["messages"] {
		if e := json.Unmarshal(raw, &headers[i]); e != nil {
			return p, e
		}
		legacy = legacy || headers[i].Role == "assistant" && headers[i].RequestID == ""
	}
	if legacy {
		if records != nil && !records.indexed {
			var total int
			for i := range arrays["responses"] {
				raw, err := records.response(i)
				if err != nil {
					return p, err
				}
				total += len(raw)
				if total > activityProjectionMaxBytes {
					return p, errActivityTooLarge
				}
				arrays["responses"][i] = raw
			}
		}
		if e := repairActivityOwners(arrays, headers, records != nil && records.indexed); e != nil {
			return p, e
		}
	}
	// Modern tool messages can also lack ownership; use only preceding assistant.
	owner := ""
	for i := range headers {
		h := &headers[i]
		if h.Role == "assistant" {
			owner = h.RequestID
		}
		if h.Role == "user" {
			owner = ""
		}
		if h.Role == "tool" && h.RequestID == "" {
			h.RequestID = owner
		}
	}
	// Resolve the original ID/call cursor using only identity headers. Bodies
	// are decoded after selection, so a page never materializes the transcript.
	identities := Snapshot{Run: s.Run, Messages: make([]Message, len(headers))}
	for i, h := range headers {
		m := Message{ID: h.ID, Role: h.Role, RequestID: h.RequestID, ToolCallID: h.ToolCallID}
		for _, call := range h.ToolCalls {
			m.ToolCalls = append(m.ToolCalls, ToolCall{ID: call.ID})
		}
		identities.Messages[i] = m
	}
	_, page, err := PageMessages(identities, query)
	if err != nil {
		return p, err
	}
	p.MessagesPage = page
	wanted := map[string]bool{}
	// Seed latest/in-flight identities before selecting related actions.
	responseIDs := map[string]bool{}
	responseOwners := make([]string, len(arrays["responses"]))
	for i, raw := range arrays["responses"] {
		var h struct {
			RequestID string `json:"request_id"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return p, e
		}
		responseIDs[h.RequestID] = true
		responseOwners[i] = h.RequestID
		if i == len(arrays["responses"])-1 {
			wanted[h.RequestID] = true
		}
	}
	for i, raw := range arrays["requests"] {
		var h struct {
			ID string `json:"id"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return p, e
		}
		if !responseIDs[h.ID] || i == len(arrays["requests"])-1 {
			wanted[h.ID] = true
		}
	}
	for i := page.Start; i < page.End; i++ {
		var m Message
		raw := arrays["messages"][i]
		if records != nil && records.message != nil {
			raw, err = records.message(i)
			if err != nil {
				return p, err
			}
		}
		if e := json.Unmarshal(raw, &m); e != nil {
			return p, e
		}
		m.ID = DisplayMessageID(s.Run.ID, m, i)
		m.RequestID = headers[i].RequestID
		if records != nil && records.indexed {
			if p.MessagePreviews == nil {
				p.MessagePreviews = map[string]DisplayMessagePreview{}
			}
			p.MessagePreviews[m.ID] = headers[i].Preview
		}
		s.Messages = append(s.Messages, m)
		if m.RequestID != "" {
			wanted[m.RequestID] = true
		}
	}
	if len(s.Messages) > 0 {
		p.MessagesPage.Oldest = s.Messages[0].ID
		p.MessagesPage.Newest = s.Messages[len(s.Messages)-1].ID
	}
	for index, raw := range arrays["actions"] {
		var h struct {
			RequestID string `json:"request_id"`
			Status    string `json:"status"`
			Result    *struct {
				Artifacts []struct {
					Kind string `json:"kind"`
				} `json:"artifacts"`
			} `json:"result"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return p, e
		}
		if h.Result != nil {
			for _, ref := range h.Result.Artifacts {
				if ref.Kind == "file" {
					p.Totals["artifacts"]++
				} else {
					p.Totals["archives"]++
				}
			}
		}
		if wanted[h.RequestID] || h.Result == nil || h.Status == "uncertain" {
			if records != nil && records.action != nil {
				raw, err = records.action(index)
				if err != nil {
					return p, err
				}
			}
			var a Action
			if e := json.Unmarshal(raw, &a); e != nil {
				return p, e
			}
			s.Actions = append(s.Actions, a)
			wanted[h.RequestID] = true
		}
	}
	// The identity pass already selected the latest and every in-flight request.
	// Keep validating all request metadata, even outside the visible window.
	// Duplicated request history and tools stay raw.
	selectedMetadataBytes := 0
	for _, raw := range arrays["requests"] {
		var r struct {
			ModelRequest
			Messages snapshotJSON `json:"messages"`
			Tools    snapshotJSON `json:"tools"`
		}
		if e := json.Unmarshal(raw, &r); e != nil {
			return p, e
		}
		if wanted[r.ID] {
			r.ModelRequest.Messages = nil
			r.ModelRequest.Tools = nil
			r.ModelRequest.ContextManifest = nil
			metadata, err := json.Marshal(r.ModelRequest)
			if err != nil {
				return p, err
			}
			selectedMetadataBytes += len(metadata)
			if selectedMetadataBytes > activityProjectionMaxBytes {
				return p, fmt.Errorf("selected request metadata bytes=%d: %w", selectedMetadataBytes, errActivityTooLarge)
			}
			s.Requests = append(s.Requests, r.ModelRequest)
		}
	}
	for i, raw := range arrays["responses"] {
		if wanted[responseOwners[i]] {
			if records != nil && (!legacy || records.indexed) {
				var err error
				raw, err = records.response(i)
				if err != nil {
					return p, err
				}
			}
			var r ResponseRecord
			if e := json.Unmarshal(raw, &r); e != nil {
				return p, e
			}
			s.Responses = append(s.Responses, r)
		}
	}
	return p, nil
}

// Planner is a read model of persisted scheduling state, not an evidence
// acceptance result. Keep every task/worker identity for editing and controls;
// report receipts explicitly retain not_checked source status.
func buildPlannerDisplay(fields map[string]json.RawMessage) (ActivityProjection, error) {
	p := ActivityProjection{Totals: map[string]int{}}
	s := &p.Snapshot
	for _, item := range []struct {
		name   string
		target any
	}{
		{"run", &s.Run}, {"plan", &s.Plan}, {"workers", &s.Workers},
		{"mailbox", &s.Mailbox}, {"root_contract", &s.RootContract},
		{"messages", &s.Messages}, {"worker_messages", &s.WorkerMessages}, {"child_reports", &s.ChildReports},
	} {
		if len(fields[item.name]) > 0 {
			if err := json.Unmarshal(fields[item.name], item.target); err != nil {
				return p, err
			}
		}
	}
	for i, message := range s.Messages {
		s.Messages[i] = Message{ID: message.ID, Role: message.Role, Attachments: message.Attachments}
	}
	p.Totals["tasks"] = len(s.Plan.Tasks)
	p.Totals["workers"] = len(s.Workers)
	p.Totals["workers_occupying"] = occupiedWorkerSlots(s)
	p.Totals["messages"] = len(s.Messages)
	s.ChildReports = ChildReportReceipts(*s, 0)
	p.HelpRequests = WorkerHelps(s.WorkerMessages)
	return p, nil
}

// Legacy ownership uses the full *identity* sequence and request history IDs,
// not a sliced window. Ambiguous/content-mismatched responses stay unbound.
// Only legacy ledgers pay for response-body comparisons; modern history does not.
func repairActivityOwners(arrays map[string][]snapshotJSON, headers []activityMessage, indexed bool) error {
	positions := map[string]int{}
	for i, m := range headers {
		if m.ID != "" {
			positions[m.ID] = i
		}
	}
	summary := map[string]bool{}
	for _, raw := range arrays["compactions"] {
		var h struct {
			RequestID string `json:"request_id"`
		}
		if e := json.Unmarshal(raw, &h); e != nil {
			return e
		}
		summary[h.RequestID] = true
	}
	type candidate struct {
		request   ModelRequest
		boundary  int
		response  ResponseRecord
		signature string
	}
	requests := map[string]candidate{}
	for _, raw := range arrays["requests"] {
		var r struct {
			ModelRequest
			Messages []struct {
				ID string `json:"id"`
			} `json:"messages"`
			Tools snapshotJSON `json:"tools"`
		}
		if e := json.Unmarshal(raw, &r); e != nil {
			return e
		}
		if summary[r.ID] || r.Purpose != "" && r.Purpose != "conversation" {
			continue
		}
		boundary := -1
		for _, m := range r.Messages {
			if i, ok := positions[m.ID]; ok && i > boundary {
				boundary = i
			}
		}
		requests[r.ID] = candidate{request: r.ModelRequest, boundary: boundary}
	}
	candidates := []candidate{}
	for _, raw := range arrays["responses"] {
		var r ResponseRecord
		if e := json.Unmarshal(raw, &r); e != nil {
			return e
		}
		if c, ok := requests[r.RequestID]; ok && r.Error == "" {
			c.response = r
			if indexed {
				var header struct {
					ContentHash string `json:"display_content_hash"`
				}
				if err := json.Unmarshal(raw, &header); err != nil {
					return err
				}
				c.signature = header.ContentHash
			}
			candidates = append(candidates, c)
		}
	}
	used := map[string]bool{}
	for i := range headers {
		h := &headers[i]
		if h.Role != "assistant" {
			continue
		}
		if h.RequestID != "" {
			used[h.RequestID] = true
			continue
		}
		var m Message
		if e := json.Unmarshal(arrays["messages"][i], &m); e != nil {
			return e
		}
		found := ""
		for _, c := range candidates {
			r := c.request
			response := c.response
			matches := m.Content == response.Response.Content && m.Thinking == response.Response.Thinking && (reflect.DeepEqual(m.ToolCalls, response.Response.ToolCalls) || len(m.ToolCalls) == 0 && len(response.Response.ToolCalls) == 0)
			if indexed {
				matches = h.ContentHash != "" && h.ContentHash == c.signature
			}
			if used[r.ID] || !matches {
				continue
			}
			if !m.CreatedAt.IsZero() && (!r.CreatedAt.IsZero() && m.CreatedAt.Before(r.CreatedAt) || !response.CreatedAt.IsZero() && m.CreatedAt.Before(response.CreatedAt)) {
				continue
			}
			if c.boundary >= i {
				continue
			}
			if c.boundary >= 0 {
				first := c.boundary + 1
				for first < len(headers) && headers[first].Role != "assistant" {
					first++
				}
				if first != i {
					continue
				}
			}
			if found != "" {
				found = ""
				break
			}
			found = r.ID
		}
		h.RequestID = found
		if found != "" {
			used[found] = true
		}
	}
	return nil
}
