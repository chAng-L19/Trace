package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageReportDeduplicatesWorkersKeepsUnknownAndPinnedPrices(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(store, &OpenAIProvider{BaseURL: "https://example.invalid/v1", Model: "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	config := CostConfig{Currency: "USD", PoolLimit: "0.000003", Rates: []ModelPrice{{Model: "fixture", InputPerMillion: "0.1", OutputPerMillion: "0.2", Currency: "USD", Source: "fixture contract", EffectiveAt: "2026-10-23"}}}
	if err := a.ConfigureCosts(config); err != nil {
		t.Fatal(err)
	}
	root, _ := a.Create("root")
	child, _ := a.Create("child")
	branch, _ := a.Create("branch")
	one, two := int64(1), int64(2)
	for _, id := range []string{root.Run.ID, child.Run.ID, branch.Run.ID} {
		_, err = store.Update(id, func(s *Snapshot) error {
			if id == child.Run.ID {
				s.Run.ParentRunID = root.Run.ID
				s.Run.ParentTaskID = "task"
			}
			if id == branch.Run.ID {
				s.Run.ParentRunID = root.Run.ID
				s.Run.ParentMessageID = "message"
			}
			req := ModelRequest{ID: "request"}
			a.stampRequest(&req)
			s.Requests = append(s.Requests, req)
			s.Responses = append(s.Responses, ResponseRecord{RequestID: "request", Response: ModelResponse{Usage: &TokenUsage{InputTokens: &one, OutputTokens: &two}}})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(filepath.Join(store.root, root.Run.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	config.Rates[0].InputPerMillion = "999"
	report, err := store.UsageReport(root.Run.ID, config)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.RunIDs) != 2 || report.Usage.ModelRequests != 2 || report.Usage.ModelResponses != 2 || report.Usage.KnownInputTokens == nil || *report.Usage.KnownInputTokens != 2 || report.MissingTotalResponses != 2 {
		t.Fatalf("incorrect tree usage: %+v", report)
	}
	if report.Cost.KnownAmount == nil || *report.Cost.KnownAmount != "0.000001000000000000" || !report.Cost.Complete || report.Pool.Remaining == nil {
		t.Fatalf("quote repriced or cost wrong: %+v", report.Cost)
	}
	_, err = store.Update(child.Run.ID, func(s *Snapshot) error { s.Responses[0].Response.Usage = nil; return nil })
	if err != nil {
		t.Fatal(err)
	}
	report, err = store.UsageReport(root.Run.ID, config)
	if err != nil {
		t.Fatal(err)
	}
	if report.Cost.Complete || report.Pool.Remaining != nil || report.Cost.UnpricedResponses != 1 {
		t.Fatalf("missing usage became zero: %+v", report)
	}
	after, err := os.ReadFile(filepath.Join(store.root, root.Run.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("usage projection rewrote source")
	}
}

func TestUsageProjectionIgnoresBodiesChecksIdentityAndSeesAtomicReplacement(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Create("usage projection")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.root, snap.Run.ID+".json")
	// Deliberately incompatible body types prove this projection never decodes
	// messages, tools, response content or action result as execution structures.
	raw := `{"run":{"id":"` + snap.Run.ID + `"},"messages":{"ignored":true},"requests":[{"id":"r","messages":"ignored","tools":17}],"responses":[{"request_id":"r","response":{"content":{"ignored":true},"usage":{"prompt_tokens":7}}}],"actions":[{"result":"ignored","stdout":"` + strings.Repeat("x", 1<<20) + `"}]}`
	if err = os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	projected, err := store.readUsageSnapshot(snap.Run.ID)
	if err != nil || len(projected.Requests) != 1 || len(projected.Responses) != 1 || len(projected.Actions) != 1 {
		t.Fatalf("projection: %+v %v", projected, err)
	}
	report, err := store.UsageReport(snap.Run.ID, CostConfig{})
	if err != nil || report.Usage.ToolActions != 1 || report.Usage.KnownInputTokens == nil || *report.Usage.KnownInputTokens != 7 || report.Cost.Complete || report.Cost.KnownAmount != nil {
		t.Fatalf("unknown projection: %+v %v", report, err)
	}
	replacement := strings.Replace(raw, `"prompt_tokens":7`, `"prompt_tokens":9`, 1)
	if _, err = store.persistSnapshotLocked(snap.Run.ID, []byte(replacement)); err != nil {
		t.Fatal(err)
	}
	report, err = store.UsageReport(snap.Run.ID, CostConfig{})
	if err != nil || report.Usage.KnownInputTokens == nil || *report.Usage.KnownInputTokens != 9 {
		t.Fatalf("stale generation: %+v %v", report, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != replacement {
		t.Fatal("usage read rewrote snapshot")
	}
	for _, invalid := range []string{strings.Replace(replacement, snap.Run.ID, "wrong-id", 1), replacement + ` {}`, `{"run":{}}`} {
		if err = os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = store.readUsageSnapshot(snap.Run.ID); err == nil {
			t.Fatal("invalid snapshot identity or trailing data accepted")
		}
	}
	if _, err = store.readUsageSnapshot("../outside"); err == nil {
		t.Fatal("path traversal accepted")
	}
}

func TestUsageReportRejectsAmbiguousLedgerAndLeavesInflightCostUnknown(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(store, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	snap, err := a.Create("usage identities")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.root, snap.Run.ID+".json")
	write := func(requests []ModelRequest, responses []ResponseRecord) {
		t.Helper()
		raw, err := json.Marshal(Snapshot{Run: snap.Run, Requests: requests, Responses: responses})
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write([]ModelRequest{{ID: "r"}}, nil)
	report, err := store.UsageReport(snap.Run.ID, CostConfig{Currency: "USD", PoolLimit: "1"})
	if err != nil || report.Cost.Complete || report.Cost.KnownAmount != nil || report.Pool.Remaining != nil {
		t.Fatalf("in-flight cost became zero: %+v %v", report, err)
	}
	for _, ledger := range []struct {
		requests  []ModelRequest
		responses []ResponseRecord
	}{
		{[]ModelRequest{{ID: "r"}, {ID: "r"}}, nil},
		{[]ModelRequest{{ID: "r"}}, []ResponseRecord{{RequestID: "r"}, {RequestID: "r"}}},
		{[]ModelRequest{{ID: "r"}}, []ResponseRecord{{RequestID: "orphan"}}},
	} {
		write(ledger.requests, ledger.responses)
		if _, err = store.UsageReport(snap.Run.ID, CostConfig{}); err == nil {
			t.Fatal("ambiguous ledger counted")
		}
	}
}

func TestUsagePriceMatchesProviderAndModelWithSpecificRatePrecedence(t *testing.T) {
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a, err := New(store, &OpenAIProvider{BaseURL: "https://user:secret@example.invalid/v1?token=secret", Model: "fixture"}, "")
	if err != nil {
		t.Fatal(err)
	}
	probe := ModelRequest{ID: "r"}
	a.stampRequest(&probe)
	wildcard := ModelPrice{Model: "fixture", Currency: "USD", InputPerMillion: "1", OutputPerMillion: "2", Source: "fixture", EffectiveAt: "2026-10-23"}
	specific := wildcard
	specific.ProviderID = probe.ProviderID
	specific.InputPerMillion = "3"
	for _, rates := range [][]ModelPrice{{wildcard, specific}, {specific, wildcard}} {
		if err = a.ConfigureCosts(CostConfig{Currency: "USD", Rates: rates}); err != nil {
			t.Fatal(err)
		}
		request := ModelRequest{ID: "r"}
		a.stampRequest(&request)
		if request.Price == nil || request.Price.InputPerMillion != "3" || strings.Contains(request.ProviderID, "secret") {
			t.Fatalf("provider rate selection: %+v", request)
		}
	}
	snap, err := a.Create("quote identity")
	if err != nil {
		t.Fatal(err)
	}
	one := int64(1)
	for _, wrong := range []ModelPrice{func() ModelPrice { p := specific; p.Model = "other"; return p }(), func() ModelPrice { p := specific; p.ProviderID = "other"; return p }()} {
		_, err = store.Update(snap.Run.ID, func(s *Snapshot) error {
			req := ModelRequest{ID: "r"}
			a.stampRequest(&req)
			req.Price = &wrong
			s.Requests = []ModelRequest{req}
			s.Responses = []ResponseRecord{{RequestID: "r", Response: ModelResponse{Usage: &TokenUsage{InputTokens: &one, OutputTokens: &one, TotalTokens: &one}}}}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		report, err := store.UsageReport(snap.Run.ID, CostConfig{Currency: "USD"})
		if err != nil || report.Cost.Complete || report.Cost.UnpricedResponses != 1 || report.Cost.KnownAmount != nil {
			t.Fatalf("mismatched quote billed: %+v %v", report, err)
		}
	}
}

func TestCostConfigRejectsInvalidDecimalCurrencyAndSource(t *testing.T) {
	for _, value := range []string{"-1", "NaN", "1/2", "1e9", "", " 1"} {
		if _, err := decimal(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if err := ValidateCostConfig(CostConfig{PoolLimit: "1"}); err == nil {
		t.Fatal("missing currency accepted")
	}
	if err := ValidateCostConfig(CostConfig{Currency: "USD", Rates: []ModelPrice{{Model: "fixture", Currency: "USD", InputPerMillion: "1", OutputPerMillion: "2"}}}); err == nil {
		t.Fatal("missing price source accepted")
	}
}

func (s *Store) persistSnapshotLocked(id string, parts ...[]byte) (os.FileInfo, error) {
	staged, err := s.stageSnapshotLocked(id, parts...)
	if err != nil {
		return nil, err
	}
	defer os.Remove(staged.path)
	s.eventViewMu.Lock()
	defer s.eventViewMu.Unlock()
	return s.publishSnapshotLocked(id, staged)
}
