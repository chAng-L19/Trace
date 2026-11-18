package main

import (
	"context"
	"fmt"

	"trace/internal/agent"
	"trace/internal/mcp"
)

func registerConfiguredMCP(ctx context.Context, runner *agent.Agent, configs []mcp.Config) error {
	seen := map[string]bool{}
	for _, config := range configs {
		if seen[config.Name] {
			return fmt.Errorf("duplicate MCP server name: %s", config.Name)
		}
		seen[config.Name] = true
	}
	for _, config := range configs {
		if _, err := runner.AddMCP(ctx, config); err != nil {
			return fmt.Errorf("MCP server %s: %w", config.Name, err)
		}
	}
	return nil
}
