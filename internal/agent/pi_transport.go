package agent

import (
	"bytes"
	"encoding/json"
	"slices"
	"time"
)

// Rebuildable process-local transport cache, never authoritative history.
// Compaction, late input and tool changes are compared with the actual final
// request, not inferred from message counts or event watermarks.
type piContextTransport struct {
	revision int
	messages []json.RawMessage
	tools    json.RawMessage
	parallel []string
}

func (c *piContextTransport) prepare(request ModelRequest, parallel []string, revision int) (map[string]any, error) {
	messages := make([]json.RawMessage, len(request.Messages))
	for i, message := range request.Messages {
		// Synthetic projection timestamps are not conversation facts and change
		// every prepare. Keep the durable request intact; normalize transport only.
		if message.Role == "system" {
			message.CreatedAt = time.Time{}
		}
		raw, err := json.Marshal(message)
		if err != nil {
			return nil, err
		}
		messages[i] = raw
	}
	tools, err := json.Marshal(request.Tools)
	if err != nil {
		return nil, err
	}
	prefix := 0
	valid := revision == c.revision && c.revision > 0
	var head json.RawMessage
	if valid {
		if len(messages) > 0 && len(c.messages) > 0 && request.Messages[0].Role == "system" && !bytes.Equal(messages[0], c.messages[0]) {
			var previous Message
			if json.Unmarshal(c.messages[0], &previous) == nil && previous.Role == "system" {
				head = messages[0]
				prefix = 1
			}
		}
		for prefix < len(messages) && prefix < len(c.messages) && bytes.Equal(messages[prefix], c.messages[prefix]) {
			prefix++
		}
	}
	base := 0
	if valid {
		base = c.revision
	}
	c.revision++
	result := map[string]any{"request_id": request.ID, "base_revision": base, "revision": c.revision, "message_prefix": prefix, "messages": messages[prefix:], "parallel_tools": parallel}
	if head != nil {
		result["message_head"] = head
	}
	if !valid || !bytes.Equal(tools, c.tools) {
		result["tools"] = request.Tools
	}
	c.messages, c.tools = messages, tools
	c.parallel = slices.Clone(parallel)
	return result, nil
}

func (c *piContextTransport) modelResponse(request ModelRequest, response ModelResponse, parallel []string) (map[string]any, error) {
	tools, err := json.Marshal(request.Tools)
	if err != nil {
		return nil, err
	}
	result := map[string]any{"response": response}
	if c.revision == 0 || !bytes.Equal(tools, c.tools) || !slices.Equal(parallel, c.parallel) {
		result["tools"], result["parallel_tools"] = request.Tools, parallel
	}
	// Internal retries/recovery may reprepare without an RPC prepare. Update
	// only executable tools here; the next prepare still compares with the
	// canonical schema that the host used to convert its retained history.
	return result, nil
}
