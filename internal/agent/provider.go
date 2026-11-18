package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

type Provider interface {
	Complete(context.Context, ModelRequest) (ModelResponse, error)
}
type StreamingProvider interface {
	CompleteStream(context.Context, ModelRequest, func(StreamDelta) error) (ModelResponse, error)
}
type StreamDelta struct {
	Kind      string `json:"kind"`
	Text      string `json:"text,omitempty"`
	Index     int    `json:"index,omitempty"`
	ID        string `json:"id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type MockProvider struct{}

func (MockProvider) Complete(_ context.Context, req ModelRequest) (ModelResponse, error) {
	if len(req.Messages) == 0 || req.Messages[0].Role != "system" || req.Messages[0].Content == "" {
		return ModelResponse{}, fmt.Errorf("missing system prompt")
	}
	goal := ""
	runID := req.ID
	if at := strings.LastIndexByte(runID, '-'); at >= 0 {
		runID = runID[:at]
	}
	history, _ := splitContextObservations(runID, req.Messages)
	for _, msg := range history {
		if msg.Role == "user" {
			goal = msg.Content
		}
	}
	return ModelResponse{Content: "Mock Agent 已加载系统提示词 " + req.PromptHash[:12] + "。当前目标：" + goal}, nil
}

type OpenAIProvider struct {
	ModelCapabilities map[string]ModelCapabilities
	ReasoningEfforts  []string
	ReasoningEffort   string
	ImageInput        bool
	BaseURL           string
	APIKey            string
	Model             string
	Client            *http.Client
}

// ProviderHTTPError lets management probes report status without exposing a
// remote response body, which may echo credentials or request content.
type ProviderHTTPError struct {
	StatusCode int
	Status     string
	Body       string
	RetryAfter string
	Usage      *TokenUsage
	Response   ModelResponse
}

func (e *ProviderHTTPError) Error() string {
	return fmt.Sprintf("provider returned %s: %s", e.Status, e.Body)
}

type wireCall struct {
	Index    int    `json:"index,omitempty"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}
type wireMessage struct {
	Role       string     `json:"role,omitempty"`
	Content    string     `json:"content"`
	Thinking   string     `json:"reasoning_content,omitempty"`
	Reasoning  string     `json:"reasoning,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
}

type requestWireMessage struct {
	wireMessage
	Content any `json:"content"`
}

func wireMessages(messages []Message) []requestWireMessage {
	out := make([]requestWireMessage, 0, len(messages))
	for _, m := range messages {
		w := requestWireMessage{wireMessage: wireMessage{Role: m.Role, Thinking: m.Thinking, ToolCallID: m.ToolCallID}, Content: m.Content + attachmentManifest(m.Attachments)}
		var parts []any
		for _, attachment := range m.Attachments {
			if attachment.Delivery == "native_image" {
				if parts == nil {
					parts = append(parts, map[string]any{"type": "text", "text": w.Content})
				}
				url := attachment.dataURL
				// Estimates use identity metadata plus a fixed image allowance,
				// never persisted base64. Actual bytes are hydrated immediately before send.
				if url == "" {
					url = "cas:" + attachment.Hash
				}
				parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": url}})
			}
		}
		if parts != nil {
			w.Content = parts
		}
		for _, call := range m.ToolCalls {
			c := wireCall{ID: call.ID, Type: "function"}
			c.Function.Name = call.Name
			c.Function.Arguments = call.Arguments
			w.ToolCalls = append(w.ToolCalls, c)
		}
		out = append(out, w)
	}
	return out
}
func (p *OpenAIProvider) request(ctx context.Context, req ModelRequest, stream bool, started time.Time, diagnostic *ModelDiagnostics) (*http.Response, error) {
	identity, err := providerIdentity(p.BaseURL)
	if err != nil {
		return nil, err
	}
	if req.ProviderModel != "" && (req.ProviderModel != p.Model || req.ProviderID != identity || req.ReasoningEffort != p.ReasoningEffort) {
		return nil, fmt.Errorf("archived request model selection differs from provider route")
	}
	for _, m := range req.Messages {
		for _, attachment := range m.Attachments {
			if m.Role != "user" {
				return nil, fmt.Errorf("input attachments require user role")
			}
			if attachment.Delivery == "native_image" && (!p.ImageInput || attachment.dataURL == "") {
				return nil, fmt.Errorf("native image attachment is not hydrated or provider image input is disabled")
			}
		}
	}
	payload := struct {
		ReasoningEffort string               `json:"reasoning_effort,omitempty"`
		Model           string               `json:"model"`
		Messages        []requestWireMessage `json:"messages"`
		Tools           []ToolDefinition     `json:"tools,omitempty"`
		Stream          bool                 `json:"stream,omitempty"`
		StreamOptions   any                  `json:"stream_options,omitempty"`
		MaxTokens       int                  `json:"max_tokens,omitempty"`
	}{ReasoningEffort: p.ReasoningEffort, Model: p.Model, Messages: wireMessages(req.Messages), Tools: req.Tools, Stream: stream, MaxTokens: req.MaxOutputTokens}
	if stream {
		payload.StreamOptions = map[string]bool{"include_usage": true}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(p.BaseURL, "/")
	if !strings.HasSuffix(base, "/chat/completions") {
		base += "/chat/completions"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	if p.APIKey != "" {
		request.Header.Set("Authorization", "Bearer "+p.APIKey)
	}
	client := p.Client
	if client == nil {
		// Run cancellation and explicit budgets own the request lifetime, including
		// a continuously progressing reasoning stream.
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		defer response.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(firstByteReader{Reader: response.Body, start: started, diagnostic: diagnostic}, 8<<10))
		var failure wireResponse
		_ = json.Unmarshal(data, &failure)
		partial := ModelResponse{Usage: failure.Usage}
		partial.observeMetadata(failure)
		return nil, &ProviderHTTPError{StatusCode: response.StatusCode, Status: response.Status, Body: strings.TrimSpace(string(data)), RetryAfter: response.Header.Get("Retry-After"), Usage: failure.Usage, Response: partial}
	}
	return response, nil
}

type wireResponse struct {
	ID           json.RawMessage   `json:"id"`
	Model        json.RawMessage   `json:"model"`
	ServiceTier  json.RawMessage   `json:"service_tier"`
	ReportedCost json.RawMessage   `json:"reported_cost"`
	Cost         json.RawMessage   `json:"cost"`
	CostCurrency json.RawMessage   `json:"cost_currency"`
	CostUnit     json.RawMessage   `json:"cost_unit"`
	Usage        *TokenUsage       `json:"usage"`
	Error        *ProviderAPIError `json:"error"`
	Choices      []struct {
		Index        int         `json:"index"`
		FinishReason string      `json:"finish_reason"`
		Message      wireMessage `json:"message"`
		Delta        wireMessage `json:"delta"`
	} `json:"choices"`
}

func decodeResponse(reader io.Reader) (ModelResponse, error) {
	var result wireResponse
	if err := json.NewDecoder(reader).Decode(&result); err != nil {
		return ModelResponse{}, err
	}
	out := ModelResponse{Usage: result.Usage}
	out.observeMetadata(result)
	if len(result.Choices) == 0 {
		if result.Error != nil {
			return out, result.Error
		}
		return out, fmt.Errorf("provider returned no choices")
	}
	choice := result.Choices[0]
	m := choice.Message
	out.Content, out.Thinking, out.StopReason = m.Content, m.Thinking+m.Reasoning, choice.FinishReason
	for _, call := range m.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: call.Function.Arguments})
	}
	if result.Error != nil {
		return out, result.Error
	}
	if choice.FinishReason == "" {
		return out, fmt.Errorf("provider JSON response ended without a finish reason")
	}
	return out, nil
}
func (p *OpenAIProvider) Complete(ctx context.Context, req ModelRequest) (out ModelResponse, callErr error) {
	started := time.Now()
	diagnostic := &ModelDiagnostics{}
	defer func() {
		diagnostic.ResponseMillis = elapsedMillis(started)
		if hasSemanticResponse(out) {
			diagnostic.FirstSemanticDeltaMillis = diagnostic.ResponseMillis
		}
		out.Diagnostics = diagnostic
	}()
	response, err := p.request(ctx, req, false, started, diagnostic)
	if err != nil {
		return providerErrorResponse(err), err
	}
	defer response.Body.Close()
	return decodeResponse(firstByteReader{Reader: response.Body, start: started, diagnostic: diagnostic})
}
func (p *OpenAIProvider) CompleteStream(ctx context.Context, req ModelRequest, emit func(StreamDelta) error) (out ModelResponse, callErr error) {
	started := time.Now()
	diagnostic := &ModelDiagnostics{}
	defer func() {
		diagnostic.ResponseMillis = elapsedMillis(started)
		if diagnostic.FirstSemanticDeltaMillis == nil && hasSemanticResponse(out) {
			diagnostic.FirstSemanticDeltaMillis = diagnostic.ResponseMillis
		}
		out.Diagnostics = diagnostic
	}()
	response, err := p.request(ctx, req, true, started, diagnostic)
	if err != nil {
		return providerErrorResponse(err), err
	}
	defer response.Body.Close()
	// Some compatible providers ignore stream=true. Preserve their JSON completion.
	reader := bufio.NewReader(firstByteReader{Reader: response.Body, start: started, diagnostic: diagnostic})
	first, err := reader.Peek(1)
	if err != nil {
		return ModelResponse{}, err
	}
	if first[0] == '{' || strings.Contains(response.Header.Get("Content-Type"), "application/json") {
		return decodeResponse(reader)
	}
	var result ModelResponse
	calls := map[int]*ToolCall{}
	done := false
	finished := false
	emitDelta := func(d StreamDelta) error {
		if semanticDelta(d) && diagnostic.FirstSemanticDeltaMillis == nil {
			diagnostic.FirstSemanticDeltaMillis = elapsedMillis(started)
		}
		if emit != nil {
			sinkStarted := time.Now()
			err := emit(d)
			if diagnostic.StreamSinkWaitMillis == nil {
				diagnostic.StreamSinkWaitMillis = new(int64)
			}
			*diagnostic.StreamSinkWaitMillis += time.Since(sinkStarted).Milliseconds()
			if err != nil {
				return &providerStreamSinkError{cause: err}
			}
		}
		return nil
	}
	process := func(data string) error {
		if data == "[DONE]" {
			done = true
			return nil
		}
		var chunk wireResponse
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			if strings.Contains(err.Error(), "unexpected end of JSON input") {
				return &providerIncompleteError{detail: "incomplete provider stream JSON: " + err.Error()}
			}
			return fmt.Errorf("decode stream: %w", err)
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
		result.observeMetadata(chunk)
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.FinishReason != "" {
				result.StopReason = choice.FinishReason
				finished = true
			}
			delta := choice.Delta
			if delta.Content != "" {
				result.Content += delta.Content
				if err := emitDelta(StreamDelta{Kind: "text", Text: delta.Content}); err != nil {
					return err
				}
			}
			thinking := delta.Thinking + delta.Reasoning
			if thinking != "" {
				result.Thinking += thinking
				if err := emitDelta(StreamDelta{Kind: "thinking", Text: thinking}); err != nil {
					return err
				}
			}
			for _, part := range delta.ToolCalls {
				call := calls[part.Index]
				if call == nil {
					call = &ToolCall{}
					calls[part.Index] = call
				}
				call.ID += part.ID
				call.Name += part.Function.Name
				call.Arguments += part.Function.Arguments
				if err := emitDelta(StreamDelta{Kind: "tool_call", Index: part.Index, ID: part.ID, Name: part.Function.Name, Arguments: part.Function.Arguments}); err != nil {
					return err
				}
			}
		}
		if chunk.Error != nil {
			return chunk.Error
		}
		return nil
	}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	var data []string
	var streamErr error
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if len(data) > 0 {
				streamErr = process(strings.Join(data, "\n"))
				data = nil
				if streamErr != nil || done {
					break
				}
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if streamErr == nil && len(data) > 0 {
		streamErr = process(strings.Join(data, "\n"))
	}
	indexes := make([]int, 0, len(calls))
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	for _, i := range indexes {
		result.ToolCalls = append(result.ToolCalls, *calls[i])
	}
	if streamErr != nil {
		return result, streamErr
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	if !done || !finished {
		return result, &providerIncompleteError{detail: "incomplete provider stream: missing finish reason or DONE marker"}
	}
	return result, nil
}

func (a *Agent) complete(ctx context.Context, id string, request ModelRequest) (out ModelResponse, callErr error) {
	started := time.Now()
	var hydrated, manifestStored *int64
	defer func() {
		if out.Diagnostics == nil {
			out.Diagnostics = &ModelDiagnostics{}
		}
		out.Diagnostics.PrepareMillis, out.Diagnostics.ArchiveMillis = request.prepareMillis, request.archiveMillis
		out.Diagnostics.HydrateMillis, out.Diagnostics.ManifestStoreMillis = hydrated, manifestStored
	}()
	var err error
	request, err = a.hydrateInputRequest(request)
	hydrated = elapsedMillis(started)
	if err != nil {
		return ModelResponse{}, err
	}
	manifestStart := time.Now()
	_, err = a.store.Update(id, func(s *Snapshot) error {
		if isStopped(s.Run.Status) {
			return errPiHalt
		}
		request.ContextManifest = buildContextManifest(s, request, a.registry.Definitions())
		for i := range s.Requests {
			if s.Requests[i].ID == request.ID {
				s.Requests[i].ContextManifest = request.ContextManifest
				return nil
			}
		}
		return fmt.Errorf("model request not archived: %s", request.ID)
	})
	manifestStored = elapsedMillis(manifestStart)
	if err != nil {
		return ModelResponse{}, err
	}
	if provider, ok := a.provider.(StreamingProvider); ok {
		streamCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		var streamStoreMillis int64
		emit, drain := modelStreamBatch(streamCtx, cancel, func(deltas []StreamDelta) error {
			storeStarted := time.Now()
			defer func() { streamStoreMillis += time.Since(storeStarted).Milliseconds() }()
			events := make([]Event, 0, len(deltas))
			for _, delta := range deltas {
				events = append(events, Event{Type: "model_stream", Summary: "模型流式增量", Data: map[string]any{"request_id": request.ID, "delta": delta}})
			}
			if err := a.store.AppendEvents(id, events); err != nil {
				return err
			}
			state, err := a.store.Observe(id)
			if err == nil && isStopped(state.Run.Status) {
				cancel()
			}
			return err
		})
		response, callErr := provider.CompleteStream(streamCtx, request, emit)
		// Tool dispatch and response settlement happen only after accepted deltas
		// have drained, including provider errors and operator cancellation.
		drainStarted := time.Now()
		sinkErr := drain()
		if response.Diagnostics == nil {
			response.Diagnostics = &ModelDiagnostics{}
		}
		response.Diagnostics.StreamDrainMillis = elapsedMillis(drainStarted)
		// drain joins the sole batch writer before this process-local measurement
		// is read. It overlaps network time and must not be added to response_ms.
		response.Diagnostics.StreamStoreMillis = &streamStoreMillis
		if sinkErr != nil {
			return response, errors.Join(callErr, &providerStreamSinkError{cause: sinkErr})
		}
		return response, callErr
	}
	return a.provider.Complete(ctx, request)
}
