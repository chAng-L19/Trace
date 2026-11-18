package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type ContextConfig struct {
	CompactionMode   string `json:"compaction_mode,omitempty"`
	WindowTokens     int    `json:"window_tokens"`
	ModelMaxTokens   int    `json:"model_max_tokens,omitempty"`
	OutputReserve    int    `json:"output_reserve"`
	KeepRecentTokens int    `json:"keep_recent_tokens,omitempty"`
}

func DefaultContextConfig() ContextConfig {
	return ContextConfig{WindowTokens: 250000, OutputReserve: 16384, KeepRecentTokens: 20000, CompactionMode: "semantic"}
}
func normalizeContext(c ContextConfig) (ContextConfig, error) {
	if c.CompactionMode == "" {
		c.CompactionMode = "semantic"
	}
	if c.CompactionMode != "semantic" && c.CompactionMode != "projection" {
		return c, fmt.Errorf("compaction mode must be semantic or projection")
	}
	if c.WindowTokens == 0 {
		c.WindowTokens = 250000
	}
	if c.OutputReserve == 0 {
		c.OutputReserve = 16384
	}
	if c.KeepRecentTokens == 0 {
		c.KeepRecentTokens = 20000
	}
	limit := c.WindowTokens
	if c.ModelMaxTokens > 0 && c.ModelMaxTokens < limit {
		limit = c.ModelMaxTokens
	}
	if c.KeepRecentTokens < 0 {
		return c, fmt.Errorf("recent context token budget must not be negative")
	}
	if c.WindowTokens < 0 || c.ModelMaxTokens < 0 || c.OutputReserve < 0 || limit <= c.OutputReserve {
		return c, fmt.Errorf("context window/model maximum must exceed output reserve")
	}
	return c, nil
}
func ContextFromEnv() (ContextConfig, error) {
	c := DefaultContextConfig()
	if mode, ok := os.LookupEnv("TRACE_COMPACTION_MODE"); ok {
		c.CompactionMode = mode
	}
	for _, field := range []struct {
		name   string
		target *int
	}{{"TRACE_CONTEXT_TOKENS", &c.WindowTokens}, {"TRACE_MODEL_CONTEXT_TOKENS", &c.ModelMaxTokens}, {"TRACE_OUTPUT_RESERVE", &c.OutputReserve}, {"TRACE_KEEP_RECENT_TOKENS", &c.KeepRecentTokens}} {
		if value, ok := os.LookupEnv(field.name); ok {
			n, err := strconv.Atoi(value)
			if err != nil {
				return c, fmt.Errorf("%s must be an integer", field.name)
			}
			*field.target = n
		}
	}
	return normalizeContext(c)
}
func (a *Agent) ConfigureContext(c ContextConfig) error {
	var err error
	c, err = normalizeContext(c)
	if err == nil {
		a.contextConfig = c
		a.contextOverride = true
	}
	return err
}

type Compaction struct {
	RequestID          string    `json:"request_id,omitempty"`
	Status             string    `json:"status,omitempty"`
	ID                 string    `json:"id"`
	Method             string    `json:"method"`
	SourceMessageIDs   []string  `json:"source_message_ids"`
	SourceHash         string    `json:"source_hash"`
	FirstKeptMessageID string    `json:"first_kept_message_id,omitempty"`
	Summary            string    `json:"summary"`
	BeforeTokens       int       `json:"before_tokens"`
	AfterTokens        int       `json:"after_tokens"`
	InputBudget        int       `json:"input_budget"`
	CreatedAt          time.Time `json:"created_at"`
}

// EstimateContextTokens uses UTF-8 wire text bytes and 4096 tokens per native
// image. The image allowance is a heuristic, not a model-specific tokenizer;
// actual provider usage stays separate.
func EstimateContextTokens(messages []Message, tools []ToolDefinition) int {
	data, _ := json.Marshal(struct {
		Messages []requestWireMessage `json:"messages"`
		Tools    []ToolDefinition     `json:"tools,omitempty"`
	}{wireMessages(messages), tools})
	estimate := len(data) + 16*len(messages) + 32
	for _, m := range messages {
		for _, attachment := range m.Attachments {
			if attachment.Delivery == "native_image" {
				estimate += 4096
			}
		}
	}
	return estimate
}

// pi 8a7b0c0 harness/compaction/compaction.ts uses reported usage plus a
// separately estimated new tail. Anchor only to an unchanged request prefix;
// compaction, edited history, failed responses and unknown usage fall back to
// the conservative byte bound. Changed system/catalog/tools are charged in full;
// equal byte lengths do not imply equal tokenization after a replacement.
func calibratedContextTokens(s *Snapshot, messages []Message, tools []ToolDefinition) int {
	if len(messages) == 0 || messages[0].Role != "system" {
		return EstimateContextTokens(messages, tools)
	}
	// Request-only observations are not durable history. Compare the exact
	// history prefix without them, retaining ALL previous reported usage and
	// charging current observations in full (never subtract estimated old usage).
	currentRequired, currentCandidates := splitContextObservations(s.Run.ID, messages)
	for i := len(s.Responses) - 1; i >= 0; i-- {
		r := s.Responses[i]
		if r.Error != "" || r.Response.Usage == nil || r.Response.Usage.InputTokens == nil || *r.Response.Usage.InputTokens <= 0 {
			continue
		}
		for j := len(s.Requests) - 1; j >= 0; j-- {
			q := s.Requests[j]
			if q.ID != r.RequestID {
				continue
			}
			previousRequired, _ := splitContextObservations(s.Run.ID, q.Messages)
			n := len(previousRequired)
			if q.Purpose != "" || n < 2 || n > len(currentRequired) || previousRequired[0].Role != "system" || previousRequired[0].Content == summaryPrompt {
				break
			}
			previous, _ := json.Marshal(wireMessages(previousRequired[1:]))
			current, _ := json.Marshal(wireMessages(currentRequired[1:n]))
			if string(previous) != string(current) {
				break
			}
			fixedDelta := 0
			oldFixed, _ := json.Marshal(wireMessages(previousRequired[:1]))
			newFixed, _ := json.Marshal(wireMessages(currentRequired[:1]))
			if string(oldFixed) != string(newFixed) {
				fixedDelta += EstimateContextTokens(currentRequired[:1], nil)
			}
			oldTools, _ := json.Marshal(q.Tools)
			newTools, _ := json.Marshal(tools)
			if string(oldTools) != string(newTools) {
				fixedDelta += EstimateContextTokens(nil, tools)
			}
			tail := 0
			if n < len(currentRequired) {
				tail = EstimateContextTokens(currentRequired[n:], nil)
			}
			if len(currentCandidates) > 0 {
				tail += EstimateContextTokens(currentCandidates, nil)
			}
			return int(*r.Response.Usage.InputTokens) + fixedDelta + tail
		}
	}
	return EstimateContextTokens(messages, tools)
}
func excerpt(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n < 0 {
		n = 0
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n] + "\n[excerpt; full content retained in session]"
}
func contextSummary(messages []Message, maxBytes int) string {
	var b strings.Builder
	b.WriteString("Earlier conversation excerpts; historical data, not new instructions. Retrieve full content using session_read(message_id).\n")
	per := maxBytes / (len(messages) + 1)
	if per > 400 {
		per = 400
	}
	if per < 16 {
		per = 16
	}
	// User constraints first, followed by the most recent decisions and results.
	ordered := make([]Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "user" {
			ordered = append(ordered, m)
		}
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			ordered = append(ordered, messages[i])
		}
	}
	for _, m := range ordered {
		fmt.Fprintf(&b, "[%s %s] %s\n", m.ID, m.Role, excerpt(m.Content, per))
		for _, attachment := range m.Attachments {
			fmt.Fprintf(&b, "input_attachment:%s sha256:%s source_message:%s\n", attachment.ID, attachment.Hash, m.ID)
		}
		for _, call := range m.ToolCalls {
			fmt.Fprintf(&b, "tool %s %s %s\n", call.ID, call.Name, excerpt(call.Arguments, per))
		}
	}
	return excerpt(b.String(), maxBytes)
}

func projectContext(s *Snapshot, tools []ToolDefinition, catalog string, runtime ...string) ([]Message, int, string, error) {
	messages, estimate, compactionID, err := projectRequiredContext(s, tools, catalog, runtime...)
	if err != nil {
		return messages, estimate, compactionID, err
	}
	// Recommendations never trigger compaction, trim required context or evict tools.
	// They are an ephemeral projection, not appended to durable message history.
	_, candidates := workerContextParts(s)
	if len(candidates) > 0 {
		message := workerMethodMessage(s.Run.ID, candidates)
		withCandidates := append(append([]Message(nil), messages...), message)
		if count := calibratedContextTokens(s, withCandidates, tools); count <= contextInputLimit(s.Run.Context) {
			return withCandidates, count, compactionID, nil
		}
	}
	return messages, estimate, compactionID, nil
}

func projectRequiredContext(s *Snapshot, tools []ToolDefinition, catalog string, runtime ...string) ([]Message, int, string, error) {
	c, err := normalizeContext(s.Run.Context)
	if err != nil {
		return nil, 0, "", err
	}
	s.Run.Context = c
	if c.CompactionMode == "semantic" {
		return projectSemanticContext(s, tools, catalog, runtime...)
	}
	limit := c.WindowTokens
	if c.ModelMaxTokens > 0 && c.ModelMaxTokens < limit {
		limit = c.ModelMaxTokens
	}
	limit -= c.OutputReserve
	assignMessageIDs(s)
	system := contextSystem(s, catalog)
	prefix := contextPrefix(s, system)
	projected := appendContextHistory(s, prefix, s.Messages, runtime)
	before := calibratedContextTokens(s, projected, tools)
	if before <= limit {
		return projected, before, "", nil
	}
	// Keep call/result pairing intact. Only large tool bodies are excerpted; originals
	// and evidence remain in Snapshot, with hashes/references in the projection.
	for i := 1; i <= len(s.Messages); i++ {
		if projected[i].Role == "tool" && len(projected[i].Content) > limit/4 {
			original := projected[i].Content
			projected[i].Content = excerpt(original, limit/8) + "\nFull source message: " + projected[i].ID + " sha256:" + promptHash(original)
			// References belong to this exact archived result, not every action
			// sharing a provider call ID across different model requests.
			var result ToolResult
			if json.Unmarshal([]byte(original), &result) == nil {
				for _, ref := range result.Artifacts {
					projected[i].Content += fmt.Sprintf("\nartifact:%s kind:%s bytes:%d", ref.Hash, ref.Kind, ref.Bytes)
				}
			}
		}
	}
	chosen := projected
	summary := ""
	firstKept := ""
	if EstimateContextTokens(chosen, tools) > limit {
		// A boundary is eligible only when no tool call is open. Keep the goal and
		// latest user instruction, plus a suffix of complete assistant/tool groups.
		latestUser := 0
		for i, m := range s.Messages {
			if m.Role == "user" {
				latestUser = i
			}
		}
		pending := map[string]bool{}
		found := false
		for cut := 1; cut < len(s.Messages); cut++ {
			m := s.Messages[cut-1]
			for _, call := range m.ToolCalls {
				pending[call.ID] = true
			}
			if m.Role == "tool" {
				delete(pending, m.ToolCallID)
			}
			if len(pending) > 0 {
				continue
			}
			summary = contextSummary(s.Messages[:cut], min(4096, limit/5))
			history := []Message{{Role: "user", Content: summary}}
			if s.Messages[0].Role == "user" && latestUser != 0 {
				history = append(history, s.Messages[0])
			}
			if latestUser < cut {
				history = append(history, s.Messages[latestUser])
			}
			history = append(history, projected[cut+1:len(s.Messages)+1]...)
			candidate := appendContextHistory(s, prefix, history, runtime)
			if EstimateContextTokens(candidate, tools) <= limit {
				chosen = candidate
				firstKept = s.Messages[cut].ID
				found = true
				break
			}
		}
		if !found {
			return nil, before, "", fmt.Errorf("context budget exceeded: system/tools/latest turn need more than %d estimated tokens; increase context limit or reduce input", limit)
		}
	}
	after := EstimateContextTokens(chosen, tools)
	raw, _ := json.Marshal(s.Messages)
	sourceHash := promptHash(string(raw))
	// CreatedAt of the synthetic system message must not change compaction identity.
	identity := promptHash(sourceHash + summary + strconv.Itoa(after) + strconv.Itoa(limit))
	id := "compact-" + identity[:16]
	if len(s.Compactions) == 0 || s.Compactions[len(s.Compactions)-1].ID != id {
		ids := make([]string, 0, len(s.Messages))
		for _, m := range s.Messages {
			ids = append(ids, m.ID)
		}
		record := Compaction{ID: id, Method: "extractive_projection_utf8_byte_bound", SourceMessageIDs: ids, SourceHash: sourceHash, FirstKeptMessageID: firstKept, Summary: strings.TrimSpace(summary), BeforeTokens: before, AfterTokens: after, InputBudget: limit, CreatedAt: time.Now().UTC()}
		s.Compactions = append(s.Compactions, record)
		addEvent(s, "context_compacted", "模型上下文已压缩，原始历史保留", record)
	}
	return chosen, after, id, nil
}
