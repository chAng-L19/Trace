package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Real pi continuation with the original Store on both persistence backends.
// Returning the first requested digest reproduces the live model's choice;
// selecting only the newest prompt would conceal a post-callback digest change.
func TestU8PiReportDigestFrozenBeforeTranslation(t *testing.T) {
	for _, backend := range []string{"json", "sqlite"} {
		for _, change := range []string{"unchanged", "evidence", "manual_chain"} {
			t.Run(backend+"/"+change, func(t *testing.T) {
				store, _ := u7Store(t, backend)
				path := filepath.Join(t.TempDir(), "observation.txt")
				if err := os.WriteFile(path, []byte("u8-delivery-proof"), 0600); err != nil {
					t.Fatal(err)
				}
				var runID, frozen string
				calls := 0
				provider := settlementProviderFunc(func(_ context.Context, request ModelRequest) (ModelResponse, error) {
					calls++
					switch calls {
					case 1:
						args, _ := json.Marshal(map[string]string{"path": path})
						return ModelResponse{ToolCalls: []ToolCall{{ID: "read-proof", Name: "read", Arguments: string(args)}}}, nil
					case 2:
						return ModelResponse{Content: "文件已经返回预期标记，本次观察保留固定来源，清理仍待独立核对。"}, nil
					case 3:
						if len(request.Tools) != 0 {
							return ModelResponse{}, fmt.Errorf("translation exposed execution tools")
						}
						const prefix = "Pending source-bound documents: "
						prompts := 0
						for _, message := range request.Messages {
							at := strings.Index(message.Content, prefix)
							if at < 0 {
								continue
							}
							prompts++
							var docs []struct {
								Digest string       `json:"source_digest"`
								Source reportSource `json:"source"`
							}
							if err := json.Unmarshal([]byte(message.Content[at+len(prefix):]), &docs); err != nil {
								return ModelResponse{}, err
							}
							if len(docs) != 1 || docs[0].Source.Kind != "final" || len(docs[0].Source.Chains) != 1 {
								return ModelResponse{}, fmt.Errorf("first delivery source omitted its automatic chain")
							}
							if frozen == "" {
								frozen = docs[0].Digest
							}
						}
						if prompts != 1 || frozen == "" {
							return ModelResponse{}, fmt.Errorf("translation carried %d source generations", prompts)
						}
						s, err := store.Load(runID)
						if err != nil {
							return ModelResponse{}, err
						}
						candidates := reportCandidates(&s)
						if len(candidates) != 1 || candidates[0].digest() != frozen {
							return ModelResponse{}, fmt.Errorf("archiving changed the issued translation digest")
						}
						if change == "evidence" {
							_, err = store.Update(runID, func(s *Snapshot) error {
								s.Evidence[0].Content += " changed after request"
								return nil
							})
						} else if change == "manual_chain" {
							_, err = store.CreateEvidenceChain(runID, EvidenceChain{
								Name: "Operator source organization", Summary: "New manual source organization after the translation request.",
								Steps: []EvidenceChainStep{{RunID: runID, EvidenceID: s.Evidence[0].ID}},
							})
						}
						if err != nil {
							return ModelResponse{}, err
						}
						body := u7Text
						body.SourceDigest = frozen
						raw, _ := json.Marshal(map[string]any{"trace_report": body})
						return ModelResponse{Content: string(raw)}, nil
					default:
						return ModelResponse{}, fmt.Errorf("unexpected execution or translation retry %d", calls)
					}
				})
				a, err := New(store, provider, "")
				if err != nil {
					t.Fatal(err)
				}
				a.SetSuccessContains("u8-delivery-proof")
				if change != "unchanged" {
					three := 3
					a.ConfigureBudget(BudgetOverride{MaxModelRequests: &three})
				}
				// Exercise execution/report delivery explicitly as a Worker.
				run, err := a.CreateConfigured("Read a local observation and deliver its bilingual report", RunConfiguration{role: "worker"})
				if err != nil {
					t.Fatal(err)
				}
				runID = run.Run.ID
				s, err := a.Run(context.Background(), runID)
				if err != nil || calls != 3 || len(s.Requests) != 3 || len(s.Actions) != 1 || s.Actions[0].CallID != "read-proof" || s.Actions[0].Status != "completed" || len(pendingToolCalls(&s)) != 0 {
					t.Fatalf("translation replayed execution: calls=%d requests=%d actions=%d status=%s err=%v", calls, len(s.Requests), len(s.Actions), s.Run.Status, err)
				}
				candidates := reportCandidates(&s)
				if len(candidates) != 1 {
					t.Fatalf("unexpected final candidates: %d", len(candidates))
				}
				if change == "unchanged" {
					if s.Run.Status != StatusCompleted || s.Result.Status != "success" || reportDeliveryPending(&s) || candidates[0].digest() != frozen {
						t.Fatalf("stable delivery failed: %+v result=%s", s.Run, s.Result.Status)
					}
				} else {
					if s.Run.Status != StatusPaused || s.Run.PauseReason != "model request budget exhausted" || !reportDeliveryPending(&s) || candidates[0].digest() == frozen {
						t.Fatalf("changed source accepted stale translation: %+v", s.Run)
					}
					if change == "manual_chain" && s.Result.Status != "success" || change == "evidence" && s.Result.Status == "success" {
						t.Fatalf("source and business state conflated: %s/%s", change, s.Result.Status)
					}
				}
				// Older incomplete documents remain immutable for the same source.
				found := false
				receipts := ReportReceipts(s)
				for i := len(receipts) - 1; i >= 0; i-- {
					if report := receipts[i]; report.SourceStatus == "current" {
						found = true
						if (report.TranslationStatus == "complete") != (change == "unchanged") {
							t.Fatalf("wrong latest current report translation: %+v", report)
						}
						break
					}
				}
				if !found {
					t.Fatal("current report missing")
				}
			})
		}
	}
}

func TestU8ReportDigestRetainsManualChainText(t *testing.T) {
	chain := EvidenceChain{Name: "Original manual name", Summary: "Original manual summary"}
	candidate := reportCandidate{source: reportSource{Kind: "final", Chains: []EvidenceChain{chain}}}
	original := candidate.digest()
	for _, field := range []string{"name", "summary"} {
		changed := candidate
		changed.source.Chains = []EvidenceChain{chain}
		if field == "name" {
			changed.source.Chains[0].Name += " changed"
		} else {
			changed.source.Chains[0].Summary += " changed"
		}
		if changed.digest() == original {
			t.Fatalf("manual chain %s omitted from source digest", field)
		}
	}
}

func TestU8AutomaticChainEmptyScopeUsesGoal(t *testing.T) {
	s := Snapshot{Run: Run{ID: "empty-scope", Goal: "Compare the supplied document", Status: StatusCompleted}}
	u8Source(&s, "document")
	s.Evidence[0].Scope = &EvidenceScope{}
	s.Events[0].Data.(map[string]string)["evidence_scope_hash"] = valueHash(s.Evidence[0].Scope)
	s.Result = Result{Status: "incomplete", Summary: "Observed the document contents", EvidenceIDs: []string{"document"}}
	archiveConclusionChains(&s)
	if len(s.EvidenceChains) != 1 || !strings.HasPrefix(s.EvidenceChains[0].Name, s.Run.Goal+" · ") || strings.Contains(s.EvidenceChains[0].Name, " @ ") {
		t.Fatalf("empty scope replaced the actual object: %+v", s.EvidenceChains)
	}
}
