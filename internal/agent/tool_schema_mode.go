package agent

import (
	"encoding/json"
	"fmt"
	"sort"
)

// Auto preserves the historical full-directory-until-overflow behavior.
// Deferred starts with core tools; narrow initially projects explicit external
// IDs. Neither mode removes tools from the discoverable directory.
type ToolSchemaConfig struct {
	Mode string   `json:"mode,omitempty"`
	IDs  []string `json:"ids,omitempty"`
}

// Child creation inherits the current persisted selection, not the original
// startup IDs. A legacy nil exec boundary must remain nil even before dispatch.
func (a *Agent) executionConfiguration(c RunConfiguration) (*ExecConfig, ToolSchemaConfig, *ToolSelection, error) {
	if c.inheritedRun == nil {
		exec, err := normalizeExecConfig(c.Exec)
		if err != nil {
			return nil, ToolSchemaConfig{}, nil, err
		}
		schema, selection, err := a.normalizeToolSchema(c.ToolSchema)
		return exec, schema, selection, err
	}
	source := c.inheritedRun
	var exec *ExecConfig
	if source.Exec != nil {
		var err error
		exec, err = normalizeExecConfig(source.Exec)
		if err != nil {
			return nil, ToolSchemaConfig{}, nil, err
		}
	}
	schema := source.ToolSchema
	schema.IDs = append([]string(nil), schema.IDs...)
	switch schema.Mode {
	case "", "auto":
	case "deferred", "narrow":
		if source.ToolSelection == nil {
			return nil, schema, nil, fmt.Errorf("saved %s tool schema selection is missing", schema.Mode)
		}
	default:
		return nil, schema, nil, fmt.Errorf("unknown saved tool schema mode: %s", schema.Mode)
	}
	var selection *ToolSelection
	if source.ToolSelection != nil {
		selection = a.registry.selection(append([]string{}, source.ToolSelection.IDs...), source.ToolSelection)
		if _, err := a.selectedDefinitions(selection); err != nil {
			return nil, schema, nil, err
		}
	}
	return exec, schema, selection, nil
}

func (a *Agent) normalizeToolSchema(c ToolSchemaConfig) (ToolSchemaConfig, *ToolSelection, error) {
	if c.Mode == "" {
		c.Mode = "auto"
	}
	switch c.Mode {
	case "auto", "deferred":
		if len(c.IDs) != 0 {
			return c, nil, fmt.Errorf("tool_schema ids require narrow mode")
		}
	case "narrow":
		if len(c.IDs) == 0 {
			return c, nil, fmt.Errorf("narrow tool_schema requires explicit ids")
		}
	default:
		return c, nil, fmt.Errorf("tool_schema mode must be auto, deferred or narrow")
	}
	if c.Mode == "auto" {
		return c, nil, nil
	}
	ids := make([]string, 0, len(c.IDs))
	seen := map[string]bool{}
	for _, id := range c.IDs {
		t, ok := a.registry.Lookup(id)
		if !ok || !externalTool(t) {
			return c, nil, fmt.Errorf("unknown external tool ID: %s", id)
		}
		if !seen[id] {
			ids = append(ids, id)
			seen[id] = true
		}
	}
	sort.Strings(ids)
	c.IDs = append([]string(nil), ids...)
	return c, a.registry.selection(ids, nil), nil
}

// Local request diagnostics, not provider usage, tokenization or cache hits.
type ToolRequestMetrics struct {
	Mode                  string `json:"mode"`
	SchemaCount           int    `json:"schema_count"`
	SchemaBytes           int    `json:"schema_bytes"`
	EstimatedSchemaTokens int    `json:"estimated_schema_tokens"`
	SearchCalls           int    `json:"search_calls"`
	SelectCalls           int    `json:"select_calls"`
}

func measureToolRequest(tools []ToolDefinition, s *Snapshot) *ToolRequestMetrics {
	raw, _ := json.Marshal(tools)
	mode := s.Run.ToolSchema.Mode
	if mode == "" {
		mode = "auto"
	}
	m := &ToolRequestMetrics{Mode: mode, SchemaCount: len(tools), SchemaBytes: len(raw), EstimatedSchemaTokens: EstimateContextTokens(nil, tools)}
	// Count journaled attempts, including failures, not delivery retries.
	for _, action := range s.Actions {
		switch action.Name {
		case "tool_search":
			m.SearchCalls++
		case "tool_select":
			m.SelectCalls++
		}
	}
	return m
}
