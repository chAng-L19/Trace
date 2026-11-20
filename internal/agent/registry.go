package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"sort"
	"strings"

	"trace/internal/localtool"
	"trace/internal/mcp"
)

// RegisteredTool keeps the model-facing stable name separate from the remote
// MCP name. The latter is never exposed as a replacement for the stable ID.
type RegisteredTool struct {
	Definition   ToolDefinition      `json:"definition"`
	Adapter      string              `json:"adapter"`
	Client       *mcp.Client         `json:"-"`
	RemoteName   string              `json:"remote_name,omitempty"`
	RemoteMethod string              `json:"remote_method,omitempty"`
	ServerName   string              `json:"server_name,omitempty"`
	LocalName    string              `json:"local_name,omitempty"`
	Local        *localtool.Manifest `json:"-"`
}

type ToolRegistry struct {
	tools   map[string]RegisteredTool
	configs map[string]mcp.Config
	// Configured identities only: these tools have no callable registration.
	disabledLocal map[string]bool
	disabledMCP   map[string]bool // Server names only; never dialed by Clone.
}

func NewToolRegistry() *ToolRegistry {
	r := &ToolRegistry{tools: map[string]RegisteredTool{}, configs: map[string]mcp.Config{}}
	r.tools["exec"] = RegisteredTool{Definition: execToolDefinition(), Adapter: "exec"}
	for _, d := range builtinDefinitions() {
		r.tools[d.Function.Name] = RegisteredTool{Definition: d, Adapter: "builtin"}
	}
	return r
}

func (r *ToolRegistry) SetMCPConfig(server string, config mcp.Config) { r.configs[server] = config }

// Clone preserves tool identities while opening independent stateful MCP
// sessions for each child agent.
func (r *ToolRegistry) Clone(ctx context.Context) (*ToolRegistry, []*mcp.Client, error) {
	copy := &ToolRegistry{tools: map[string]RegisteredTool{}, configs: map[string]mcp.Config{}, disabledLocal: maps.Clone(r.disabledLocal), disabledMCP: maps.Clone(r.disabledMCP)}
	var clients []*mcp.Client
	for id, t := range r.tools {
		if t.Client == nil {
			copy.tools[id] = t
		}
	}
	for server, config := range r.configs {
		c, err := mcp.Dial(ctx, config)
		if err != nil {
			return copy, clients, err
		}
		clients = append(clients, c)
		discovered, err := c.Discover(ctx)
		if err != nil {
			return copy, clients, err
		}
		if err = copy.AddMCP(server, c, discovered); err != nil {
			return copy, clients, err
		}
		copy.SetMCPConfig(server, config)
	}
	for _, t := range r.tools {
		if t.Client != nil {
			if _, ok := r.configs[t.ServerName]; !ok {
				return copy, clients, fmt.Errorf("MCP server %s lacks clone config", t.ServerName)
			}
		}
	}
	return copy, clients, nil
}

func (r *ToolRegistry) AddMCP(server string, client *mcp.Client, tools []mcp.Tool) error {
	if client == nil {
		return fmt.Errorf("MCP client is required")
	}
	entries := map[string]RegisteredTool{}
	for _, tool := range tools {
		if strings.TrimSpace(tool.ID) == "" || strings.TrimSpace(tool.Name) == "" {
			return fmt.Errorf("MCP tool has missing stable ID or name")
		}
		if _, exists := r.tools[tool.ID]; exists {
			return fmt.Errorf("duplicate registered tool ID: %s", tool.ID)
		}
		if _, exists := entries[tool.ID]; exists {
			return fmt.Errorf("duplicate registered tool ID: %s", tool.ID)
		}
		var params any
		if err := json.Unmarshal(tool.InputSchema, &params); err != nil {
			return fmt.Errorf("MCP tool %s schema: %w", tool.Name, err)
		}
		d := ToolDefinition{Type: "function"}
		d.Function.Name = tool.ID
		d.Function.Description = fmt.Sprintf("MCP %s/%s: %s", server, tool.Name, tool.Description)
		d.Function.Parameters = params
		entries[tool.ID] = RegisteredTool{Definition: d, Adapter: "mcp", Client: client, RemoteName: tool.Name, RemoteMethod: tool.Method, ServerName: server}
	}
	for id, tool := range entries {
		r.tools[id] = tool
	}
	return nil
}

// IndexLocalManifests checks the exact name used for stable IDs and stored
// secrets, independently of availability or executable configuration.
func IndexLocalManifests(manifests []localtool.Manifest) (map[string]localtool.Manifest, error) {
	byName := make(map[string]localtool.Manifest, len(manifests))
	for _, m := range manifests {
		if strings.TrimSpace(m.Name) == "" {
			return nil, fmt.Errorf("local tool name is required")
		}
		if _, exists := byName[m.Name]; exists {
			return nil, fmt.Errorf("duplicate local tool name: %q", m.Name)
		}
		byName[m.Name] = m
	}
	return byName, nil
}

// AddLocal registers generic argv/stdio command adapters. The manifest is
// copied before registration so callers cannot mutate an active registry.
func (r *ToolRegistry) AddLocal(manifests []localtool.Manifest) error {
	if _, err := IndexLocalManifests(manifests); err != nil {
		return err
	}
	entries := map[string]RegisteredTool{}
	disabled := map[string]bool{}
	for _, manifest := range manifests {
		id := manifest.StableID()
		if _, exists := r.tools[id]; exists || r.disabledLocal[id] {
			return fmt.Errorf("duplicate registered tool ID: %s", id)
		}
		if manifest.Disabled {
			disabled[id] = true
			continue
		}
		if err := manifest.Validate(); err != nil {
			return err
		}
		definition := manifest.Definition()
		d := ToolDefinition{Type: "function"}
		d.Function.Name = definition.ID
		d.Function.Description = definition.Description
		if d.Function.Description == "" {
			d.Function.Description = "Execute local tool " + definition.Name
		}
		var params any
		if err := json.Unmarshal(definition.InputSchema, &params); err != nil {
			return fmt.Errorf("local tool %s schema: %w", manifest.Name, err)
		}
		d.Function.Parameters = params
		copyManifest := manifest
		copyManifest.Args = append([]string(nil), manifest.Args...)
		copyManifest.Env = map[string]string{}
		for key, value := range manifest.Env {
			copyManifest.Env[key] = value
		}
		entries[id] = RegisteredTool{Definition: d, Adapter: "local", LocalName: manifest.Name, Local: &copyManifest}
	}
	if len(disabled) != 0 {
		if r.disabledLocal == nil {
			r.disabledLocal = map[string]bool{}
		}
		maps.Copy(r.disabledLocal, disabled)
	}
	for id, tool := range entries {
		r.tools[id] = tool
	}
	return nil
}

func (r *ToolRegistry) Definitions() []ToolDefinition {
	if r == nil {
		return nil
	}
	result := make([]ToolDefinition, 0, len(r.tools))
	// exec is deliberately first; map order must not alter model requests.
	if tool, ok := r.tools["exec"]; ok {
		result = append(result, tool.Definition)
	}
	var names []string
	for name := range r.tools {
		if name != "exec" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		tool := r.tools[name]
		result = append(result, tool.Definition)
	}
	return result
}

func (r *ToolRegistry) Lookup(name string) (RegisteredTool, bool) {
	if r == nil {
		return RegisteredTool{}, false
	}
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *ToolRegistry) Tools() []RegisteredTool {
	definitions := r.Definitions()
	result := make([]RegisteredTool, 0, len(definitions))
	for _, definition := range definitions {
		tool, _ := r.Lookup(definition.Function.Name)
		result = append(result, tool)
	}
	return result
}

// Call is kept on the registry so callers cannot accidentally invoke a remote
// name without resolving the stable model-facing ID first.
func (r *ToolRegistry) Call(ctx context.Context, id string, args json.RawMessage) (mcp.Result, error) {
	return r.CallWithProgress(ctx, id, args, nil)
}

func (r *ToolRegistry) CallWithProgress(ctx context.Context, id string, args json.RawMessage, onProgress func(mcp.Progress)) (mcp.Result, error) {
	tool, ok := r.Lookup(id)
	if !ok || tool.Client == nil {
		return mcp.Result{}, fmt.Errorf("unknown MCP tool: %s", id)
	}
	if tool.RemoteMethod != "" {
		if onProgress != nil {
			return tool.Client.CallMethodWithProgress(ctx, tool.RemoteMethod, args, onProgress)
		}
		return tool.Client.CallMethod(ctx, tool.RemoteMethod, args)
	}
	if onProgress == nil {
		return tool.Client.Call(ctx, tool.RemoteName, args)
	}
	return tool.Client.CallWithProgress(ctx, tool.RemoteName, args, onProgress)
}
