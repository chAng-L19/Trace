package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"trace/internal/mcp"
)

func validateMCPArguments(arguments string) error {
	var object map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &object) != nil || object == nil {
		return fmt.Errorf("MCP tool arguments must be a JSON object")
	}
	return nil
}

// ExecuteMCP explicitly resolves a pending call using its stable registry ID.
// Validation precedes budget changes or action creation. Arguments are the
// original persisted model arguments, not a second operator-supplied payload.
func (a *Agent) ExecuteMCP(parent context.Context, id, callID, toolID string) (final Snapshot, executeErr error) {
	tool, ok := a.registry.Lookup(toolID)
	if !ok || tool.Client == nil {
		return Snapshot{}, fmt.Errorf("unknown MCP tool: %s", toolID)
	}
	return a.ExecuteRegistered(parent, id, callID, toolID)
}

// ExecuteRegistered resolves a registered local or MCP pending call without
// replacing its persisted model arguments with operator-supplied input.
func (a *Agent) ExecuteRegistered(parent context.Context, id, callID, toolID string) (final Snapshot, executeErr error) {
	tool, ok := a.registry.Lookup(toolID)
	if !ok || (tool.Client == nil && tool.Local == nil && tool.Adapter != "builtin") {
		return Snapshot{}, fmt.Errorf("unknown registered tool: %s", toolID)
	}
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, err
	}
	defer release()
	snapshot, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	call, ok := pendingToolCalls(&snapshot)[callID]
	if snapshot.Run.Status != StatusWaitingTool || !ok || call.Name != toolID {
		return snapshot, fmt.Errorf("pending call does not match tool ID: %s", toolID)
	}
	if tool.Local != nil {
		_, _, err = tool.Local.Invocation(json.RawMessage(call.Arguments))
	} else {
		err = validateMCPArguments(call.Arguments)
	}
	if err != nil {
		return snapshot, err
	}
	operation := func(agent *Agent, ctx context.Context, id string) (Snapshot, error) {
		return agent.executeRegisteredLocked(ctx, id, callID, toolID)
	}
	if snapshot.Run.ParentTaskID != "" && a.workerRunID != id {
		return a.resumeWorkerOperation(parent, snapshot, operation)
	}
	return operation(a, parent, id)
}

func (a *Agent) executeRegisteredLocked(parent context.Context, id, callID, toolID string) (Snapshot, error) {
	tool, ok := a.registry.Lookup(toolID)
	if !ok || (tool.Client == nil && tool.Local == nil && tool.Adapter != "builtin") {
		return Snapshot{}, fmt.Errorf("unknown registered tool: %s", toolID)
	}
	snap, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	call, ok := pendingToolCalls(&snap)[callID]
	if snap.Run.Status != StatusWaitingTool || !ok || call.Name != toolID {
		return Snapshot{}, fmt.Errorf("pending call does not match tool ID: %s", toolID)
	}
	var command []string
	if tool.Local != nil {
		command, _, err = tool.Local.Invocation(json.RawMessage(call.Arguments))
	} else {
		err = validateMCPArguments(call.Arguments)
	}
	if err != nil {
		return Snapshot{}, err
	}
	return a.executeManualToolCycle(parent, id, func(ctx context.Context) (Snapshot, error) {
		if tool.Adapter == "builtin" {
			return a.executeBuiltin(ctx, id, call)
		}
		if tool.Local != nil {
			return a.executeLocal(ctx, id, call, tool.Local, command)
		}
		return a.executeMCP(ctx, id, call)
	})
}

func (a *Agent) executeMCP(ctx context.Context, id string, call ToolCall) (Snapshot, error) {
	return a.executeAction(ctx, id, call.ID, "mcp", nil, func() ToolResult {
		remote, err := a.registry.CallWithProgress(ctx, call.Name, json.RawMessage(call.Arguments), func(progress mcp.Progress) {
			_, _ = a.store.Update(id, func(s *Snapshot) error {
				addEvent(s, "mcp_progress", progress.Message, map[string]any{"call_id": call.ID, "progress": progress.Progress, "total": progress.Total, "message": progress.Message})
				return nil
			})
		})
		result := ToolResult{Stdout: remote.Text(), MCPResult: remote.Raw, MCPIsError: remote.IsError, Interrupted: ctx.Err() != nil}
		if remote.IsError {
			result.ExitCode = 1
		}
		if err != nil {
			result.ExitCode = -1
			result.Error = err.Error()
			var rpcError *mcp.RPCError
			// A server RPC error is an explicit response. A lost transport reply
			// may hide side effects and must not trigger an automatic replay.
			result.OutcomeUncertain = !errors.As(err, &rpcError)
		}
		return result
	})
}
