package web

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"trace/internal/agent"
)

// Another Store has no access to this process's commit notification channels.
// The polling fallback must still deliver its durable cursor exactly once.
func TestSSECommitWakeKeepsCrossStorePolling(t *testing.T) {
	root := t.TempDir()
	store, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	a, err := agent.New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	app, err := New(a, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	server := httptest.NewServer(app.Handler())
	defer server.Close()
	snap, err := a.Create("cross-store SSE fixture")
	if err != nil {
		t.Fatal(err)
	}
	other, err := agent.NewStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/runs/"+snap.Run.ID+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Last-Event-ID", fmt.Sprint(snap.Run.EventSeq))
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal(response.Status)
	}
	if err = other.AppendEvents(snap.Run.ID, []agent.Event{{Type: "external"}}); err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		if line := scanner.Text(); strings.HasPrefix(line, "id: ") {
			if line != fmt.Sprintf("id: %d", snap.Run.EventSeq+1) {
				t.Fatalf("replayed/skipped cursor: %s", line)
			}
			return
		}
	}
	t.Fatalf("cross-store commit not delivered: %v", scanner.Err())
}
