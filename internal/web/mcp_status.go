package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"trace/internal/mcp"
)

type mcpServiceStatus struct {
	Name        string      `json:"name"`
	Enabled     bool        `json:"enabled"`
	Connection  mcp.Status  `json:"connection"`
	LastAttempt *mcp.Status `json:"last_attempt,omitempty"`
}

func (s *Server) getMCPStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	connections := s.a.MCPStatuses()
	rows := make([]mcpServiceStatus, 0, len(s.config.MCP))
	for _, config := range s.config.MCP {
		state, ok := connections[config.Name]
		if !ok {
			state.State = "not_connected"
		}
		if config.Disabled {
			state = mcp.Status{State: "disabled"}
		}
		row := mcpServiceStatus{Name: config.Name, Enabled: !config.Disabled, Connection: state}
		if attempt, ok := s.mcpAttempts[config.Name]; ok {
			row.LastAttempt = &attempt
		}
		rows = append(rows, row)
	}
	writeJSON(w, 200, rows)
}

// Explicit discovery of the saved service, on a temporary connection. A probe
// never replaces the Agent, its directory, or any unsaved browser form.
func (s *Server) probeMCP(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !decode(w, r, &in) {
		return
	}
	s.mu.Lock()
	var config *mcp.Config
	for _, item := range s.config.MCP {
		if item.Name == in.Name {
			copy := item
			config = &copy
			break
		}
	}
	if s.closed || s.configuring || config == nil {
		s.mu.Unlock()
		writeError(w, 409, fmt.Errorf("服务不存在、正在停止或配置正在更新"))
		return
	}
	if config.Disabled {
		s.mu.Unlock()
		writeJSON(w, 200, mcp.Status{State: "disabled", Detail: "服务已禁用，未启动检测"})
		return
	}
	owner := s.a.Registry()
	if s.mcpProbeSequence == nil {
		s.mcpProbeSequence = map[string]uint64{}
	}
	s.mcpProbeSequence[in.Name]++
	sequence := s.mcpProbeSequence[in.Name]
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	client, err := mcp.Dial(ctx, *config)
	var result mcp.Status
	if err != nil {
		result = mcp.Failure("connect", err)
		var connection *mcp.ConnectionError
		if errors.As(err, &connection) {
			result = connection.Status
		}
	} else {
		_, err = client.Discover(ctx)
		result = client.Status()
		if err != nil {
			failure := mcp.Failure("discovery", err)
			failure.ProtocolVersion, failure.Capabilities = result.ProtocolVersion, result.Capabilities
			result = failure
		}
		result = result.WithCleanup("cleanup", client.Close())
	}
	s.mu.Lock()
	if s.a.Registry() == owner && !s.closed && s.mcpProbeSequence[in.Name] == sequence {
		if s.mcpAttempts == nil {
			s.mcpAttempts = map[string]mcp.Status{}
		}
		s.mcpAttempts[in.Name] = result
	}
	s.mu.Unlock()
	writeJSON(w, 200, result)
}
