package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type SearchProbeResult struct {
	Source            string    `json:"source"`
	Status            string    `json:"status"`
	Auth              string    `json:"auth"`
	Quota             string    `json:"quota"`
	Rate              string    `json:"rate"`
	CheckedAt         time.Time `json:"checked_at"`
	RetryAfterSeconds int       `json:"retry_after_seconds,omitempty"`
	AvailableCredits  *int64    `json:"available_credits,omitempty"`
}

// Explicit settings action only. Account-info endpoints never submit a search
// and their private account fields are neither persisted nor sent to the model.
func (a *Agent) ProbeSearchSource(ctx context.Context, source string) (SearchProbeResult, error) {
	out := SearchProbeResult{Source: source, Status: "unavailable", Auth: "unknown", Quota: "unknown", Rate: "unknown", CheckedAt: time.Now().UTC()}
	if _, ok := searchSourceURLs[source]; !ok {
		return out, fmt.Errorf("unknown search source")
	}
	if a.searchSources == nil {
		return out, nil
	}
	c, ok := a.searchSources.configs[source]
	if !ok || !c.Enabled || c.APIKey == "" {
		return out, nil
	}
	gate := a.searchSources.gates[source]
	status, retry, release := gate.acquire(c)
	if release == nil {
		out.Status, out.Rate, out.RetryAfterSeconds = status, status, retry
		return out, nil
	}
	defer release()
	params := url.Values{}
	path := ""
	switch source {
	case "fofa":
		path = "/api/v1/info/my"
		params.Set("key", c.APIKey)
		if c.Email != "" {
			params.Set("email", c.Email)
		}
	case "quake":
		path = "/api/v3/user/info"
	case "shodan":
		path = "/api-info"
		params.Set("key", c.APIKey)
	}
	endpoint := strings.TrimSuffix(c.BaseURL, "/") + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return out, fmt.Errorf("invalid source account request")
	}
	if source == "quake" {
		request.Header.Set("X-QuakeToken", c.APIKey)
	}
	response, err := a.searchSources.client.Do(request)
	if err != nil {
		out.Status = "outcome_unknown"
		return out, nil
	}
	defer response.Body.Close()
	if response.StatusCode == 401 || response.StatusCode == 403 {
		out.Status, out.Auth = "auth_error", "rejected"
		return out, nil
	}
	if response.StatusCode == 429 {
		out.Status, out.Rate, out.RetryAfterSeconds = "rate_limited", "limited", gate.recordRateLimit(response.Header.Get("Retry-After"))
		return out, nil
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		out.Status = "upstream_error"
		return out, nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(raw) > 65536 {
		out.Status = "outcome_unknown"
		return out, nil
	}
	var doc map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&doc) != nil || doc == nil {
		out.Status = "invalid_response"
		return out, nil
	}
	if status := sourceBusinessError(source, doc); status != "" {
		out.Status = status
		if status == "auth_error" {
			out.Auth = "rejected"
		}
		if status == "quota_exhausted" {
			out.Quota = "exhausted"
		}
		return out, nil
	}
	valid := false
	switch source {
	case "fofa":
		valid = doc["email"] != nil || doc["username"] != nil
	case "quake":
		account, ok := doc["data"].(map[string]any)
		valid = ok && len(account) > 0 && sourceString(doc["code"]) == "0"
	case "shodan":
		_, valid = doc["query_credits"].(json.Number)
	}
	if !valid {
		out.Status = "invalid_response"
		return out, nil
	}
	out.Status, out.Auth, out.Rate = "authenticated", "accepted", "accepted"
	if credits, ok := doc["query_credits"].(json.Number); ok {
		if n, err := credits.Int64(); err == nil && n >= 0 {
			out.AvailableCredits = &n
			out.Quota = "reported"
		}
	}
	return out, nil
}
