package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"trace/internal/worker"
)

type Status string

const (
	StatusCreated     Status = "created"
	StatusRunning     Status = "running"
	StatusWaitingTool Status = "waiting_tool"
	StatusPaused      Status = "paused"
	StatusCancelled   Status = "cancelled"
	StatusCompleted   Status = "completed"
	StatusFailed      Status = "failed"
)

// RunResponsibility is pinned at creation; the empty value preserves historical runs.
type RunResponsibility string

const (
	ResponsibilityCoordinator RunResponsibility = "coordinator"
	ResponsibilityWorker      RunResponsibility = "worker"
)

type Run struct {
	Responsibility            RunResponsibility `json:"responsibility,omitempty"`
	Exec                      *ExecConfig       `json:"exec,omitempty"`
	ToolSchema                ToolSchemaConfig  `json:"tool_schema,omitempty"`
	ModelSelection            *ModelSelection   `json:"model_selection,omitempty"`
	ToolSelection             *ToolSelection    `json:"tool_selection,omitempty"`
	ParentTaskID              string            `json:"parent_task_id,omitempty"`
	Context                   ContextConfig     `json:"context"`
	ParentRunID               string            `json:"parent_run_id,omitempty"`
	ParentMessageID           string            `json:"parent_message_id,omitempty"`
	ID                        string            `json:"id"`
	Goal                      string            `json:"goal"`
	Title                     string            `json:"title,omitempty"`
	Archived                  bool              `json:"archived,omitempty"`
	Status                    Status            `json:"status"`
	Deleted                   bool              `json:"deleted,omitempty"`
	PromptHash                string            `json:"prompt_hash"`
	PromptBytes               int               `json:"prompt_bytes"`
	CreatedAt                 time.Time         `json:"created_at"`
	UpdatedAt                 time.Time         `json:"updated_at"`
	EventSeq                  int               `json:"event_seq"`
	RequestSeq                int               `json:"request_seq"`
	Error                     string            `json:"error,omitempty"`
	PauseReason               string            `json:"pause_reason,omitempty"`
	Budget                    Budget            `json:"budget"`
	TreeBudget                TreeBudget        `json:"tree_budget,omitempty"`
	ResourceLimits            *ResourceLimits   `json:"resource_limits,omitempty"`
	BudgetCheckpoint          *BudgetCheckpoint `json:"budget_checkpoint,omitempty"`
	BudgetConfigured          bool              `json:"budget_configured,omitempty"`
	ElapsedMillis             int64             `json:"elapsed_millis"`
	BudgetActiveSince         time.Time         `json:"budget_active_since,omitempty"`
	Usage                     UsageStats        `json:"usage"`
	SuccessContains           string            `json:"success_contains,omitempty"`
	RequireVerifiedCompletion bool              `json:"require_verified_completion,omitempty"`
}

type Message struct {
	ReportOnly  bool              `json:"report_only,omitempty"`
	RequestID   string            `json:"request_id,omitempty"`
	Attachments []InputAttachment `json:"attachments,omitempty"`
	ID          string            `json:"id"`
	ParentID    string            `json:"parent_id,omitempty"`
	Thinking    string            `json:"thinking,omitempty"`
	Role        string            `json:"role"`
	Content     string            `json:"content"`
	ToolCallID  string            `json:"tool_call_id,omitempty"`
	ToolCalls   []ToolCall        `json:"tool_calls,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
}

type Event struct {
	Sequence  int       `json:"sequence"`
	Type      string    `json:"type"`
	Summary   string    `json:"summary"`
	Data      any       `json:"data,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ModelRequest struct {
	// Process-local preparation clocks; copied into the response diagnostic only.
	prepareMillis   *int64
	archiveMillis   *int64
	ToolMetrics     *ToolRequestMetrics `json:"tool_metrics,omitempty"`
	ReasoningEffort string              `json:"reasoning_effort,omitempty"`
	ContextManifest *ContextManifest    `json:"context_manifest,omitempty"`
	ProviderModel   string              `json:"provider_model,omitempty"`
	ProviderID      string              `json:"provider_id,omitempty"`
	Price           *ModelPrice         `json:"price,omitempty"`
	PlannerThrough  int                 `json:"planner_through,omitempty"`
	Purpose         string              `json:"purpose,omitempty"`
	MaxOutputTokens int                 `json:"max_output_tokens,omitempty"`
	ContextTokens   int                 `json:"context_tokens"`
	CompactionID    string              `json:"compaction_id,omitempty"`
	ID              string              `json:"id"`
	PromptHash      string              `json:"prompt_hash"`
	Messages        []Message           `json:"messages"`
	Tools           []ToolDefinition    `json:"tools,omitempty"`
	CreatedAt       time.Time           `json:"created_at"`
}

type ToolDefinition struct {
	Type     string `json:"type"`
	Function struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Parameters  any    `json:"parameters"`
	} `json:"function"`
}

// UnmarshalJSON preserves the JSON numbers in archived tool parameters.
func (d *ToolDefinition) UnmarshalJSON(raw []byte) error {
	type definition ToolDefinition
	return decodeToolJSON(raw, (*definition)(d))
}

// ToolDefinition's only interface field is Function.Parameters.
func decodeToolJSON(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("tool JSON must contain one value")
	}
	return nil
}

type ModelResponse struct {
	ReportedModel       string            `json:"reported_model,omitempty"`
	ProviderResponseID  string            `json:"provider_response_id,omitempty"`
	ReportedServiceTier string            `json:"reported_service_tier,omitempty"`
	IdentityConflict    bool              `json:"identity_conflict,omitempty"`
	ReportedCost        *ProviderCost     `json:"reported_cost,omitempty"`
	Diagnostics         *ModelDiagnostics `json:"diagnostics,omitempty"`
	Thinking            string            `json:"thinking,omitempty"`
	Content             string            `json:"content"`
	ToolCalls           []ToolCall        `json:"tool_calls,omitempty"`
	StopReason          string            `json:"stop_reason,omitempty"`
	Usage               *TokenUsage       `json:"usage,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// A started action without a durable result has an unknown external outcome.
// Recovery records it as uncertain rather than replaying a side effect.
type Action struct {
	ResourceOwner         string      `json:"resource_owner,omitempty"`
	ResourceRoot          string      `json:"resource_root,omitempty"`
	ResourceRootIdentity  string      `json:"resource_root_identity,omitempty"`
	TimeoutSeconds        int         `json:"timeout_seconds,omitempty"`
	RequestID             string      `json:"request_id"`
	CallID                string      `json:"call_id"`
	Name                  string      `json:"name"`
	Adapter               string      `json:"adapter,omitempty"`
	RemoteName            string      `json:"remote_name,omitempty"`
	RemoteMethod          string      `json:"remote_method,omitempty"`
	ServerName            string      `json:"server_name,omitempty"`
	LocalName             string      `json:"local_name,omitempty"`
	Arguments             string      `json:"arguments"`
	Command               []string    `json:"command,omitempty"`
	Status                string      `json:"status"`
	StartedAt             time.Time   `json:"started_at"`
	FinishedAt            time.Time   `json:"finished_at,omitempty"`
	Result                *ToolResult `json:"result,omitempty"`
	ResourceCleanupStatus string      `json:"resource_cleanup_status,omitempty"`
	ResourceCleanupError  string      `json:"resource_cleanup_error,omitempty"`

	PlanTaskID             string `json:"plan_task_id,omitempty"`
	ExecutorOwnershipError string `json:"executor_ownership_error,omitempty"`
}

type ToolResult struct {
	WorkerResourceOwner string `json:"worker_resource_owner,omitempty"`
	WorkerResourceLimit string `json:"worker_resource_limit,omitempty"`
	// Historical JSON only: keep original field order for byte-exact evidence verification.
	// New tools neither populate nor interpret these retired integration fields.
	LegacyREASession       json.RawMessage `json:"rea_session,omitempty"`
	LegacyREACheckpoint    json.RawMessage `json:"rea_checkpoint,omitempty"`
	LegacyREARestore       json.RawMessage `json:"rea_restore,omitempty"`
	StdoutPath             string          `json:"-"`
	StderrPath             string          `json:"-"`
	Artifacts              []ArtifactRef   `json:"artifacts,omitempty"`
	EvidenceID             string          `json:"evidence_id,omitempty"`
	Stdout                 string          `json:"stdout"`
	Stderr                 string          `json:"stderr"`
	ExitCode               int             `json:"exit_code"`
	Error                  string          `json:"error,omitempty"`
	Interrupted            bool            `json:"interrupted,omitempty"`
	MCPResult              json.RawMessage `json:"mcp_result,omitempty"`
	MCPIsError             bool            `json:"mcp_is_error,omitempty"`
	WorkerPID              int             `json:"worker_pid,omitempty"`
	WorkerStatus           worker.Status   `json:"worker_status,omitempty"`
	WorkerOutcomeUncertain bool            `json:"worker_outcome_uncertain,omitempty"`
	OutcomeUncertain       bool            `json:"outcome_uncertain,omitempty"`
	WorkerCleanupError     string          `json:"worker_cleanup_error,omitempty"`
	Diagnostic             *ToolDiagnostic `json:"diagnostic,omitempty"`
	executionEndedAt       time.Time
}

type ResponseRecord struct {
	RequestID string        `json:"request_id"`
	Response  ModelResponse `json:"response"`
	Error     string        `json:"error,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}
