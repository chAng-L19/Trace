package agent

import "encoding/json"

// Result distinguishes a finished model turn from verified target success.
type Result struct {
	Remaining   []RemainingWork `json:"remaining,omitempty"`
	Report      *ReportText     `json:"report,omitempty"`
	Status      string          `json:"status"`
	Summary     string          `json:"summary"`
	Answers     []Answer        `json:"answers"`
	EvidenceIDs []string        `json:"evidence_ids"`
	// Supplemental references never establish outcome proof or waive answers/checks.
	SupportingEvidenceIDs []string                `json:"supporting_evidence_ids,omitempty"`
	ArtifactEvidenceIDs   []string                `json:"artifact_evidence_ids,omitempty"`
	ValidationIssues      []ResultValidationIssue `json:"validation_issues,omitempty"`
	ReproductionSteps     []string                `json:"reproduction_steps"`
	CleanupStatus         string                  `json:"cleanup_status"`
	TerminalReason        string                  `json:"terminal_reason"`
}

// ValidationIssues are recomputed from the ledger, never trusted from a claim.
// AnswerIndex is zero-based; Field identifies the result JSON member.
type ResultValidationIssue struct {
	Code        string `json:"code"`
	Field       string `json:"field"`
	EvidenceID  string `json:"evidence_id,omitempty"`
	ToolID      string `json:"tool_id,omitempty"`
	Adapter     string `json:"adapter,omitempty"`
	AnswerIndex *int   `json:"answer_index,omitempty"`
	Blocking    bool   `json:"blocking"`
	Repair      string `json:"repair"`
}

type Answer struct {
	Title       string   `json:"title,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Value       string   `json:"value"`
	Kind        string   `json:"kind"`
	EvidenceIDs []string `json:"evidence_ids"`
	Verified    bool     `json:"verified"`
}

type Evidence struct {
	Scope               *EvidenceScope `json:"evidence_scope,omitempty"`
	ID                  string         `json:"id"`
	RequestID           string         `json:"request_id"`
	CallID              string         `json:"call_id"`
	SourceEventSequence int            `json:"source_event_sequence"`
	ContentHash         string         `json:"content_hash"`
	Content             string         `json:"content"`
	Type                string         `json:"type"`
	Verification        string         `json:"verification"`
}

type Budget struct {
	CheckpointSeconds int64 `json:"checkpoint_seconds"`
	MaxModelRequests  int   `json:"max_model_requests"`
	MaxActions        int   `json:"max_actions"`
	MaxSeconds        int64 `json:"max_seconds"`
}

// Nil means preserve the persisted limit; zero explicitly means unlimited.
type BudgetOverride struct {
	CheckpointSeconds *int64
	MaxModelRequests  *int
	MaxActions        *int
	MaxSeconds        *int64
}

type TokenUsage struct {
	// Preserve native reports without inferring currency or treating missing as zero.
	Cost         json.RawMessage `json:"cost,omitempty"`
	CostCurrency json.RawMessage `json:"cost_currency,omitempty"`
	CostUnit     json.RawMessage `json:"cost_unit,omitempty"`
	InputTokens  *int64          `json:"prompt_tokens"`
	OutputTokens *int64          `json:"completion_tokens"`
	TotalTokens  *int64          `json:"total_tokens"`
	// Cache counts are subsets of prompt_tokens, never additional input.
	PromptCacheHitTokens  *int64              `json:"prompt_cache_hit_tokens,omitempty"`
	PromptCacheMissTokens *int64              `json:"prompt_cache_miss_tokens,omitempty"`
	CachedTokens          *int64              `json:"cached_tokens,omitempty"`
	PromptTokensDetails   *PromptTokenDetails `json:"prompt_tokens_details,omitempty"`
}

type PromptTokenDetails struct {
	CachedTokens     *int64 `json:"cached_tokens,omitempty"`
	CacheWriteTokens *int64 `json:"cache_write_tokens,omitempty"`
}

type UsageStats struct {
	ModelRequests         int              `json:"model_requests"`
	ModelResponses        int              `json:"model_responses"`
	ToolActions           int              `json:"tool_actions"`
	KnownInputTokens      *int64           `json:"known_input_tokens"`
	KnownOutputTokens     *int64           `json:"known_output_tokens"`
	KnownTotalTokens      *int64           `json:"known_total_tokens"`
	MissingUsageResponses int              `json:"missing_usage_responses"`
	PromptCache           PromptCacheStats `json:"prompt_cache"`
}
