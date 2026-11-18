package mcp

import (
	"context"
	"encoding/json"
)

// Progress is a server observation, not proof that a tool or target succeeded.
type Progress struct {
	ProgressToken any      `json:"progressToken"`
	Progress      float64  `json:"progress"`
	Total         *float64 `json:"total,omitempty"`
	Message       string   `json:"message,omitempty"`
}

type progressKey struct{}

func (c *Client) CallWithProgress(ctx context.Context, name string, arguments json.RawMessage, onProgress func(Progress)) (Result, error) {
	return c.Call(context.WithValue(ctx, progressKey{}, onProgress), name, arguments)
}

func (c *Client) CallMethodWithProgress(ctx context.Context, method string, arguments json.RawMessage, onProgress func(Progress)) (Result, error) {
	return c.CallMethod(context.WithValue(ctx, progressKey{}, onProgress), method, arguments)
}

func observeProgress(ctx context.Context, raw []byte, id int64) {
	callback, _ := ctx.Value(progressKey{}).(func(Progress))
	if callback == nil {
		return
	}
	var frame struct {
		JSONRPC string   `json:"jsonrpc"`
		Method  string   `json:"method"`
		Params  Progress `json:"params"`
	}
	if json.Unmarshal(raw, &frame) != nil || frame.JSONRPC != "2.0" || frame.Method != "notifications/progress" {
		return
	}
	token, ok := frame.Params.ProgressToken.(float64)
	if !ok || token != float64(id) {
		return
	}
	callback(frame.Params)
}
