// Package mcp implements MCP discovery and calls over stdio and Streamable HTTP.
package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const ProtocolVersion = "2025-06-18"
const MaxMessageBytes = 16 << 20

type Config struct {
	GitHubURL   string            `json:"github_url,omitempty"`
	LocalPath   string            `json:"local_path,omitempty"`
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Disabled    bool              `json:"disabled,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Dir         string            `json:"dir,omitempty"`
	HTTPClient  *http.Client      `json:"-"`
}

type Tool struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Method      string          `json:"method,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// ToolID is stable, model-compatible, and unambiguous even when names contain separators.
func ToolID(server, tool string) string {
	pair, _ := json.Marshal([2]string{server, tool})
	digest := sha256.Sum256(pair)
	return "mcp_" + hex.EncodeToString(digest[:28])
}

type Result struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
	Raw               json.RawMessage   `json:"-"`
}

// Text extracts text blocks without discarding non-text content from Raw.
func (r Result) Text() string {
	var blocks []string
	for _, raw := range r.Content {
		var block struct{ Type, Text string }
		if json.Unmarshal(raw, &block) == nil && block.Type == "text" {
			blocks = append(blocks, block.Text)
		}
	}
	return strings.Join(blocks, "\n")
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("MCP RPC %d: %s", e.Code, e.Message) }

type request struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type transport interface {
	roundTrip(context.Context, request) (json.RawMessage, error)
	setProtocol(string)
	close(context.Context) error
}

type Client struct {
	name         string
	capabilities map[string]json.RawMessage
	transport    transport
	gate         chan struct{}
	lifetime     context.Context
	cancel       context.CancelFunc
	nextID       int64
	closeOnce    sync.Once
	closeErr     error
	statusMu     sync.RWMutex
	status       Status
}

func Dial(ctx context.Context, config Config) (*Client, error) {
	if strings.TrimSpace(config.Name) == "" {
		return nil, errors.New("MCP server name is required")
	}
	if (config.URL == "") == (config.Command == "") {
		return nil, errors.New("configure exactly one MCP URL or command")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var t transport
	var err error
	if config.URL != "" {
		t, err = newHTTP(config)
	} else {
		t, err = newStdio(config)
	}
	if err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	c := &Client{name: config.Name, transport: t, gate: make(chan struct{}, 1), lifetime: lifetime, cancel: cancel}
	raw, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": ProtocolVersion, "capabilities": map[string]any{},
		"clientInfo": map[string]string{"name": "trace", "version": "0.1"},
	})
	if err == nil {
		var result struct {
			ProtocolVersion string                     `json:"protocolVersion"`
			Capabilities    map[string]json.RawMessage `json:"capabilities"`
		}
		err = json.Unmarshal(raw, &result)
		if err == nil && result.ProtocolVersion != ProtocolVersion && result.ProtocolVersion != "2025-03-26" {
			err = fmt.Errorf("unsupported MCP protocol version %q", result.ProtocolVersion)
		}
		if err == nil {
			c.capabilities = result.Capabilities
			c.status.ProtocolVersion = result.ProtocolVersion
			for _, name := range []string{"tools", "resources", "prompts", "logging", "completions"} {
				if c.supports(name) {
					c.status.Capabilities = append(c.status.Capabilities, name)
				}
			}
			t.setProtocol(result.ProtocolVersion)
			_, err = c.exchange(ctx, "notifications/initialized", nil, false)
		}
	}
	if err != nil {
		status := Failure("initialize", err)
		status.ProtocolVersion, status.Capabilities = c.status.ProtocolVersion, c.status.Capabilities
		closeErr := c.Close()
		return nil, &ConnectionError{Name: config.Name, Status: status.WithCleanup("cleanup", closeErr), Err: errors.Join(err, closeErr)}
	}
	return c, nil
}

func (c *Client) Tools(ctx context.Context) ([]Tool, error) {
	tools := []Tool{}
	cursor := ""
	seenCursors, seenTools := map[string]bool{}, map[string]bool{}
	for {
		params := map[string]string{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, err
		}
		if page.Tools == nil {
			return nil, errors.New("MCP tools/list omitted tools array")
		}
		for _, tool := range page.Tools {
			if tool.Name == "" || seenTools[tool.Name] {
				return nil, fmt.Errorf("invalid or duplicate MCP tool name %q", tool.Name)
			}
			// Shape check only: RawMessage values keep numbers such as 1e400 unparsed.
			var schema map[string]json.RawMessage
			if json.Unmarshal(tool.InputSchema, &schema) != nil || schema == nil {
				return nil, fmt.Errorf("MCP tool %q has no object inputSchema", tool.Name)
			}
			seenTools[tool.Name] = true
			// Routing metadata belongs to Trace, never to a tools/list response.
			tool.Method = ""
			tool.ID = ToolID(c.name, tool.Name)
			tools = append(tools, tool)
		}
		if page.NextCursor == "" {
			return tools, nil
		}
		if seenCursors[page.NextCursor] {
			return nil, errors.New("MCP tools/list repeated pagination cursor")
		}
		seenCursors[page.NextCursor] = true
		cursor = page.NextCursor
	}
}

// Call returns remote tool errors in Result.IsError; transport and RPC errors use error.
// It never retries a call: interruption leaves external side effects uncertain.
func (c *Client) Call(ctx context.Context, name string, arguments json.RawMessage) (Result, error) {
	if arguments == nil {
		arguments = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(arguments, &object) != nil || object == nil {
		return Result{}, errors.New("MCP tool arguments must be a JSON object")
	}
	raw, err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments})
	if err != nil {
		return Result{Raw: append(json.RawMessage(nil), raw...)}, err
	}
	var result Result
	if err := json.Unmarshal(raw, &result); err != nil {
		return Result{Raw: append(json.RawMessage(nil), raw...)}, err
	}
	if result.Content == nil {
		return Result{Raw: append(json.RawMessage(nil), raw...)}, errors.New("MCP tools/call omitted content array")
	}
	result.Raw = append(json.RawMessage(nil), raw...)
	return result, nil
}

func (c *Client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	return c.exchange(ctx, method, params, true)
}

func (c *Client) exchange(ctx context.Context, method string, params any, response bool) (json.RawMessage, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.lifetime, cancel)
	defer func() { stop(); cancel() }()
	// ponytail: one in-flight request per connection; add multiplexing only if measured.
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if c.lifetime.Err() != nil {
		return nil, errors.New("MCP client is closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := request{JSONRPC: "2.0", Method: method, Params: params}
	if response {
		c.nextID++
		req.ID = c.nextID
	}
	_, _, bridgeMethod := methodResult(method)
	if (method == "tools/call" || bridgeMethod) && ctx.Value(progressKey{}) != nil {
		original := params.(map[string]any)
		copyParams := make(map[string]any, len(original)+1)
		for key, value := range original {
			copyParams[key] = value
		}
		copyParams["_meta"] = map[string]any{"progressToken": req.ID}
		req.Params = copyParams
	}
	raw, err := c.transport.roundTrip(ctx, req)
	c.observe(method, err)
	return raw, err
}

func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		c.gate <- struct{}{}
		defer func() { <-c.gate }()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c.closeErr = c.transport.close(ctx)
		if c.closeErr != nil {
			c.observe("cleanup", c.closeErr)
			return
		}
		c.statusMu.Lock()
		c.status.State = "closed"
		c.statusMu.Unlock()
	})
	return c.closeErr
}

// decodeResponse returns nil, nil for notifications, never for an unrelated response.
func decodeResponse(data []byte, id int64) (json.RawMessage, error) {
	var msg struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Result  json.RawMessage `json:"result"`
		Error   *RPCError       `json:"error"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, fmt.Errorf("MCP JSON frame: %w", err)
	}
	if msg.JSONRPC != "2.0" {
		return nil, errors.New("MCP frame has invalid jsonrpc version")
	}
	if len(msg.ID) == 0 && msg.Method != "" {
		return nil, nil
	}
	if msg.Method != "" {
		return nil, fmt.Errorf("unsupported MCP server request %q", msg.Method)
	}
	if string(msg.ID) != fmt.Sprint(id) {
		return nil, fmt.Errorf("MCP response ID mismatch: got %s want %d", msg.ID, id)
	}
	if (len(msg.Result) == 0) == (msg.Error == nil) {
		return nil, errors.New("MCP response requires exactly one result or error")
	}
	if msg.Error != nil {
		// Preserve the original error envelope, including data and extensions.
		return append(json.RawMessage(nil), data...), msg.Error
	}
	return msg.Result, nil
}
