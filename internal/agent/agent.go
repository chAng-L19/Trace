package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"trace/internal/localtool"
	"trace/internal/mcp"
	"trace/internal/worker"
)

type Agent struct {
	searchSources             *searchSourceRuntime
	modelProviders            map[string]*OpenAIProvider
	roleModels                RoleModels
	costConfig                CostConfig
	store                     *Store
	provider                  Provider
	prompt                    string
	promptID                  string
	budget                    BudgetOverride
	treeBudget                *TreeBudget
	requireVerifiedCompletion bool
	successContains           string
	registry                  *ToolRegistry
	mcpClients                []*mcp.Client
	contextConfig             ContextConfig
	contextOverride           bool
	skills                    []Skill
	workers                   *workerLifecycle
	mcpState                  *mcpLifecycle
	workerRunID               string // Run whose worker lifecycle this Agent currently owns.
	workerVisit               uint64 // Current durable child visit; local task ownership cannot cross visits.
}

func New(store *Store, provider Provider, promptPath string) (*Agent, error) {
	if provider == nil {
		provider = MockProvider{}
	}
	prompt := DefaultPrompt
	if promptPath != "" {
		data, err := os.ReadFile(promptPath)
		if err != nil {
			return nil, fmt.Errorf("load runtime prompt: %w", err)
		}
		prompt = string(data)
	}
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("runtime prompt is empty")
	}
	a := &Agent{store: store, provider: provider, prompt: prompt, promptID: promptHash(prompt), registry: NewToolRegistry(), contextConfig: DefaultContextConfig(), workers: newWorkerLifecycle(), mcpState: &mcpLifecycle{}}
	if err := a.ConfigureModels(nil, RoleModels{}); err != nil {
		return nil, err
	}
	return a, nil
}

func promptHash(prompt string) string {
	hash := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(hash[:])
}
func (a *Agent) Store() *Store      { return a.store }
func (a *Agent) PromptHash() string { return a.promptID }

func (a *Agent) Registry() *ToolRegistry { return a.registry }

func (a *Agent) AddLocalTools(manifests []localtool.Manifest) error {
	return a.registry.AddLocal(manifests)
}

func (a *Agent) AddMCP(ctx context.Context, config mcp.Config) ([]mcp.Tool, error) {
	a.mcpState.mu.Lock()
	defer a.mcpState.mu.Unlock()
	if a.mcpState.closed {
		return nil, fmt.Errorf("agent MCP lifecycle is closed")
	}
	if _, exists := a.registry.configs[config.Name]; exists || a.registry.disabledMCP[config.Name] {
		return nil, fmt.Errorf("MCP server %q is already registered", config.Name)
	}
	if config.Disabled {
		if a.registry.disabledMCP == nil {
			a.registry.disabledMCP = map[string]bool{}
		}
		a.registry.disabledMCP[config.Name] = true
		return nil, nil
	}
	client, err := mcp.Dial(ctx, config)
	if err != nil {
		var connection *mcp.ConnectionError
		if errors.As(err, &connection) {
			return nil, err
		}
		return nil, &mcp.ConnectionError{Name: config.Name, Status: mcp.Failure("connect", err), Err: err}
	}
	tools, err := client.Discover(ctx)
	if err != nil {
		status := client.Status()
		failure := mcp.Failure("discovery", err)
		failure.ProtocolVersion, failure.Capabilities = status.ProtocolVersion, status.Capabilities
		closeErr := client.Close()
		return nil, &mcp.ConnectionError{Name: config.Name, Status: failure.WithCleanup("cleanup", closeErr), Err: errors.Join(err, closeErr)}
	}
	if err := a.registry.AddMCP(config.Name, client, tools); err != nil {
		failure := mcp.Failure("registration", err)
		status := client.Status()
		failure.ProtocolVersion, failure.Capabilities = status.ProtocolVersion, status.Capabilities
		closeErr := client.Close()
		return nil, &mcp.ConnectionError{Name: config.Name, Status: failure.WithCleanup("cleanup", closeErr), Err: errors.Join(err, closeErr)}
	}
	a.mcpClients = append(a.mcpClients, client)
	a.registry.SetMCPConfig(config.Name, config)
	return tools, nil
}

func (a *Agent) Create(goal string, attachments ...InputAttachment) (Snapshot, error) {
	return a.CreateConfigured(goal, RunConfiguration{}, attachments...)
}

type RunConfiguration struct {
	CompletionCriteria string
	inheritedRun       *Run
	RootContract       *RootContract
	Exec               *ExecConfig
	ToolSchema         ToolSchemaConfig
	ModelSelection     *ModelSelection
	role               string
	TreeBudget         *TreeBudget
	Requirements       []VerificationRequirement
	SuccessContains    *string
	Budget             *Budget
	Title              string
}

func (a *Agent) CreateConfigured(goal string, config RunConfiguration, attachments ...InputAttachment) (Snapshot, error) {
	if strings.TrimSpace(goal) == "" {
		return Snapshot{}, fmt.Errorf("goal is required")
	}
	execConfig, toolSchema, toolSelection, err := a.executionConfiguration(config)
	if err != nil {
		return Snapshot{}, err
	}
	if err := ValidateRequirements(config.Requirements); err != nil {
		return Snapshot{}, err
	}
	if config.Budget != nil {
		if err := ValidateBudget(*config.Budget); err != nil {
			return Snapshot{}, err
		}
	}
	if config.TreeBudget != nil {
		if err := ValidateTreeBudget(*config.TreeBudget); err != nil {
			return Snapshot{}, err
		}
	}
	selection := config.ModelSelection
	if selection == nil {
		selection = a.roleModels.Orchestrator
		if config.role == "worker" {
			selection = a.roleModels.Worker
		}
	}
	selected, provider, err := a.resolveModel(selection, false)
	if err != nil {
		return Snapshot{}, err
	}
	local := *a
	local.provider = provider
	a = &local
	attachments, err = a.resolveInputAttachments(attachments, false)
	if err != nil {
		return Snapshot{}, err
	}
	return a.store.createInitialized(goal, a.prompt, a.promptID, func(s *Snapshot) error {
		s.Run.Exec, s.Run.ToolSchema, s.Run.ToolSelection = execConfig, toolSchema, toolSelection
		s.Run.Responsibility = ResponsibilityCoordinator
		if config.role == "worker" {
			s.Run.Responsibility = ResponsibilityWorker
		}
		s.Run.ModelSelection = selected
		s.Run.Context = a.contextConfig
		if a.treeBudget != nil {
			s.Run.TreeBudget = *a.treeBudget
		}
		if config.TreeBudget != nil {
			s.Run.TreeBudget = *config.TreeBudget
		}
		s.Run.Budget = DefaultBudget()
		if a.budget.MaxModelRequests != nil {
			s.Run.Budget.MaxModelRequests = *a.budget.MaxModelRequests
		}
		if a.budget.MaxActions != nil {
			s.Run.Budget.MaxActions = *a.budget.MaxActions
		}
		if a.budget.MaxSeconds != nil {
			s.Run.Budget.MaxSeconds = *a.budget.MaxSeconds
		}
		if a.budget.CheckpointSeconds != nil {
			s.Run.Budget.CheckpointSeconds = *a.budget.CheckpointSeconds
		}
		s.Run.RequireVerifiedCompletion = a.requireVerifiedCompletion
		s.Run.SuccessContains = a.successContains
		if config.SuccessContains != nil {
			s.Run.SuccessContains = *config.SuccessContains
		}
		if config.Budget != nil {
			s.Run.Budget, s.Run.BudgetConfigured = *config.Budget, true
		}
		s.Run.Title = strings.TrimSpace(config.Title)
		s.Requirements = append([]VerificationRequirement(nil), config.Requirements...)
		if config.RootContract == nil && config.role != "worker" {
			ids := []string{}
			for _, r := range s.Requirements {
				ids = append(ids, r.ID)
			}
			contract := pinRootContract(s, RootContract{Version: 2, Goal: s.Run.Goal, CompletionCriteria: strings.TrimSpace(config.CompletionCriteria), RequirementIDs: ids})
			contract.AutoIntent = true
			s.RootContract = &contract
		}
		if config.RootContract != nil {
			if err := validateRootContract(s, *config.RootContract); err != nil {
				return err
			}
			contract := pinRootContract(s, *config.RootContract)
			s.RootContract = &contract
		}
		if len(attachments) > 0 {
			s.Messages = append(s.Messages, Message{Role: "user", Content: goal, Attachments: attachments, CreatedAt: time.Now().UTC()})
			if err := a.prepareInputMessages(s); err != nil {
				return err
			}
		}
		addEvent(s, "run_created", "运行已创建", map[string]any{"prompt_hash": s.Run.PromptHash, "prompt_bytes": s.Run.PromptBytes, "requirements": s.Requirements, "success_contains": s.Run.SuccessContains, "budget": s.Run.Budget})
		a.prepareRecon(s)
		return nil
	})
}

// ErrRunAdmission identifies a rejected execution lock acquisition. The caller
// never owned the run and must not persist this request error onto its state.
var ErrRunAdmission = errors.New("run admission failed")

func (a *Agent) Run(ctx context.Context, id string) (Snapshot, error) {
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrRunAdmission, err)
	}
	defer release()
	return a.runLocked(ctx, id)
}

func (a *Agent) runLocked(parent context.Context, id string) (final Snapshot, runErr error) {
	snapshot, err := a.reconcileExecResourcesLocked(id, worker.ReconcileResourceOwner)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.Run.Deleted {
		return Snapshot{}, fmt.Errorf("run was deleted from archive")
	}
	if snapshot.Run.ParentTaskID != "" && a.workerRunID != id {
		return a.resumeWorkerLocked(parent, snapshot)
	}
	a, err = a.withRunModel(snapshot.Run)
	if err != nil {
		return snapshot, err
	}
	if snapshot.Run.PromptHash != promptHash(snapshot.Prompt) {
		return a.fail(id, fmt.Errorf("saved prompt hash mismatch"))
	}
	restoredWaiting := false
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		if s.Run.Status == StatusCompleted || s.Run.Status == StatusCancelled {
			return fmt.Errorf("run is %s", s.Run.Status)
		}
		if err := a.checkWorkerAncestors(s); err != nil {
			return err
		}
		settleBudgetTime(s)
		applyBudget(s, a.budget)
		if a.contextOverride {
			s.Run.Context = a.contextConfig
		}
		pending := len(pendingToolCalls(s)) > 0
		waitingInput := strings.HasPrefix(s.Run.PauseReason, "waiting_input:")
		restoredWaiting = pending && (waitingInput || s.Run.Status == StatusPaused && !strings.Contains(s.Run.PauseReason, "budget"))
		if reason := budgetReason(s, "time"); reason != "" {
			pauseBudget(s, reason)
			return nil
		}
		if waitingInput && !pending {
			// request_input leaves a tool result as the latest message, so this
			// is not an idle continuation. Preserve the marker through any other
			// calls in its batch; append the answer only after all tool results.
			consumeMailbox(s, "follow_up")
		}
		s.Run.Error = ""
		if !pending && strings.HasPrefix(s.Run.PauseReason, "worker_yield:") {
			s.Messages = append(s.Messages, Message{Role: "user", Content: "Resume unfinished worker visit from checkpoint: " + s.Run.PauseReason + ". Inspect current external state before repeating an action.", CreatedAt: time.Now().UTC()})
		}
		if !waitingInput || !pending {
			s.Run.PauseReason = ""
		}
		for i := range s.Compactions {
			if s.Compactions[i].Status == "pending" {
				s.Compactions[i].Status = "interrupted"
				addEvent(s, "context_summary_interrupted", "恢复时保留未完成摘要请求，重新生成摘要仍计入预算", map[string]string{"request_id": s.Compactions[i].RequestID})
			}
		}
		if len(pendingToolCalls(s)) > 0 {
			s.Run.Status = StatusWaitingTool
		} else {
			s.Run.Status = StatusRunning
		}
		addEvent(s, "run_started", "运行开始", nil)
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	// An operator resume never silently executes an outstanding paused action.
	if restoredWaiting || isStopped(snap.Run.Status) {
		return snap, nil
	}
	ctx, stop, err := a.beginBudgetContext(parent, id)
	if err != nil {
		return Snapshot{}, err
	}
	defer func() {
		if saved, err := a.store.Load(id); err == nil && saved.Run.Status == StatusPaused {
			a.pauseSnapshotWorkers(saved)
		}
		stop()
		if saved, err := a.store.Load(id); err == nil {
			final = saved
		} else if runErr == nil {
			runErr = err
		}
	}()
	return a.runPi(ctx, id)
}

func validateResponse(r ModelResponse) error {
	if r.StopReason != "" && r.StopReason != "stop" && r.StopReason != "tool_calls" {
		return fmt.Errorf("model stopped with reason: %s", r.StopReason)
	}
	if len(r.ToolCalls) == 0 {
		if r.StopReason == "tool_calls" {
			return fmt.Errorf("model declared tool calls without calls")
		}
		if strings.TrimSpace(r.Content) == "" {
			return fmt.Errorf("model returned an empty response")
		}
	}
	seen := map[string]bool{}
	for _, call := range r.ToolCalls {
		if call.ID == "" || call.Name == "" || seen[call.ID] {
			return fmt.Errorf("invalid or duplicate tool call id: %q", call.ID)
		}
		seen[call.ID] = true
	}
	return nil
}

func (a *Agent) fail(id string, cause error) (Snapshot, error) {
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		if isStopped(s.Run.Status) {
			return nil
		}
		if reportDeliveryPending(s) {
			// Report translation is a delivery concern, not an operator gate.
			// Keep the business result and source ledger, but terminate on a
			// non-retryable delivery failure instead of pausing for confirmation.
			s.Run.Status, s.Run.PauseReason = StatusFailed, ""
			addEvent(s, "report_translation_failed", "报告交付失败；保留业务结果与原始来源", map[string]string{"error": cause.Error()})
		} else {
			s.Run.Status = StatusFailed
			addEvent(s, "run_error", "运行失败", map[string]string{"error": cause.Error()})
		}
		s.Run.Error = cause.Error()
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	if isStopped(snap.Run.Status) {
		return snap, nil
	}
	return snap, cause
}

func isStopped(status Status) bool { return status == StatusPaused || status == StatusCancelled }

// Persisted controls are visible to a CLI running in another process.
func (a *Agent) controlledContext(parent context.Context, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	changed, unsubscribe := a.store.SubscribeScheduling(id)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer unsubscribe()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			// Subscribe before the first read; hints only follow durable commits.
			// Polling remains necessary for writers in another process/Store.
			if ctx.Err() != nil {
				return
			}
			s, err := a.store.Observe(id)
			if err != nil || s.Run.Deleted || isStopped(s.Run.Status) {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-changed:
			}
		}
	}()
	return ctx, func() { cancel(); <-done }
}

func (a *Agent) Message(ctx context.Context, id, content string, attachments ...InputAttachment) (Snapshot, error) {
	if strings.TrimSpace(content) == "" {
		return Snapshot{}, fmt.Errorf("message is required")
	}
	attachments, err := a.resolveInputAttachments(attachments, false)
	if err != nil {
		return Snapshot{}, err
	}
	release, err := a.store.AcquireRunLock(id)
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %w", ErrRunAdmission, err)
	}
	defer release()
	snapshot, err := a.store.Load(id)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.Run.Status == StatusPaused {
		// Use the existing queue so pending tool results precede new input.
		// Run keeps the persisted budget and paused-action resolution gates.
		if _, err := a.QueueMessage(id, "steer", content, attachments...); err != nil {
			return Snapshot{}, err
		}
		return a.runLocked(ctx, id)
	}
	_, err = a.store.Update(id, func(s *Snapshot) error {
		if s.Run.Status == StatusRunning || s.Run.Status == StatusWaitingTool || isStopped(s.Run.Status) {
			return fmt.Errorf("run is %s", s.Run.Status)
		}
		s.Run.Status = StatusCreated
		s.Run.Error = ""
		if len(s.Messages) == 0 {
			s.Messages = append(s.Messages, Message{Role: "user", Content: s.Run.Goal, CreatedAt: time.Now().UTC()})
		}
		s.Messages = append(s.Messages, Message{Role: "user", Content: content, Attachments: attachments, CreatedAt: time.Now().UTC()})
		if err := a.prepareInputMessages(s); err != nil {
			return err
		}
		addEvent(s, "message_added", "补充指令已保存", nil)
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	return a.runLocked(ctx, id)
}

func (a *Agent) Pause(id string) (Snapshot, error) {
	// A retry on an already stopped run needs only the current header. The
	// mutating path still rechecks under Update's lock and provenance gates.
	run, err := a.store.listRun(id)
	if err != nil {
		return Snapshot{}, err
	}
	if run.Deleted {
		return Snapshot{}, fmt.Errorf("run was deleted from archive")
	}
	if run.Status != StatusRunning && run.Status != StatusWaitingTool {
		return Snapshot{}, fmt.Errorf("run is not active")
	}
	snap, err := a.store.updatePause(id, func(s *Snapshot) error {
		if s.Run.Status != StatusRunning && s.Run.Status != StatusWaitingTool {
			return fmt.Errorf("run is not active")
		}
		s.Run.Status = StatusPaused
		s.Run.PauseReason = "operator_pause"
		addEvent(s, "run_paused", "已请求暂停运行", nil)
		return nil
	})
	if err == nil {
		err = a.pauseSnapshotWorkers(snap)
	}
	return snap, err
}
func (a *Agent) Cancel(id string) (Snapshot, error) {
	snap, err := a.store.Update(id, func(s *Snapshot) error {
		if s.Run.Status == StatusCompleted || s.Run.Status == StatusCancelled {
			return fmt.Errorf("run is already terminal")
		}
		s.Run.Status = StatusCancelled
		addEvent(s, "run_cancelled", "已请求取消运行", nil)
		return nil
	})
	if err == nil && snap.Run.ParentRunID != "" && snap.Run.ParentTaskID != "" {
		_, err = a.settleCancelledChild(snap.Run.ParentRunID, id)
	}
	return snap, err
}

func pendingToolCalls(s *Snapshot) map[string]ToolCall {
	pending := map[string]ToolCall{}
	for _, m := range s.Messages {
		for _, call := range m.ToolCalls {
			pending[call.ID] = call
		}
		if m.Role == "tool" {
			delete(pending, m.ToolCallID)
		}
	}
	return pending
}

func (a *Agent) pendingWorkerInput(s *Snapshot) (bool, error) {
	for _, m := range s.WorkerMessages {
		if m.To == "parent" && workerMessageWakes(m) {
			return true, nil
		}
	}
	if s.Run.ParentRunID == "" || s.Run.ParentTaskID == "" {
		return false, nil
	}
	parent, err := a.store.Observe(s.Run.ParentRunID)
	if err != nil {
		return false, err
	}
	for _, w := range parent.Workers {
		if w.RunID != s.Run.ID {
			continue
		}
		for _, m := range parent.WorkerMessages {
			if m.To == w.ID && workerMessageWakes(m) {
				return true, nil
			}
		}
	}
	return false, nil
}
