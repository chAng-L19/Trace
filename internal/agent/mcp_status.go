package agent

import "trace/internal/mcp"

// MCPStatuses snapshots connections owned by this Agent; it performs no I/O.
func (a *Agent) MCPStatuses() map[string]mcp.Status {
	a.mcpState.mu.Lock()
	defer a.mcpState.mu.Unlock()
	out := make(map[string]mcp.Status, len(a.mcpClients))
	for _, client := range a.mcpClients {
		out[client.Name()] = client.Status()
	}
	return out
}
