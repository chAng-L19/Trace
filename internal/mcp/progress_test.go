package mcp

import (
	"context"
	"strings"
	"testing"
)

func TestStdioProgressAndNonzeroShutdown(t *testing.T) {
	c := stdioClient(t, "progress")
	var seen []Progress
	result, err := c.CallWithProgress(context.Background(), "one", nil, func(p Progress) { seen = append(seen, p) })
	if err != nil || result.Text() != "{}" || len(seen) != 1 || seen[0].Message != "observed" || seen[0].Progress != 1 {
		t.Fatalf("progress=%+v result=%+v error=%v", seen, result, err)
	}
	bad := stdioClient(t, "exit-on-close")
	if err := bad.Close(); err == nil || !strings.Contains(err.Error(), "exit status 19") {
		t.Fatalf("nonzero server shutdown was hidden: %v", err)
	}
}
