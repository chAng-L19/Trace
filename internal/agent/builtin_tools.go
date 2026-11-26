package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

func builtinDefinitions() []ToolDefinition {
	var out []ToolDefinition
	add := func(name, description string, properties map[string]any, required ...string) {
		if required == nil {
			required = []string{}
		}
		d := ToolDefinition{Type: "function"}
		d.Function.Name = name
		d.Function.Description = description
		d.Function.Parameters = map[string]any{"type": "object", "properties": properties, "required": required}
		out = append(out, d)
	}
	str := func() any { return map[string]string{"type": "string"} }
	integer := func() any { return map[string]string{"type": "integer"} }
	array := func() any { return map[string]any{"type": "array", "items": map[string]string{"type": "string"}} }
	requirements := map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{
		"id": str(), "description": str(), "category": map[string]any{"type": "string", "enum": []string{"goal", "cleanup"}},
		"tool_id": str(), "arguments_hash": str(), "require_review": map[string]string{"type": "boolean"},
		"predicate": map[string]any{"type": "object", "properties": map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"contains", "equals", "regex", "json_equals"}}, "value": str(), "pointer": str(), "expected": map[string]any{}}, "required": []string{"kind"}},
	}, "required": []string{"id", "description", "category", "predicate"}}}
	add("tool_search", "Search the complete external tool directory by names/descriptions, with pagination. Empty query lists all tools; returns stable IDs and provenance. Use tool_select to activate full schemas for the next model request.", map[string]any{"query": str(), "offset": integer(), "limit": integer()})
	add("tool_select", "Activate external tool schemas by stable IDs for following model requests. Selection persists across resume. By default adds IDs; replace=true replaces the selection (empty IDs clears it).", map[string]any{"ids": array(), "replace": map[string]string{"type": "boolean"}}, "ids")
	add("read", "Read file byte range or start_line/line_count; return SHA256, byte-faithful base64, MIME and image dimensions. stdout is JSON with escaped file text in content; full text verification uses json_equals at /content. Final answers.value must be a literal stdout substring, such as the returned hash or unchanged label, not decoded file JSON.", map[string]any{"path": str(), "offset": integer(), "limit": integer(), "start_line": integer(), "line_count": integer()}, "path")
	add("write", "Atomically write UTF-8 file; optional expected_hash detects concurrent edits.", map[string]any{"path": str(), "content": str(), "expected_hash": str()}, "path", "content")
	add("edit", "Atomically apply one exact old_text/new_text or an edits array, returning changed-line diff; expected_hash detects concurrent edits.", map[string]any{"path": str(), "old_text": str(), "new_text": str(), "expected_hash": str(), "edits": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"old_text": str(), "new_text": str()}, "required": []string{"old_text", "new_text"}}}}, "path")
	add("artifact_read", "Read a byte-faithful CAS artifact page (default 4 KiB, max 64 KiB); advance using next_offset. Raw search receipts are audit-only; use normalized source_search items/status. Large pages may themselves be archived.", map[string]any{"hash": str(), "offset": integer(), "limit": integer()}, "hash")
	add("artifact_register", "Register an existing regular file as a deliverable, copying its exact bytes into CAS. Optional name and purpose describe the file; returns its hash, byte count and detected MIME type. This does not verify its conclusions.", map[string]any{"path": str(), "name": str(), "purpose": str()}, "path")
	add("search_sources", "Read enabled native search data sources and shared concurrency/rate bounds. Credentials and endpoints are never returned. Real MCP search tools remain in the MCP directory.", map[string]any{})
	add("source_search", "Retrieve exactly one FOFA/Quake/Shodan search page only when current task facts require it. page starts at 1; page_size defaults to 100 and Shodan requires 100. Results are unverified discovery candidates; raw response is archived in CAS. Observe partial/next_page/auth/quota/rate/outcome_unknown; no automatic retries. Never use search hits as proof of target state, ownership, version or a vulnerability.", map[string]any{"source": map[string]any{"type": "string", "enum": []string{"fofa", "quake", "shodan"}}, "query": str(), "page": integer(), "page_size": integer()}, "source", "query")
	add("plan", "Inspect or update task DAG. mode=append adds new tasks; mode=patch replaces only supplied task definitions while preserving all others; default replace requires the full plan. All modes preserve started contracts. completion_criteria is natural-language intent, never a machine predicate. Split only independently verifiable work; keep tightly coupled edits together. Declare expected_artifacts, child-local requirements and explicit shared write resource_keys; identical keys serialize. For the first nonempty root plan, explicitly set capacity to the observed shared resource limit (or configure it first). Capacity is a root-tree admission ceiling, not a target; default five never requires filling five slots. Plan tasks beyond capacity stay pending without starting a model. Publish capacity and tasks together. required_tool_ids/required_asset_ids block before claim when absent; optional asset_ids do not. Use context_evidence_ids for at most eight explicit parent execution observations documenting the known interface or input artifact; references keep original run/hash and are not child proof. Select sources without credentials; never copy the full history. Use handoff_task_ids only for at most four explicit task-result checkpoints the new task must inherit; task dependencies still define execution order. Use priority for expected value, estimated_seconds for the useful stage execution window (0=unknown), and expected_progress for an observable intermediate artifact, never a business-success predicate. Admission reserves checkpoint time across worker ancestors. Started contracts are immutable; disposed IDs never satisfy dependencies.", map[string]any{"mode": map[string]any{"type": "string", "enum": []string{"replace", "append", "patch"}}, "capacity": integer(), "tasks": map[string]any{"type": "array", "items": map[string]any{"type": "object", "properties": map[string]any{"id": str(), "goal": str(), "completion_criteria": str(), "execution_role": map[string]any{"type": "string", "enum": []string{"worker", "recon"}}, "dependencies": array(), "handoff_task_ids": array(), "context_evidence_ids": array(), "asset_ids": array(), "resource_keys": array(), "required_tool_ids": array(), "required_asset_ids": array(), "expected_artifacts": array(), "input_attachment_ids": array(), "success_contains": str(), "requirements": requirements, "priority": integer(), "estimated_seconds": integer(), "expected_progress": str(), "mode": str(), "stage": map[string]string{"type": "boolean"}, "evidence_scope": map[string]any{"type": "object", "properties": map[string]any{"object_id": str(), "version": str()}, "required": []string{"object_id", "version"}}, "model_selection": map[string]any{"type": "object", "properties": map[string]any{"provider": str(), "model": str(), "reasoning_effort": str()}, "required": []string{"provider", "model"}}, "required_image_input": map[string]string{"type": "boolean"}}, "required": []string{"id", "goal"}}}})
	add("task_claim", "Claim or heartbeat a frontier task with a worker lease. Use a stable manual claim identity or the AgentWorker ID returned by dispatch, never a Run ID. A child may use its own WorkerContext.worker_id to execute one local task in its current worker slot; other manual identities reserve a new slot.", map[string]any{"task_id": str(), "worker_id": str(), "lease_seconds": integer()}, "task_id", "worker_id", "lease_seconds")
	add("task_retry", "Reopen a failed child task only after cleanup completed and its ledger proves zero actions. Preserves the old attempt and never replays executed actions; use worker_dispatch afterwards.", map[string]any{"task_id": str()}, "task_id")
	add("task_dispose", "Explicitly close unfinished, settled work without replaying effects or deleting evidence. blocked/abandoned remain failures for legacy Runs. With an explicit immutable root_contract, a fully settled stage may be retired without claiming the root acceptance passed. superseded requires an existing replacement preserving the same goal and acceptance contract; replan pending dependencies explicitly, since the old ID never becomes a successful dependency. Live workers or unknown cleanup reject disposition.", map[string]any{"task_id": str(), "status": map[string]any{"type": "string", "enum": []string{"blocked", "abandoned", "superseded"}}, "reason": str(), "replacement_task_id": str()}, "task_id", "status", "reason")
	add("task_complete", "Conclude an owned task in this Run's plan; only the owning parent Run may complete its parent task, using its held claim identity. Dispatched child tasks are settled by the worker lifecycle. A child must not claim/complete a task in the parent's plan; finish with a final result after checks pass; yield only unfinished work. verified requires real execution evidence and the business predicate.", map[string]any{"task_id": str(), "worker_id": str(), "status": str(), "evidence_ids": array()}, "task_id", "worker_id", "status")
	add("worker_dispatch", "Dispatch a ready task by task_id, or omit task_id to dispatch the priority frontier within the configured root-tree admission ceiling (shared by nested workers); pending tasks beyond capacity do not start workers or model requests. Returns durable child identities.", map[string]any{"task_id": str()})
	add("worker_status", "Read current child runs, messages and task frontier.", map[string]any{})
	add("worker_history", "Read an explicit bounded page of this Run worker messages, workers or tasks. open_help=true pages unresolved help in current snapshot order. parent=true reads the registered parent shared ledger; omitted current-view records are not resolved or deleted.", map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"messages", "workers", "tasks"}}, "offset": integer(), "limit": integer(), "open_help": map[string]string{"type": "boolean"}, "parent": map[string]string{"type": "boolean"}}, "kind")
	add("worker_yield", "Pause UNFINISHED child work with a concrete next action. Do not use after successful acceptance; return a final result instead. Prefer checkpoint as JSON WorkerHandoff: subject, instance, progress, findings[], negative_results[], next_action, evidence_ids[]; non-JSON text is retained as next_action for compatibility. Evidence IDs must already verify in this child Run. Checkpoint is handoff context, never new evidence or task completion. Releases the slot after cleanup; parent may resume the same worker/run later.", map[string]any{"reason": str(), "checkpoint": str()}, "reason", "checkpoint")
	add("worker_resume", "Continue an existing failed or paused child with the same worker/run identity and prior history, after cleanup. Never creates a new attempt or replays completed actions; unresolved tool effects still require explicit resolution. Cancelled/completed children stay terminal.", map[string]any{"worker_id": str()}, "worker_id")
	add("worker_wait", "Wait up to 300 seconds by default; return early on any worker outcome or new durable input. Other workers keep running. Use this instead of shell sleep or polling loops to await children.", map[string]any{"seconds": integer()})
	add("worker_message", "Send a durable message to a child, parent, or sibling, delivered between tool batches. Optional kind: help (attempts, blocker and needed decision), report, reply, resolved. reply/resolved require reply_to naming the original help ID between the same participants. Delivery, reply and resolution are separate; resolved does not complete a task or resume a paused worker. Reuse operation_id for retries of the same logical send; a changed payload requires a new ID.", map[string]any{"operation_id": str(), "to": str(), "content": str(), "kind": map[string]any{"type": "string", "enum": []string{"help", "report", "reply", "resolved"}}, "reply_to": str()}, "to", "content")
	add("worker_cancel", "Cancel the observed child visit. Pass expected_visit from worker_status; a stale visit is rejected. Cleanup and unknown outcomes remain recorded.", map[string]any{"worker_id": str(), "expected_visit": map[string]any{"type": "integer", "minimum": 0}}, "worker_id", "expected_visit")
	add("worker_recover", "Reconcile durable cleanup receipts without replaying work; expired visits without cleanup proof remain uncertain and occupy capacity.", map[string]any{})
	add("asset_upsert", "Add/update an included/excluded asset and parent hierarchy; coverage is discovered/in_progress/observed/verified/negative/failed/uncertain. Verified and negative need execution evidence and a nonempty predicate copied as a literal substring of at least one cited verified stdout; predicate is not a natural-language explanation. source_run_id explicitly selects a registered child ledger.", map[string]any{"id": str(), "kind": str(), "value": str(), "coverage": str(), "predicate": str(), "parent_id": str(), "scope": str(), "source_task_id": str(), "source_run_id": str(), "evidence_ids": array()}, "id", "kind", "value")
	add("asset_relate", "Add a relation between existing assets with optional execution evidence.", map[string]any{"from": str(), "to": str(), "kind": str(), "evidence_ids": array()}, "from", "to", "kind")
	add("assets", "Inspect asset coverage and relation graph.", map[string]any{})
	add("skills_list", "List installed runtime skill metadata; bodies are loaded only on demand.", map[string]any{})
	add("read_skill", "Read a named UTF-8 runtime skill (maximum 65536 bytes), returning its body, source, SHA256 and immutable artifact. Optionally pin an expected_sha256 from a previous read.", map[string]any{"name": str(), "expected_sha256": str()}, "name")
	add("session_read", "Read a full archived conversation message by its stable message ID.", map[string]any{"message_id": str()}, "message_id")
	out = append(out, verificationDefinitions()...)
	out = append(out, evidenceChainDefinitions()...)
	return out
}

func decodeBuiltin(raw string, value any) error {
	if err := validateMCPArguments(raw); err != nil {
		return err
	}
	return decodeToolJSON([]byte(raw), value)
}

func (a *Agent) builtin(ctx context.Context, id string, call ToolCall) (any, error) {
	if value, handled, err := a.verificationBuiltin(ctx, id, call); handled {
		return value, err
	}
	switch call.Name {
	case "search_sources":
		return a.SearchSourceStatus(), nil
	case "source_search":
		return a.sourceSearch(ctx, id, call)
	case "evidence_chain_create", "evidence_chain_read", "evidence_chain_reference", "evidence_chain_search":
		return a.evidenceChainBuiltin(id, call)
	case "artifact_register":
		var in ArtifactRegistration
		if err := decodeBuiltin(call.Arguments, &in); err != nil {
			return nil, err
		}
		return a.store.RegisterArtifact(in)
	case "tool_search", "tool_select":
		return a.toolDirectory(call.Name, id, call.Arguments)
	case "session_read":
		var r struct {
			MessageID string `json:"message_id"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		return a.ReadMessage(id, r.MessageID)
	case "read", "write", "edit":
		var r FileRequest
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		var fields map[string]json.RawMessage
		json.Unmarshal([]byte(call.Arguments), &fields)
		required := []string{"path"}
		if call.Name == "write" {
			required = append(required, "content")
		}
		if call.Name == "edit" && len(r.Edits) == 0 {
			required = append(required, "old_text", "new_text")
		}
		for _, key := range required {
			if raw, ok := fields[key]; !ok || string(raw) == "null" {
				return nil, fmt.Errorf("%s is required", key)
			}
		}
		if call.Name == "edit" && len(r.Edits) > 0 {
			var edits []map[string]json.RawMessage
			json.Unmarshal(fields["edits"], &edits)
			for _, edit := range edits {
				for _, key := range []string{"old_text", "new_text"} {
					if raw, ok := edit[key]; !ok || string(raw) == "null" {
						return nil, fmt.Errorf("edit %s is required", key)
					}
				}
			}
		}
		return fileOperation(call.Name, r, a.store.checkModelArtifactRead)
	case "artifact_read":
		var r struct {
			Hash          string
			Offset, Limit int64
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		if r.Limit == 0 {
			r.Limit = 4096
		}
		if err := a.store.checkModelArtifactRead(r.Hash); err != nil {
			return nil, err
		}
		return a.store.ReadArtifact(r.Hash, r.Offset, r.Limit)
	case "plan":
		var r struct {
			Mode     string
			Tasks    []PlanTask
			Capacity *int
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		capacity := 0
		if r.Capacity != nil {
			capacity = *r.Capacity
			if capacity < 1 || capacity > 32 {
				return nil, fmt.Errorf("capacity must be 1..32")
			}
		}
		var s Snapshot
		var err error
		if r.Tasks == nil {
			if r.Capacity != nil {
				s, err = a.SetPlanCapacity(id, capacity)
			} else {
				s, err = a.store.Load(id)
			}
		} else {
			s, err = a.updatePlan(id, r.Tasks, r.Mode, capacity, true)
		}
		if r.Tasks == nil {
			return s.Plan, err
		}
		return compactPlan(s.Plan), err
	case "task_dispose":
		var r struct {
			TaskID            string `json:"task_id"`
			Status            string `json:"status"`
			Reason            string `json:"reason"`
			ReplacementTaskID string `json:"replacement_task_id"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		s, err := a.DisposeTask(id, r.TaskID, r.Status, r.Reason, r.ReplacementTaskID)
		return compactPlan(s.Plan), err
	case "task_retry":
		var r struct {
			TaskID string `json:"task_id"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		s, err := a.RetryTask(id, r.TaskID)
		return compactPlan(s.Plan), err
	case "task_claim", "task_complete":
		var r struct {
			TaskID       string `json:"task_id"`
			WorkerID     string `json:"worker_id"`
			LeaseSeconds int    `json:"lease_seconds"`
			Status       string
			EvidenceIDs  []string `json:"evidence_ids"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		current, err := a.store.Load(id)
		if err != nil {
			return nil, err
		}
		_, localTaskErr := planTask(&current, r.TaskID)
		if localTaskErr != nil && current.Run.ParentRunID != "" && current.Run.ParentTaskID != "" && r.TaskID == current.Run.ParentTaskID {
			return nil, fmt.Errorf("this child cannot claim or complete parent task %s; report to the parent and yield, then let the parent settle its own plan", r.TaskID)
		}
		if r.WorkerID != "" {
			isRunID := r.WorkerID == current.Run.ID || r.WorkerID == current.Run.ParentRunID
			for _, worker := range current.Workers {
				isRunID = isRunID || r.WorkerID == worker.RunID
			}
			if isRunID {
				return nil, fmt.Errorf("worker_id must be a stable claim/AgentWorker identity, not a Run ID")
			}
		}
		var s Snapshot
		if call.Name == "task_claim" {
			s, err = a.ClaimTask(id, r.TaskID, r.WorkerID, r.LeaseSeconds)
		} else {
			s, err = a.CompleteTask(id, r.TaskID, r.WorkerID, r.Status, r.EvidenceIDs)
		}
		if err != nil {
			return nil, err
		}
		return planTask(&s, r.TaskID)
	case "worker_dispatch":
		var r struct {
			TaskID string `json:"task_id"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		// Child lifetime follows the persisted parent status, not the tool call
		// context, which ends immediately after dispatch returns.
		if r.TaskID == "" {
			return a.DispatchFrontier(context.Background(), id)
		}
		return a.DispatchTask(context.Background(), id, r.TaskID)
	case "worker_yield":
		var in struct {
			Reason     string
			Checkpoint string
		}
		if err := decodeBuiltin(call.Arguments, &in); err != nil {
			return nil, err
		}
		var handoff *WorkerHandoff
		trimmed := strings.TrimSpace(in.Checkpoint)
		if strings.HasPrefix(trimmed, "{") {
			var parsed WorkerHandoff
			decoder := json.NewDecoder(bytes.NewBufferString(in.Checkpoint))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&parsed); err != nil {
				return nil, err
			}
			if err := decoder.Decode(new(any)); err != io.EOF {
				return nil, fmt.Errorf("checkpoint must contain one JSON object")
			}
			if strings.TrimSpace(parsed.NextAction) == "" {
				return nil, fmt.Errorf("structured checkpoint requires next_action")
			}
			handoff = &parsed
		}
		s, err := a.YieldWorkerWithHandoff(id, in.Reason, in.Checkpoint, handoff)
		return map[string]string{"status": string(s.Run.Status), "reason": s.Run.PauseReason}, err
	case "worker_history":
		var query WorkerHistoryQuery
		if err := decodeBuiltin(call.Arguments, &query); err != nil {
			return nil, err
		}
		return a.workerHistory(id, query)
	case "worker_status":
		if err := a.reconcileWorkerSettlements(id); err != nil {
			return nil, err
		}
		s, err := a.store.Load(id)
		if err != nil {
			return nil, err
		}
		state := a.compactWorkerActivity(s)
		if s.Run.ParentRunID != "" && s.Run.ParentTaskID != "" {
			parent, err := a.store.Load(s.Run.ParentRunID)
			if err != nil {
				return nil, err
			}
			state["parent_run_id"], state["parent"] = parent.Run.ID, a.compactWorkerActivity(parent)
		}
		return state, nil
	case "worker_wait":
		var r struct{ Seconds int }
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		s, err := a.WaitWorkers(ctx, id, r.Seconds)
		return a.compactWorkerActivity(s), err
	case "worker_message":
		var r struct {
			To, Content, Kind string
			ReplyTo           string `json:"reply_to"`
			OperationID       string `json:"operation_id"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		s, err := a.store.Load(id)
		if err != nil {
			return nil, err
		}
		from := "parent"
		root := id
		childTarget := false
		for _, w := range s.Workers {
			childTarget = childTarget || w.ID == r.To
		}
		if !childTarget && s.Run.ParentRunID != "" && s.Run.ParentTaskID != "" {
			root = s.Run.ParentRunID
			p, e := a.store.Load(root)
			if e != nil {
				return nil, e
			}
			from = ""
			for _, w := range p.Workers {
				if w.RunID == id {
					from = w.ID
				}
			}
		}
		if from == "" {
			return nil, fmt.Errorf("child has no registered worker identity")
		}
		_, receipt, created, err := a.SendWorkerMessageOperation(root, from, r.To, r.Content, r.Kind, r.ReplyTo, r.OperationID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"receipt": receipt, "created": created, "status": "accepted", "processed": false}, nil
	case "worker_resume":
		var r struct {
			WorkerID string `json:"worker_id"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		// Like dispatch, continuation outlives the current parent tool call.
		return a.ResumeWorker(context.Background(), id, r.WorkerID)
	case "worker_cancel":
		var r struct {
			WorkerID      string  `json:"worker_id"`
			ExpectedVisit *uint64 `json:"expected_visit"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		if r.ExpectedVisit == nil {
			return nil, fmt.Errorf("expected_visit is required; use the visit observed in worker_status")
		}
		s, err := a.CancelWorkerVisit(id, r.WorkerID, r.ExpectedVisit)
		return s.Workers, err
	case "worker_recover":
		s, err := a.RecoverWorkers(id)
		return s.Workers, err
	case "asset_upsert":
		var r Asset
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		s, err := a.UpsertAsset(id, r)
		return s.Assets, err
	case "asset_relate":
		var r AssetRelation
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		s, err := a.RelateAssets(id, r)
		return s.Relations, err
	case "assets":
		s, err := a.store.Load(id)
		return map[string]any{"assets": s.Assets, "relations": s.Relations, "coverage": Coverage(s)}, err
	case "skills_list":
		return a.ModelSkills(), nil
	case "read_skill":
		var r struct {
			Name           string `json:"name"`
			ExpectedSHA256 string `json:"expected_sha256,omitempty"`
		}
		if err := decodeBuiltin(call.Arguments, &r); err != nil {
			return nil, err
		}
		return a.readModelSkillDocument(r.Name, r.ExpectedSHA256)
	}
	return nil, fmt.Errorf("unknown builtin %s", call.Name)
}

func (a *Agent) executeBuiltin(ctx context.Context, id string, call ToolCall) (Snapshot, error) {
	return a.executeAction(ctx, id, call.ID, "builtin", nil, func() ToolResult {
		value, err := a.builtin(ctx, id, call)
		if err != nil {
			result := ToolResult{ExitCode: 1, Error: err.Error()}
			var blocked *TaskDispositionError
			if errors.As(err, &blocked) {
				result.Diagnostic = &ToolDiagnostic{Code: "task_disposition_blocked", Summary: blocked.Reason, NextAction: blocked.NextAction, Blockers: blocked}
			}
			return result
		}
		data, err := json.Marshal(value)
		if err != nil {
			return ToolResult{ExitCode: 1, Error: err.Error()}
		}
		result := ToolResult{Stdout: string(data)}
		if search, ok := value.(SourceSearchResult); ok {
			if search.Raw != nil {
				result.Artifacts = []ArtifactRef{*search.Raw}
			}
			result.OutcomeUncertain = search.Status == "outcome_unknown"
			if search.Status != "completed" && search.Status != "partial" {
				result.ExitCode = 1
			}
		}
		if ref, ok := value.(ArtifactRef); call.Name == "artifact_register" && ok {
			result.Artifacts = []ArtifactRef{ref}
		}
		if skill, ok := value.(SkillRead); call.Name == "read_skill" && ok {
			result.Artifacts = []ArtifactRef{skill.Artifact}
		}
		return result
	})
}
