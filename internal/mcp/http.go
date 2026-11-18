package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type httpTransport struct {
	url, session, protocol string
	headers                http.Header
	client                 *http.Client
}

func newHTTP(config Config) (*httpTransport, error) {
	u, err := url.Parse(config.URL)
	if err != nil {
		return nil, err
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("MCP URL must be absolute HTTP or HTTPS")
	}
	client := config.HTTPClient
	if client == nil {
		client = &http.Client{}
	}
	headers := http.Header{}
	for name, value := range config.Headers {
		headers.Set(name, value)
	}
	return &httpTransport{url: config.URL, client: client, headers: headers, protocol: ProtocolVersion}, nil
}

func (t *httpTransport) setProtocol(version string) { t.protocol = version }

func (t *httpTransport) request(ctx context.Context, method string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, t.url, body)
	if err != nil {
		return nil, err
	}
	req.Header = t.headers.Clone()
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", t.protocol)
	if t.session != "" {
		req.Header.Set("Mcp-Session-Id", t.session)
	}
	return req, nil
}

func (t *httpTransport) roundTrip(ctx context.Context, rpc request) (json.RawMessage, error) {
	body, err := json.Marshal(rpc)
	if err != nil {
		return nil, err
	}
	if len(body) > MaxMessageBytes {
		return nil, errors.New("MCP request exceeds frame limit")
	}
	req, err := t.request(ctx, http.MethodPost, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	// Cancellation is a separate notification; aborting HTTP alone does not
	// tell the provider to settle its resources. No tool call is replayed.
	stop := context.AfterFunc(ctx, func() {
		if rpc.ID == 0 {
			return
		}
		notifyCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		data, _ := json.Marshal(request{JSONRPC: "2.0", Method: "notifications/cancelled", Params: map[string]any{"requestId": rpc.ID, "reason": "caller cancelled"}})
		// The callback may outlive initialize/setProtocol. Reuse this request's
		// immutable headers instead of reading mutable transport session state.
		notification, err := http.NewRequestWithContext(notifyCtx, http.MethodPost, req.URL.String(), bytes.NewReader(data))
		if err == nil {
			notification.Header = req.Header.Clone()
			if response, err := t.client.Do(notification); err == nil {
				response.Body.Close()
			}
		}
	})
	defer stop()
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &HTTPError{StatusCode: resp.StatusCode}
	}
	if rpc.Method == "initialize" {
		t.session = resp.Header.Get("Mcp-Session-Id")
	}
	if rpc.ID == 0 {
		return nil, nil
	}
	media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil {
		return nil, fmt.Errorf("MCP response content type: %w", err)
	}
	switch media {
	case "application/json":
		raw, err := io.ReadAll(io.LimitReader(resp.Body, MaxMessageBytes+1))
		if err != nil {
			return nil, err
		}
		if len(raw) > MaxMessageBytes {
			return nil, errors.New("MCP response exceeds frame limit")
		}
		result, err := decodeResponse(raw, rpc.ID)
		if result == nil && err == nil {
			err = errors.New("MCP JSON body is not a response")
		}
		return result, err
	case "text/event-stream":
		return readSSEProgress(resp.Body, rpc.ID, func(raw []byte) { observeProgress(ctx, raw, rpc.ID) })
	default:
		return nil, fmt.Errorf("unsupported MCP response content type %q", media)
	}
}

// readSSE joins data fields with newlines, not concatenation; malformed events fail.
func readSSE(reader io.Reader, id int64) (json.RawMessage, error) {
	return readSSEProgress(reader, id, nil)
}

func readSSEProgress(reader io.Reader, id int64, observe func([]byte)) (json.RawMessage, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), MaxMessageBytes+1)
	var data strings.Builder
	first := true
	flush := func() (json.RawMessage, error) {
		if data.Len() == 0 {
			return nil, nil
		}
		payload := strings.TrimSuffix(data.String(), "\n")
		data.Reset()
		if payload == "" {
			return nil, nil
		}
		if observe != nil {
			observe([]byte(payload))
		}
		return decodeResponse([]byte(payload), id)
	}
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if line == "" {
			result, err := flush()
			if result != nil || err != nil {
				return result, err
			}
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		if field != "data" {
			continue
		}
		value = strings.TrimPrefix(value, " ")
		if data.Len()+len(value)+1 > MaxMessageBytes {
			return nil, errors.New("MCP SSE event exceeds frame limit")
		}
		data.WriteString(value)
		data.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("MCP SSE stream: %w", err)
	}
	// The final event must end with a blank line, as specified by SSE framing.
	return nil, errors.New("MCP SSE stream ended without a matching complete response")
}

func (t *httpTransport) close(ctx context.Context) error {
	if t.session == "" {
		return nil
	}
	req, err := t.request(ctx, http.MethodDelete, nil)
	if err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &HTTPError{StatusCode: resp.StatusCode}
	}
	return nil
}
