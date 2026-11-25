package web

import (
	"os"
	"os/exec"
	"testing"
)

// Server-only: existing ARTEX-derived crossing/merge/cycle fixtures plus real
// pointer hits on parallel relations in both modes and >560px outer scrolling.
func TestExplorationGraphInteractions(t *testing.T) {
	if os.Getenv("TRACE_P0_BROWSER") != "1" {
		t.Skip("server-only: TRACE_P0_BROWSER=1 and fixed headed CloakBrowser required")
	}
	node := testNodeBinary()
	raw, err := exec.Command(node, "exploration_interaction_test.mjs").CombinedOutput()
	t.Logf("EXPLORATION_LAYOUT %s", raw)
	if err != nil {
		t.Fatal(err)
	}
	runP0Browser(t, "exploration_browser.mjs", "exploration")
}
