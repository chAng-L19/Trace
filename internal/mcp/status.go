package mcp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"time"
)

// Status contains observed protocol facts, never server messages or configuration.
// Configured/enabled is deliberately a separate concern from connection health.
type Status struct {
	State           string    `json:"state"`
	Stage           string    `json:"stage,omitempty"`
	Code            string    `json:"code,omitempty"`
	Detail          string    `json:"detail,omitempty"`
	ProtocolVersion string    `json:"protocol_version,omitempty"`
	Capabilities    []string  `json:"capabilities"`
	InterfaceCount  int       `json:"interface_count"`
	CheckedAt       time.Time `json:"checked_at"`
	Cleanup         []Status  `json:"cleanup,omitempty"`
}

// WithCleanup retains the primary observation and appends only sanitized cleanup failures.
func (s Status) WithCleanup(stage string, err error) Status {
	if err != nil {
		s.Cleanup = append(s.Cleanup, Failure(stage, err))
	}
	return s
}

type ConnectionError struct {
	Name   string
	Status Status
	Err    error
}

func (e *ConnectionError) Error() string { return e.Err.Error() }
func (e *ConnectionError) Unwrap() error { return e.Err }

type HTTPError struct{ StatusCode int }

func (e *HTTPError) Error() string { return fmt.Sprintf("MCP HTTP status %d", e.StatusCode) }

// Failure uses fixed descriptions: remote errors can echo headers, URLs or env.
func Failure(stage string, err error) Status {
	s := Status{State: "failed", Stage: stage, Code: "protocol", Detail: "MCP 协议响应异常", CheckedAt: time.Now().UTC()}
	var rpc *RPCError
	var response *HTTPError
	var network net.Error
	var request *url.Error
	switch {
	case errors.Is(err, context.Canceled):
		s.State, s.Code, s.Detail = "cancelled", "cancelled", "连接检测已取消"
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout():
		s.Code, s.Detail = "timeout", "MCP 连接请求超时"
	case errors.As(err, &rpc):
		s.Code, s.Detail = "rpc", fmt.Sprintf("MCP 返回协议错误（%d）", rpc.Code)
	case errors.As(err, &response):
		s.Code, s.Detail = "http", fmt.Sprintf("MCP 返回 HTTP %d", response.StatusCode)
	case errors.As(err, &request):
		s.Code, s.Detail = "network", "MCP 网络连接失败"
	case stage == "connect":
		s.Code, s.Detail = "connection", "MCP 启动或连接失败，请检查连接配置和本地依赖"
	}
	return s
}

func (c *Client) Name() string { return c.name }
func (c *Client) Status() Status {
	c.statusMu.RLock()
	defer c.statusMu.RUnlock()
	s := c.status
	s.Capabilities = append([]string{}, s.Capabilities...)
	return s
}

func (c *Client) observe(stage string, err error) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	if err != nil {
		s := Failure(stage, err)
		s.ProtocolVersion, s.Capabilities, s.InterfaceCount = c.status.ProtocolVersion, c.status.Capabilities, c.status.InterfaceCount
		c.status = s
	} else {
		c.status.State, c.status.Stage, c.status.Code, c.status.Detail = "connected", stage, "", "协议连接已响应；接口执行能力以实际调用为准"
		c.status.CheckedAt = time.Now().UTC()
	}
}
