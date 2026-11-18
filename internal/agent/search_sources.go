package agent

// Independent Go adapters following the public FOFA/Quake/Shodan HTTP contracts.
// Muteki connectors.py/connector_manifest.py (3b8fd7abe4ee) informed pagination
// and uncertain-result handling; execution and CAS remain Trace's own ledger.
import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type SearchSourceConfig struct {
	Enabled        bool   `json:"enabled"`
	APIKey         string `json:"api_key,omitempty"`
	Email          string `json:"email,omitempty"`
	BaseURL        string `json:"base_url,omitempty"`
	MaxConcurrency int    `json:"max_concurrency,omitempty"`
	MinIntervalMS  int    `json:"min_interval_ms,omitempty"`
	MaxPages       int    `json:"max_pages,omitempty"`
	MaxRequests    int    `json:"max_requests,omitempty"`
	MaxResults     int    `json:"max_results,omitempty"`
}

type searchSourceRuntime struct {
	configs map[string]SearchSourceConfig
	gates   map[string]*searchSourceGate
	client  *http.Client
}

type searchSourceGate struct {
	mu           sync.Mutex
	active       int
	next         time.Time
	blockedUntil time.Time
}

var searchSourceURLs = map[string]string{"fofa": "https://fofa.info", "quake": "https://quake.360.net", "shodan": "https://api.shodan.io"}

func (a *Agent) ConfigureSearchSources(configs map[string]SearchSourceConfig) error {
	runtime := &searchSourceRuntime{configs: map[string]SearchSourceConfig{}, gates: map[string]*searchSourceGate{}, client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	for name, config := range configs {
		base, ok := searchSourceURLs[name]
		if !ok {
			return fmt.Errorf("unknown search source %q", name)
		}
		if config.BaseURL == "" {
			config.BaseURL = base
		}
		u, err := url.Parse(config.BaseURL)
		if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1"))) {
			return fmt.Errorf("search source %s requires an HTTPS base_url without credentials/query (HTTP loopback supported)", name)
		}
		if config.MaxConcurrency == 0 {
			config.MaxConcurrency = 1
		}
		if config.MinIntervalMS == 0 {
			config.MinIntervalMS = 1000
		}
		if config.MaxPages == 0 {
			config.MaxPages = 5
		}
		if config.MaxRequests == 0 {
			config.MaxRequests = 20
		}
		if config.MaxResults == 0 {
			config.MaxResults = 1000
		}
		if config.MaxRequests < 1 || config.MaxRequests > 10000 || config.MaxResults < 1 || config.MaxResults > 1000000 {
			return fmt.Errorf("search source %s has invalid request/result limits", name)
		}
		if config.MaxConcurrency < 1 || config.MaxConcurrency > 5 || config.MinIntervalMS < 0 || config.MinIntervalMS > 60000 || config.MaxPages < 1 || config.MaxPages > 100 {
			return fmt.Errorf("search source %s has invalid concurrency, interval or page bounds", name)
		}
		if config.APIKey == "[stored secret]" || config.Email == "[stored secret]" {
			return fmt.Errorf("search source %s secret placeholder has no stored value", name)
		}
		if config.Enabled && strings.TrimSpace(config.APIKey) == "" {
			return fmt.Errorf("search source %s requires api_key", name)
		}
		runtime.configs[name] = config
		// Runtime pointer is shared by all role/provider/child Agent copies.
		runtime.gates[name] = &searchSourceGate{}
	}
	a.searchSources = runtime
	return nil
}

type SearchSourceStatus struct {
	Source            string `json:"source"`
	Enabled           bool   `json:"enabled"`
	Configured        bool   `json:"configured"`
	MaxConcurrency    int    `json:"max_concurrency"`
	MinIntervalMS     int    `json:"min_interval_ms"`
	MaxPages          int    `json:"max_pages"`
	MaxRequests       int    `json:"max_requests"`
	MaxResults        int    `json:"max_results"`
	Active            int    `json:"active"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

func (a *Agent) SearchSourceStatus() []SearchSourceStatus {
	rows := []SearchSourceStatus{}
	for _, name := range []string{"fofa", "quake", "shodan"} {
		row := SearchSourceStatus{Source: name}
		if a.searchSources != nil {
			c := a.searchSources.configs[name]
			row.Enabled, row.Configured = c.Enabled, c.APIKey != ""
			row.MaxConcurrency, row.MinIntervalMS, row.MaxPages = c.MaxConcurrency, c.MinIntervalMS, c.MaxPages
			row.MaxRequests, row.MaxResults = c.MaxRequests, c.MaxResults
			if gate := a.searchSources.gates[name]; gate != nil {
				gate.mu.Lock()
				row.Active = gate.active
				if wait := time.Until(gate.blockedUntil); wait > 0 {
					row.RetryAfterSeconds = int(wait.Seconds()) + 1
				}
				gate.mu.Unlock()
			}
		}
		rows = append(rows, row)
	}
	return rows
}

type SourceSearchRequest struct {
	Source   string `json:"source"`
	Query    string `json:"query"`
	Page     int    `json:"page,omitempty"`
	PageSize int    `json:"page_size,omitempty"`
}

func (r *SourceSearchRequest) normalize() error {
	if _, ok := searchSourceURLs[r.Source]; !ok {
		return fmt.Errorf("source must be fofa, quake or shodan")
	}
	if strings.TrimSpace(r.Query) == "" || len(r.Query) > 4096 {
		return fmt.Errorf("query must contain 1..4096 bytes")
	}
	if r.Page == 0 {
		r.Page = 1
	}
	if r.PageSize == 0 {
		r.PageSize = 100
	}
	if r.Page < 1 || r.Page > 100 || r.PageSize < 1 || r.PageSize > 100 {
		return fmt.Errorf("page and page_size must be within 1..100")
	}
	if r.Source == "shodan" && r.PageSize != 100 {
		return fmt.Errorf("Shodan uses fixed page_size=100")
	}
	return nil
}

type SourceSearchItem struct {
	ID         string   `json:"id"`
	Source     string   `json:"source"`
	IP         string   `json:"ip,omitempty"`
	Host       string   `json:"host,omitempty"`
	Port       string   `json:"port,omitempty"`
	Protocol   string   `json:"protocol,omitempty"`
	Title      string   `json:"title,omitempty"`
	Domains    []string `json:"domains,omitempty"`
	ObservedAt string   `json:"observed_at,omitempty"`
	Status     string   `json:"status"`
}

type SourceSearchResult struct {
	SourceSearchRequest
	Status            string             `json:"status"`
	Partial           bool               `json:"partial"`
	RetrievedAt       time.Time          `json:"retrieved_at"`
	Items             []SourceSearchItem `json:"items"`
	Total             *int64             `json:"total,omitempty"`
	NextPage          int                `json:"next_page,omitempty"`
	LimitReached      bool               `json:"limit_reached,omitempty"`
	Raw               *ArtifactRef       `json:"raw,omitempty"`
	Auth              string             `json:"auth"`
	Quota             string             `json:"quota"`
	Rate              string             `json:"rate"`
	RetryAfterSeconds int                `json:"retry_after_seconds,omitempty"`
	HTTPStatus        int                `json:"http_status,omitempty"`
	Message           string             `json:"message,omitempty"`
}

func (g *searchSourceGate) acquire(c SearchSourceConfig) (string, int, func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	until := g.next
	if g.blockedUntil.After(until) {
		until = g.blockedUntil
	}
	if now.Before(until) {
		return "rate_limited", int(until.Sub(now).Seconds()) + 1, nil
	}
	if g.active >= c.MaxConcurrency {
		return "busy", 1, nil
	}
	g.active++
	g.next = now.Add(time.Duration(c.MinIntervalMS) * time.Millisecond)
	return "", 0, func() { g.mu.Lock(); g.active--; g.mu.Unlock() }
}

// executeAction persists started/uncertain before/after this function. No
// automatic retry (including redirects) can spend a second upstream credit.
func (a *Agent) sourceSearch(ctx context.Context, runID string, call ToolCall) (SourceSearchResult, error) {
	var req SourceSearchRequest
	if err := decodeBuiltin(call.Arguments, &req); err != nil {
		return SourceSearchResult{}, err
	}
	if err := req.normalize(); err != nil {
		return SourceSearchResult{}, err
	}
	out := SourceSearchResult{SourceSearchRequest: req, RetrievedAt: time.Now().UTC(), Items: []SourceSearchItem{}, Status: "unavailable", Auth: "unknown", Quota: "unknown", Rate: "unknown"}
	if a.searchSources == nil {
		return out, nil
	}
	c, exists := a.searchSources.configs[req.Source]
	if !exists || !c.Enabled || c.APIKey == "" {
		return out, nil
	}
	if req.Page > c.MaxPages {
		out.Status, out.LimitReached = "page_limit", true
		return out, nil
	}
	if runID != "" {
		s, err := a.store.Load(runID)
		if err != nil {
			return out, err
		}
		for _, action := range s.Actions {
			if action.Name != "source_search" || action.CallID == call.ID && actionRequestMatchesCurrent(&s, action) {
				continue
			}
			if action.Status != "uncertain" && action.Status != "started" {
				continue
			}
			var previous SourceSearchRequest
			if json.Unmarshal([]byte(action.Arguments), &previous) == nil && previous.normalize() == nil && previous == req {
				out.Status, out.Partial, out.Message = "outcome_unknown", true, "An earlier matching request has no settled receipt; reconcile it before another acquisition."
				return out, nil
			}
		}
	}
	gate := a.searchSources.gates[req.Source]
	status, retry, release := gate.acquire(c)
	if release == nil {
		out.Status, out.Rate, out.RetryAfterSeconds = status, status, retry
		return out, nil
	}
	defer release()
	admitted, err := a.reserveSearchUsage(runID, req, c)
	if err != nil {
		return out, err
	}
	if !admitted {
		out.Status, out.Quota = "quota_exhausted", "local_limit"
		return out, nil
	}
	request, err := sourceHTTPRequest(ctx, c, req)
	if err != nil {
		return out, err
	}
	response, err := a.searchSources.client.Do(request)
	if err != nil {
		out.Status, out.Partial, out.Message = "outcome_unknown", true, "Upstream transport ended without a complete receipt; no retry was sent."
		return out, nil
	}
	defer response.Body.Close()
	out.HTTPStatus = response.StatusCode
	const responseLimit = 4 << 20
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if len(raw) > 0 {
		ref, err := a.store.PutArtifact("search_response", raw)
		if err != nil {
			out.Status, out.Partial, out.Message = "outcome_unknown", true, "Received response could not be archived; reconcile before repeating."
			return out, nil
		}
		ref.MIMEType, ref.Name = "application/json", req.Source+"-response.json"
		ref.Purpose = "Audit-only raw receipt; model access uses normalized items and status."
		out.Raw = &ref
	}
	if readErr != nil || len(raw) > responseLimit {
		out.Status, out.Partial = "outcome_unknown", true
		out.Message = "Incomplete or oversized response; archived bytes are a partial receipt."
		return out, nil
	}
	if response.StatusCode == 401 || response.StatusCode == 403 {
		out.Status, out.Auth = "auth_error", "rejected"
		return out, nil
	}
	if response.StatusCode == 429 {
		out.Status, out.Rate, out.RetryAfterSeconds = "rate_limited", "limited", retryAfter(response.Header.Get("Retry-After"))
		gate.mu.Lock()
		gate.blockedUntil = time.Now().Add(time.Duration(out.RetryAfterSeconds) * time.Second)
		gate.mu.Unlock()
		return out, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		out.Status = "upstream_error"
		return out, nil
	}
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&document) != nil || document == nil {
		out.Status, out.Partial = "invalid_response", true
		return out, nil
	}
	if status := sourceBusinessError(req.Source, document); status != "" {
		out.Status = status
		if status == "auth_error" {
			out.Auth = "rejected"
		}
		if status == "quota_exhausted" {
			out.Quota = "exhausted"
		}
		if status == "rate_limited" {
			out.Rate = "limited"
			out.RetryAfterSeconds = 60
			gate.mu.Lock()
			gate.blockedUntil = time.Now().Add(time.Minute)
			gate.mu.Unlock()
		}
		return out, nil
	}
	out.Auth, out.Rate, out.Status = "accepted", "accepted", "completed"
	decodeSourceItems(&out, document)
	for i := range out.Items {
		row := &out.Items[i]
		for _, value := range []*string{&row.IP, &row.Host, &row.Port, &row.Protocol, &row.Title, &row.ObservedAt} {
			*value = redactSourceValue(*value, c)
		}
		for j := range row.Domains {
			row.Domains[j] = redactSourceValue(row.Domains[j], c)
		}
	}
	if out.NextPage > c.MaxPages {
		out.LimitReached = true
	}
	return out, nil
}

func sourceHTTPRequest(ctx context.Context, c SearchSourceConfig, r SourceSearchRequest) (*http.Request, error) {
	base := strings.TrimSuffix(c.BaseURL, "/")
	params := url.Values{}
	path, method, body := "", "GET", ""
	switch r.Source {
	case "fofa":
		path = "/api/v1/search/all"
		params.Set("key", c.APIKey)
		if c.Email != "" {
			params.Set("email", c.Email)
		}
		params.Set("qbase64", base64.StdEncoding.EncodeToString([]byte(r.Query)))
		params.Set("page", strconv.Itoa(r.Page))
		params.Set("size", strconv.Itoa(r.PageSize))
		params.Set("fields", "host,ip,port,protocol,title,domain")
	case "quake":
		path, method = "/api/v3/search/quake_service", "POST"
		data, _ := json.Marshal(map[string]any{"query": r.Query, "start": (r.Page - 1) * r.PageSize, "size": r.PageSize})
		body = string(data)
	case "shodan":
		path = "/shodan/host/search"
		params.Set("key", c.APIKey)
		params.Set("query", r.Query)
		params.Set("page", strconv.Itoa(r.Page))
	}
	u := base + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, u, strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("invalid search request")
	}
	request.Header.Set("Accept", "application/json")
	if r.Source == "quake" {
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-QuakeToken", c.APIKey)
	}
	return request, nil
}

func retryAfter(value string) int {
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return min(seconds, 86400)
	}
	if when, err := http.ParseTime(value); err == nil && when.After(time.Now()) {
		return min(int(time.Until(when).Seconds())+1, 86400)
	}
	return 60
}

func redactSourceValue(value string, config SearchSourceConfig) string {
	for _, secret := range []string{config.APIKey, config.Email} {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	if u, err := url.Parse(value); err == nil && u.Host != "" {
		u.User, u.RawQuery, u.Fragment = nil, "", ""
		value = u.String()
	}
	return excerpt(value, 512)
}

func sourceBusinessError(source string, doc map[string]any) string {
	failure := false
	switch source {
	case "fofa":
		failure, _ = doc["error"].(bool)
	case "quake":
		failure = sourceString(doc["code"]) != "0" && doc["code"] != nil
	case "shodan":
		failure = doc["error"] != nil
	}
	if !failure {
		return ""
	}
	message := strings.ToLower(sourceString(doc["errmsg"]) + " " + sourceString(doc["message"]) + " " + sourceString(doc["error"]))
	for _, item := range []struct {
		status string
		terms  []string
	}{{"quota_exhausted", []string{"quota", "credit", "balance", "积分", "余额", "配额"}}, {"rate_limited", []string{"rate limit", "too many", "频率", "频繁"}}, {"auth_error", []string{"unauthorized", "invalid key", "api key", "token", "认证", "权限"}}} {
		for _, term := range item.terms {
			if strings.Contains(message, term) {
				return item.status
			}
		}
	}
	return "query_error"
}

func sourceString(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	if n, ok := value.(json.Number); ok {
		return n.String()
	}
	return ""
}

func sourceDomains(value any) []string {
	result := []string{}
	if values, ok := value.([]any); ok {
		for _, entry := range values {
			if s := sourceString(entry); s != "" {
				result = append(result, s)
			}
		}
	} else if s := sourceString(value); s != "" {
		result = append(result, s)
	}
	sort.Strings(result)
	return result
}

func decodeSourceItems(out *SourceSearchResult, doc map[string]any) {
	var rows []any
	var total any
	switch out.Source {
	case "fofa":
		rows, _ = doc["results"].([]any)
		total = doc["size"]
	case "quake":
		rows, _ = doc["data"].([]any)
		if meta, ok := doc["meta"].(map[string]any); ok {
			if pagination, ok := meta["pagination"].(map[string]any); ok {
				total = pagination["total"]
			}
		}
	case "shodan":
		rows, _ = doc["matches"].([]any)
		total = doc["total"]
	}
	if n, err := strconv.ParseInt(sourceString(total), 10, 64); err == nil && n >= 0 {
		out.Total = &n
	}
	if rows == nil {
		out.Status, out.Partial = "invalid_response", true
		return
	}
	if len(rows) > out.PageSize {
		out.Partial = true
	}
	for _, value := range rows[:min(len(rows), out.PageSize)] {
		item := SourceSearchItem{ID: out.Source + ":" + valueHash(value), Source: out.Source, Status: "candidate_not_verified"}
		if out.Source == "fofa" {
			row, ok := value.([]any)
			if !ok || len(row) < 6 {
				out.Partial = true
				continue
			}
			item.Host, item.IP, item.Port, item.Protocol, item.Title = sourceString(row[0]), sourceString(row[1]), sourceString(row[2]), sourceString(row[3]), sourceString(row[4])
			item.Domains = sourceDomains(row[5])
		} else {
			row, ok := value.(map[string]any)
			if !ok {
				out.Partial = true
				continue
			}
			item.IP, item.Port, item.Protocol = sourceString(row["ip"]), sourceString(row["port"]), sourceString(row["transport"])
			item.Domains, item.ObservedAt = sourceDomains(row["domain"]), sourceString(row["time"])
			if out.Source == "shodan" {
				item.IP, item.Domains, item.ObservedAt = sourceString(row["ip_str"]), sourceDomains(row["hostnames"]), sourceString(row["timestamp"])
			}
			if service, ok := row["service"].(map[string]any); ok {
				if httpData, ok := service["http"].(map[string]any); ok {
					item.Title, item.Host = sourceString(httpData["title"]), sourceString(httpData["host"])
				}
			}
			if httpData, ok := row["http"].(map[string]any); ok {
				item.Title = sourceString(httpData["title"])
			}
		}
		out.Items = append(out.Items, item)
	}
	if len(rows) > 0 && (out.Total == nil && len(rows) >= out.PageSize || out.Total != nil && int64(out.Page*out.PageSize) < *out.Total) {
		out.NextPage, out.Partial = out.Page+1, true
	}
	if out.Total == nil {
		out.Partial = true
	}
	if out.Partial {
		out.Status = "partial"
	}
}
