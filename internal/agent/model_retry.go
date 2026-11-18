package agent

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Reference contracts: pi agent-session.ts _prepareRetry (8a7b0c03, MIT),
// DeepSeek Harness core/agent-loop/src/agent.ts and llm/llm-retry/src/index.ts
// (57190202, MIT): archive each failed attempt before a cancellable delay.
// Independent adaptation: retries re-enter Trace's existing request/budget
// loop; no tool action is replayed and no provider-local attempt is hidden.
func modelRetryDelay(err error, attempt int) time.Duration {
	var failure *ProviderHTTPError
	if attempt < 1 || classifyProviderFailure(err) != providerTransient {
		return 0
	}
	delay := time.Duration(min(attempt, 30)) * time.Second
	if !errors.As(err, &failure) {
		return delay
	}
	// Respect the server's delay instead of immediately repeating a rate-limited
	// request. The existing Run time budget and controls own this wait's lifetime.
	value := strings.TrimSpace(failure.RetryAfter)
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
		delay = max(delay, time.Duration(seconds)*time.Second)
	} else if until, err := http.ParseTime(value); err == nil {
		delay = max(delay, time.Until(until))
	}
	return delay
}
