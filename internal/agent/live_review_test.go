package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Opt-in server test: deterministic intent creation, real filesystem execution,
// one independent request to the configured model, and actual fresh retests.
// This proves a fixture observation, never a vulnerability or exploit impact.
func TestLiveProviderEvidenceReviewAndRetest(t *testing.T) {
	configPath := os.Getenv("TRACE_LIVE_CONFIG")
	if configPath == "" {
		t.Skip("set TRACE_LIVE_CONFIG on the test server")
	}
	var config struct {
		Provider struct {
			URL    string `json:"url"`
			Model  string `json:"model"`
			APIKey string `json:"api_key"`
		} `json:"provider"`
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &config); err != nil {
		t.Fatal("invalid live provider configuration JSON")
	}
	if config.Provider.APIKey == "" || config.Provider.Model == "" {
		t.Fatal("live provider configuration missing")
	}
	root := os.Getenv("TRACE_LIVE_OUTPUT")
	if root == "" {
		root = t.TempDir()
	}
	root = filepath.Join(root, "live-review")
	if err = os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	// Preserve earlier acceptance runs and never reuse their fixture/state.
	root, err = os.MkdirTemp(root, "run-")
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(root, "observation.json")
	writeFixture := func(value int) {
		t.Helper()
		b, e := json.Marshal(map[string]any{"fixture": "live-review", "value": value})
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(fixturePath, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	writeFixture(37)
	store, err := NewStore(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	live := &OpenAIProvider{BaseURL: config.Provider.URL, Model: config.Provider.Model, APIKey: config.Provider.APIKey}
	a, err := New(store, live, "")
	if err != nil {
		t.Fatal(err)
	}
	s, err := a.Create("Record the local fixture observation: observation.json has fixture=live-review and integer value=37. This is an observation test, not a vulnerability claim.")
	if err != nil {
		t.Fatal(err)
	}
	id := s.Run.ID
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	started := time.Now()
	t.Logf("live review run=%s model=%s root=%s", id, config.Provider.Model, root)
	args, _ := json.Marshal(map[string]string{"path": fixturePath, "operation": "read_json"})
	command := []string{"python3", "-c", "import json,sys; print(json.dumps(json.load(open(sys.argv[1], encoding='utf-8')), sort_keys=True))", fixturePath}
	observe := func(callID string) (Snapshot, string) {
		t.Helper()
		// Only intent generation is scripted; the command is executed by the
		// existing process adapter and the semantic reviewer is the real model.
		a.provider = manualProvider(ToolCall{ID: callID, Name: "live_fixture_read", Arguments: string(args)})
		waiting, e := a.Run(ctx, id)
		a.provider = live
		if e != nil {
			t.Fatal(e)
		}
		if waiting.Run.Status != StatusWaitingTool {
			t.Fatalf("intent not persisted as pending: %s", waiting.Run.Status)
		}
		observed, e := a.executeCommand(ctx, id, callID, command)
		if e != nil {
			t.Fatal(e)
		}
		for _, ev := range observed.Evidence {
			if ev.CallID == callID {
				if verifyEvidence(&observed, &ev) != "verified" {
					t.Fatal("fixture execution failed provenance validation")
				}
				return observed, ev.ID
			}
		}
		t.Fatal("fixture execution evidence missing")
		return Snapshot{}, ""
	}
	s, original := observe("initial-read")
	output, ok := verifiedOutput(&s, original)
	if !ok {
		t.Fatal("initial stdout unavailable")
	}
	var observed struct {
		Fixture string `json:"fixture"`
		Value   int    `json:"value"`
	}
	if json.Unmarshal([]byte(output), &observed) != nil || observed.Fixture != "live-review" || observed.Value != 37 {
		t.Fatal("actual initial JSON observation differs from fixture")
	}
	f := Finding{ID: "fixture-observation", Title: "Fixture read stdout reports integer value 37", Severity: "info", RootCause: "This observation follows directly from the parsed stdout JSON fields. No program defect or unobserved internal mechanism is alleged.", Impact: "The observed stdout JSON reports fixture=live-review and value=37. This demonstrates only the fixture read output, not a vulnerability or any broader security impact.", Remediation: "No security remediation follows from this observation alone. For an experimental negative control, change the fixture value and rerun the same read.", ReproductionSteps: []string{"Write observation.json with fixture=live-review and value=37.", "Run the recorded Python JSON-reading command against that file and inspect its output."}, EvidenceIDs: []string{original}, Predicate: EvidencePredicate{Kind: "json_equals", Pointer: "/value", Expected: 37}}
	if _, err = a.SubmitFinding(id, f); err != nil {
		t.Fatal(err)
	}
	s, err = a.ReviewFinding(ctx, id, f.ID)
	if err != nil {
		t.Fatalf("real provider review failed (%T); no provider error body is printed", err)
	}
	if s.Findings[0].Status != "reviewed" || s.Findings[0].Review == nil || s.Findings[0].Review.Status != "pass" {
		t.Fatal("real provider did not pass the exact fixture observation")
	}
	reviewID := s.Findings[0].Review.RequestID
	if _, err = a.RetestFinding(id, f.ID, []string{original}); err == nil {
		t.Fatal("negative control: original evidence counted as fresh execution")
	}
	// An identical command on changed real bytes must fail the predicate.
	writeFixture(38)
	_, negative := observe("changed-file-read")
	s, err = a.RetestFinding(id, f.ID, []string{negative})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Findings[0].Retests[len(s.Findings[0].Retests)-1].Status; got != "fail" || s.Findings[0].Status == "reproduced" {
		t.Fatal("negative control: value 38 was accepted as value 37")
	}
	writeFixture(37)
	_, fresh := observe("fresh-matching-read")
	s, err = a.RetestFinding(id, f.ID, []string{fresh})
	if err != nil {
		t.Fatal(err)
	}
	if s.Findings[0].Status != "reproduced" || s.Findings[0].Retests[len(s.Findings[0].Retests)-1].Status != "pass" {
		t.Fatal("fresh identical execution did not reproduce the observation")
	}
	writeSnapshot := func(name string, snapshot Snapshot) {
		t.Helper()
		b, e := json.MarshalIndent(snapshot, "", "  ")
		if e != nil {
			t.Fatal(e)
		}
		if e = os.WriteFile(filepath.Join(root, name), b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	writeSnapshot("reproduced.json", s)
	// Change only the archived reviewer input: the real execution remains
	// verified, but the prior assessment must no longer qualify as current.
	s, err = store.Update(id, func(s *Snapshot) error {
		for i := range s.Requests {
			if s.Requests[i].ID != reviewID {
				continue
			}
			var in reviewInput
			if e := json.Unmarshal([]byte(s.Requests[i].Messages[1].Content), &in); e != nil {
				return e
			}
			in.Outputs = []string{`{"fixture":"live-review","value":999}`}
			b, e := json.Marshal(in)
			if e != nil {
				return e
			}
			s.Requests[i].Messages[1].Content = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Findings[0].Status != "submitted" {
		t.Fatal("negative control: edited review output did not revoke the assessment")
	}
	if _, valid := verifiedOutput(&s, original); !valid {
		t.Fatal("review-input edit unexpectedly altered actual execution evidence")
	}
	writeSnapshot("stale-review.json", s)
	// Independent second control: changing source content without its ledger
	// counterparts revokes source verification and the derived finding.
	s, err = store.Update(id, func(s *Snapshot) error {
		for i := range s.Evidence {
			if s.Evidence[i].ID == original {
				s.Evidence[i].Content += " altered"
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if s.Findings[0].Status != "invalid" || s.Findings[0].SourceStatus != "invalid" {
		t.Fatal("negative control: altered source output retained a valid finding")
	}
	writeSnapshot("invalid-source.json", s)
	t.Logf("live review accepted and fresh retest passed; controls passed: old evidence rejected, changed file failed, edited review stale, altered source invalid; review_request=%s elapsed=%s", reviewID, time.Since(started))
}
