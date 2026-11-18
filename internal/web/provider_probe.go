package web

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"trace/internal/agent"
)

type providerProbeResult struct {
	Status        string    `json:"status"`
	Code          string    `json:"code,omitempty"`
	Detail        string    `json:"detail"`
	ElapsedMillis int64     `json:"elapsed_ms"`
	CheckedAt     time.Time `json:"checked_at"`
}

// Only this explicit action calls a provider. It uses a temporary provider,
// never an Agent Run, configuration rebuild, or persisted readiness assertion.
func (s *Server) probeProvider(w http.ResponseWriter, r *http.Request) {
	var input struct {
		ProviderConfig
		Provider string `json:"provider,omitempty"`
	}
	if !decode(w, r, &input) {
		return
	}
	config := input.ProviderConfig
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		writeJSON(w, 503, providerProbeResult{Status: "failed", Code: "closed", Detail: "服务正在停止"})
		return
	}
	if config.APIKey == hiddenSecret {
		stored := s.config.Provider
		if input.Provider != "" && input.Provider != "default" {
			var exists bool
			stored, exists = s.config.Providers[input.Provider]
			if !exists {
				s.mu.Unlock()
				writeJSON(w, 200, providerProbeResult{Status: "failed", Code: "configuration", Detail: "该模型配置已变化，请重新读取配置或填写新的 API Key", CheckedAt: time.Now().UTC()})
				return
			}
		}
		config.APIKey = stored.APIKey
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	ctx, cancel := context.WithCancel(r.Context())
	stop := context.AfterFunc(s.ctx, cancel)
	defer func() { stop(); cancel() }()
	started := time.Now()
	result := providerProbeResult{Status: "failed"}
	defer func() {
		result.ElapsedMillis = time.Since(started).Milliseconds()
		result.CheckedAt = time.Now().UTC()
		writeJSON(w, 200, result)
	}()
	if config.URL == "" {
		result.Status, result.Code, result.Detail = "mock", "mock", "Mock 模式：未发送网络请求"
		return
	}
	parsed, err := url.Parse(config.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || strings.TrimSpace(config.Model) == "" {
		result.Code, result.Detail = "configuration", "请填写 HTTP(S) API 地址和模型名称"
		return
	}
	p := &agent.OpenAIProvider{BaseURL: config.URL, Model: config.Model, APIKey: config.APIKey}
	response, err := p.Complete(ctx, agent.ModelRequest{MaxOutputTokens: 64, Messages: []agent.Message{{Role: "user", Content: "Reply with OK."}}})
	if err != nil {
		result.Code, result.Detail = probeError(err)
		return
	}
	validFinish := response.StopReason == "" || response.StopReason == "stop" || response.StopReason == "length"
	if len(response.ToolCalls) > 0 || !validFinish || (strings.TrimSpace(response.Content) == "" && strings.TrimSpace(response.Thinking) == "" && response.StopReason != "length") {
		result.Code, result.Detail = "response", "API 响应没有有效文本或推理内容，未确认模型就绪"
		return
	}
	result.Status, result.Detail = "success", "模型 API 已返回有效响应；尚未执行任务"
	if strings.TrimSpace(response.Content) == "" || response.StopReason == "length" {
		result.Status, result.Code, result.Detail = "responded", "incomplete_reply", "API 已响应，但短检测请求未形成完整答案（可能达到输出上限）；任务执行能力尚未确认"
	}
}

func probeError(err error) (string, string) {
	if errors.Is(err, context.Canceled) {
		return "cancelled", "检测已取消"
	}
	var network net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &network) && network.Timeout() {
		return "timeout", "连通性检测超时，请检查服务后重新检测"
	}
	var response *agent.ProviderHTTPError
	if errors.As(err, &response) {
		switch response.StatusCode {
		case 401, 403:
			return "authentication", "API 拒绝凭据或访问权限"
		case 404:
			return "not_found", "API 端点或模型未找到"
		case 429:
			return "rate_limit", "API 限流或额度不足"
		default:
			return "http", "API 返回非成功 HTTP 状态"
		}
	}
	var request *url.Error
	if errors.As(err, &request) {
		return "network", "网络连接失败，请检查 API 地址与网络"
	}
	return "response", "API 响应格式异常或返回错误，未确认模型就绪"
}
