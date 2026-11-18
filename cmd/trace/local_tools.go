package main

import (
	"context"

	"trace/internal/agent"
	"trace/internal/web"
)

func registerConfiguredTools(ctx context.Context, runner *agent.Agent, c web.Config) error {
	if err := runner.AddLocalTools(c.LocalTools); err != nil {
		return err
	}
	return registerConfiguredMCP(ctx, runner, c.MCP)
}
