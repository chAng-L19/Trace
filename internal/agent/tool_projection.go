package agent

import (
	"fmt"
	"sort"
	"strings"
)

// A nil selection preserves the complete directory for small registries. A
// non-nil selection projects only its currently registered external schemas.
type ToolSelection struct {
	IDs        []string          `json:"ids"`
	MCPServers map[string]string `json:"mcp_servers,omitempty"` // Selected registration ID -> owning server.
}

// Capture ownership from live registrations, or carry it forward for a retained
// selection. IDs are opaque (including MCP method bridge IDs).
func (r *ToolRegistry) selection(ids []string, previous *ToolSelection) *ToolSelection {
	next := &ToolSelection{IDs: ids}
	for _, id := range ids {
		var server string
		if tool, ok := r.Lookup(id); ok {
			if tool.Adapter == "mcp" {
				server = tool.ServerName
			}
		} else if previous != nil {
			server = previous.MCPServers[id]
		}
		if server != "" {
			if next.MCPServers == nil {
				next.MCPServers = map[string]string{}
			}
			next.MCPServers[id] = server
		}
	}
	return next
}

func (r *ToolRegistry) disabledSelection(s *ToolSelection, id string) bool {
	server, known := s.MCPServers[id]
	return r.disabledLocal[id] || (known && r.disabledMCP[server])
}

type ToolMatch struct {
	ID           string `json:"id"`
	Description  string `json:"description"`
	Adapter      string `json:"adapter"`
	Server       string `json:"server,omitempty"`
	RemoteName   string `json:"remote_name,omitempty"`
	RemoteMethod string `json:"remote_method,omitempty"`
	LocalName    string `json:"local_name,omitempty"`
}
type ToolSearchResult struct {
	Tools      []ToolMatch `json:"tools"`
	Total      int         `json:"total"`
	NextOffset int         `json:"next_offset"`
}

func externalTool(t RegisteredTool) bool { return t.Adapter == "mcp" || t.Adapter == "local" }
func toolMatch(t RegisteredTool) ToolMatch {
	return ToolMatch{ID: t.Definition.Function.Name, Description: t.Definition.Function.Description, Adapter: t.Adapter, Server: t.ServerName, RemoteName: t.RemoteName, RemoteMethod: t.RemoteMethod, LocalName: t.LocalName}
}

func (a *Agent) SearchTools(query string, offset, limit int) (ToolSearchResult, error) {
	if offset < 0 || limit < 0 || limit > 50 {
		return ToolSearchResult{}, fmt.Errorf("offset must be non-negative and limit must be 0..50")
	}
	if limit == 0 {
		limit = 10
	}
	var matches []ToolMatch
	terms := strings.Fields(strings.ToLower(query))
	for _, t := range a.registry.Tools() {
		if !externalTool(t) {
			continue
		}
		haystack := strings.ToLower(t.Definition.Function.Name + " " + t.Definition.Function.Description + " " + t.RemoteName + " " + t.LocalName + " " + t.ServerName)
		matched := true
		for _, term := range terms {
			if !strings.Contains(haystack, term) {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, toolMatch(t))
		}
	}
	start := min(offset, len(matches))
	end := min(start+limit, len(matches))
	return ToolSearchResult{Tools: matches[start:end], Total: len(matches), NextOffset: end}, nil
}

func (a *Agent) selectedDefinitions(selection *ToolSelection) ([]ToolDefinition, error) {
	if selection == nil {
		return a.registry.Definitions(), nil
	}
	selected := map[string]bool{}
	for _, id := range selection.IDs {
		if _, ok := a.registry.Lookup(id); !ok && !a.registry.disabledSelection(selection, id) {
			return nil, fmt.Errorf("selected tool is no longer registered: %s", id)
		}
		selected[id] = true
	}
	var result []ToolDefinition
	for _, t := range a.registry.Tools() {
		if !externalTool(t) || selected[t.Definition.Function.Name] {
			result = append(result, t.Definition)
		}
	}
	return result, nil
}

const deferredToolHint = "\n\nExternal tool schemas are loaded on demand. The complete tool directory remains available via tool_search (empty query lists every tool, with pagination). Search names/descriptions, then call tool_select with stable IDs; full schemas appear in your next request. Selection persists across resume and can be replaced. Do not guess tool IDs.\n"

func (a *Agent) modelTools(s *Snapshot, catalog string, runtime ...string) ([]ToolDefinition, string, error) {
	switch s.Run.ToolSchema.Mode {
	case "", "auto":
	case "deferred", "narrow":
		if s.Run.ToolSelection == nil {
			return nil, "", fmt.Errorf("saved %s tool schema selection is missing", s.Run.ToolSchema.Mode)
		}
	default:
		return nil, "", fmt.Errorf("unknown saved tool schema mode: %s", s.Run.ToolSchema.Mode)
	}
	c, err := normalizeContext(s.Run.Context)
	if err != nil {
		return nil, "", err
	}
	if s.Run.ToolSelection == nil {
		all := a.runToolDefinitions(s.Run, a.registry.Definitions())
		fixed := appendContextHistory(s, contextPrefix(s, contextSystem(s, catalog)), []Message{{Role: "user", Content: s.Run.Goal}}, runtime)
		if EstimateContextTokens(fixed, all) <= contextInputLimit(c) {
			return all, "", nil
		}
		core, _ := a.selectedDefinitions(&ToolSelection{})
		core = a.runToolDefinitions(s.Run, core)
		if len(core) == len(all) {
			return all, "", nil
		}
		// A schema-only overflow defers the entire external directory. No guessed
		// relevance filter can hide an otherwise available capability.
		s.Run.ToolSelection = &ToolSelection{IDs: []string{}}
		addEvent(s, "tool_directory_deferred", "外部工具 schema 按需加载，完整目录保留", map[string]int{"registered": len(all)})
	}
	definitions, err := a.selectedDefinitions(s.Run.ToolSelection)
	return a.runToolDefinitions(s.Run, definitions), deferredToolHint, err
}

func (a *Agent) SelectTools(id string, ids []string, replace bool) (Snapshot, error) {
	snapshot, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	workerContext := a.workerContextProjection(&snapshot)
	inherited, err := a.inheritedContextLedger(&snapshot)
	if err != nil {
		return Snapshot{}, err
	}
	return a.store.Update(id, func(s *Snapshot) error {
		selected := map[string]bool{}
		if !replace && s.Run.ToolSelection != nil {
			for _, id := range s.Run.ToolSelection.IDs {
				selected[id] = true
			}
		}
		for _, id := range ids {
			// A disabled identity may survive this run's save, never enter a new selection.
			if s.Run.ToolSelection != nil && a.registry.disabledSelection(s.Run.ToolSelection, id) && hasString(s.Run.ToolSelection.IDs, id) {
				selected[id] = true
				continue
			}
			t, ok := a.registry.Lookup(id)
			if !ok || !externalTool(t) {
				return fmt.Errorf("unknown external tool ID: %s", id)
			}
			selected[id] = true
		}
		nextIDs := make([]string, 0, len(selected))
		for id := range selected {
			nextIDs = append(nextIDs, id)
		}
		sort.Strings(nextIDs)
		next := a.registry.selection(nextIDs, s.Run.ToolSelection)
		definitions, err := a.selectedDefinitions(next)
		if err != nil {
			return err
		}
		c, err := normalizeContext(s.Run.Context)
		if err != nil {
			return err
		}
		s.workerContextMessage = workerContext
		planner, _ := plannerContextFrom(s)
		runtime := planner + verificationContext(s) + contextLedgerContext(s, inherited) + a.stageModelContext() + a.reconContext(s)
		fixed := appendContextHistory(s, contextPrefix(s, contextSystem(s, a.skillCatalog()+deferredToolHint)), []Message{{Role: "user", Content: s.Run.Goal}}, []string{runtime})
		if EstimateContextTokens(fixed, a.runToolDefinitions(s.Run, definitions)) > contextInputLimit(c) {
			return fmt.Errorf("selected schemas exceed context budget; select fewer tools")
		}
		s.Run.ToolSelection = next
		addEvent(s, "tools_selected", "工具 schema 选择已持久化", next)
		return nil
	})
}

func (a *Agent) toolDirectory(name, id, raw string) (any, error) {
	if name == "tool_search" {
		var r struct {
			Query         string
			Offset, Limit int
		}
		if err := decodeBuiltin(raw, &r); err != nil {
			return nil, err
		}
		return a.SearchTools(r.Query, r.Offset, r.Limit)
	}
	var r struct {
		IDs     []string
		Replace bool
	}
	if err := decodeBuiltin(raw, &r); err != nil {
		return nil, err
	}
	if r.IDs == nil {
		return nil, fmt.Errorf("ids is required (empty array clears selection with replace=true)")
	}
	s, err := a.SelectTools(id, r.IDs, r.Replace)
	if err != nil {
		return nil, err
	}
	return map[string]any{"selected_ids": s.Run.ToolSelection.IDs, "schema_delivery": "full schemas are included in the next model request"}, nil
}
