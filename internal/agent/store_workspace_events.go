package agent

import "encoding/json"

// WorkspaceEvents preserves request/call ownership before trimming metadata.
// The same projection is used for lightweight and provenance-checked reads.
func WorkspaceEvents(snap Snapshot, settled, archived map[string]bool) []Event {
	callKey := func(request, call string) string { return request + "\x00" + call }
	pending := map[string]bool{}
	unique := map[string]string{}
	for _, a := range snap.Actions {
		if _, exists := unique[a.CallID]; exists {
			unique[a.CallID] = ""
		} else {
			unique[a.CallID] = a.RequestID
		}
		if a.Result == nil {
			pending[callKey(a.RequestID, a.CallID)] = true
		}
	}
	var metadata, live []Event
	current := ""
	owners := map[string]string{}
	for _, e := range snap.Events {
		if e.Type == "task_claimed" {
			continue
		}
		raw, ok := e.Data.(json.RawMessage)
		if !ok {
			raw, _ = json.Marshal(e.Data)
		}
		var identity struct {
			Request string `json:"request_id"`
			Call    string `json:"call_id"`
		}
		_ = json.Unmarshal(raw, &identity)
		request, call := identity.Request, identity.Call
		if e.Type == "model_request" {
			current = request
		}
		if e.Type == "action_started" {
			if request == "" {
				request = current
			}
			owners[call] = request
		}
		if request == "" && call != "" {
			request = owners[call]
			if request == "" {
				request = unique[call]
			}
		}
		if request == "" && e.Type == "model_error" {
			request = current
		}
		if e.Type == "model_stream" && (settled[request] || archived[request]) {
			continue
		}
		if (e.Type == "tool_output" || e.Type == "tool_progress" || e.Type == "mcp_progress") && (request == "" || !pending[callKey(request, call)]) {
			continue
		}
		// Annotate the projection before trimming metadata so repeated call IDs keep
		// their original request owner even when action_started leaves the tail.
		if request != "" && request != identity.Request {
			var data map[string]json.RawMessage
			if json.Unmarshal(raw, &data) == nil && len(data) > 0 {
				data["request_id"], _ = json.Marshal(request)
				e.Data = data
			}
		}
		switch e.Type {
		case "task_claimed":
			continue
		case "model_stream":
			if !settled[request] && !archived[request] {
				live = append(live, e)
			}
		case "tool_output", "tool_progress", "mcp_progress":
			if request != "" && pending[callKey(request, call)] {
				live = append(live, e)
			}
		default:
			metadata = append(metadata, e)
		}
	}
	if len(metadata) > 200 {
		metadata = metadata[len(metadata)-200:]
	}
	// Live display is a bounded tail, not a second ledger. The original events
	// remain available through event history/export and provenance uses Load.
	bytes, start := 0, len(live)
	for start > 0 && len(live)-start < 500 {
		raw, _ := json.Marshal(live[start-1])
		if bytes+len(raw) > 256*1024 {
			break
		}
		bytes += len(raw)
		start--
	}
	live = live[start:]
	events := make([]Event, 0, len(metadata)+len(live))
	i, j := 0, 0
	for i < len(metadata) || j < len(live) {
		if j == len(live) || (i < len(metadata) && metadata[i].Sequence < live[j].Sequence) {
			events = append(events, metadata[i])
			i++
		} else {
			events = append(events, live[j])
			j++
		}
	}
	return events
}
