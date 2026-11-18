package web

import (
	"os"
	"testing"
)

// Entry timing reuses the real Store/HTTP/layout fixture. Its five worker
// records test presentation, not worker execution or the original B9 gate.
func TestSessionEntryBrowserAcceptance(t *testing.T) {
	if os.Getenv("TRACE_ENTRY_BROWSER") != "1" {
		t.Skip("server-only session-entry timing and navigation acceptance")
	}
	t.Setenv("TRACE_P0_BROWSER", "1")
	TestP0BrowserAcceptance(t)
}
