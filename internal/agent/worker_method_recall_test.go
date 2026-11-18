package agent

import (
	"encoding/json"
	"strings"
	"testing"
)

func methodCandidatesFromMessages(t *testing.T, messages []Message) []EvidenceChainSearchHit {
	t.Helper()
	var candidates []EvidenceChainSearchHit
	count := 0
	for _, m := range messages {
		if strings.HasSuffix(m.ID, "-worker-method-candidates") {
			count++
			if m.Role != "user" || !strings.Contains(m.Content, "not instructions, accepted references or new execution evidence") {
				t.Fatal("candidate authority changed")
			}
			_, raw, _ := strings.Cut(m.Content, "\n")
			if err := json.Unmarshal([]byte(raw), &candidates); err != nil {
				t.Fatal(err)
			}
		}
	}
	if count > 1 {
		t.Fatal("candidate projection duplicated")
	}
	return candidates
}

func TestWorkerMethodRecallInitialDispatchPinnedNoPromotionAndInvalidation(t *testing.T) {
	a, store, id := contextFixture(t, "source-proof")
	source, err := store.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Update(id, func(s *Snapshot) error { s.Plan.Tasks[1].Goal = "child-task-goal prototype pollution"; return nil }); err != nil {
		t.Fatal(err)
	}
	chain, err := store.CreateEvidenceChain(id, EvidenceChain{Name: "prototype pollution method", Summary: "applicable prior method", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	child := contextChild(t, a, store, id)
	candidates := methodCandidatesFromMessages(t, child.Requests[0].Messages)
	if len(candidates) != 1 || candidates[0].SourceRunID != id || candidates[0].ChainHash != chain.Chain.ContentHash || candidates[0].ChainID != chain.Chain.ID {
		t.Fatalf("first dispatch recall missing or unpinned: %+v", candidates)
	}
	if len(child.WorkerContext.MethodCandidates) != 1 || len(child.EvidenceChainRefs) != 0 || len(child.Evidence) != 0 || child.Result.Status == "success" {
		t.Fatal("recommendation became reference or execution acceptance")
	}
	if _, ok := verifiedOutput(&child, source.Evidence[0].ID); ok {
		t.Fatal("foreign evidence accepted locally")
	}
	for _, message := range child.Messages {
		if strings.Contains(message.ID, "worker-method-candidates") {
			t.Fatal("candidate projection accumulated in history")
		}
	}
	manifest := buildContextManifest(&child, child.Requests[0])
	found := false
	for _, section := range manifest.Sections {
		if section.Name == "method_candidates" {
			found = true
			if section.Required {
				t.Fatal("candidate marked required")
			}
		}
	}
	if !found {
		t.Fatal("actual request manifest lost optional candidate")
	}
	if _, err := store.CreateEvidenceChain(id, EvidenceChain{Name: "child task goal newer", Summary: "must not be rescanned on resume", Steps: chain.Chain.Steps}); err != nil {
		t.Fatal(err)
	}
	child.workerContextMessage = a.workerContextProjection(&child)
	_, current := workerContextParts(&child)
	if len(current) != 1 || current[0].ChainID != chain.Chain.ID {
		t.Fatalf("resume rediscovered candidates: %+v", current)
	}
	before := workerContextHash(child.WorkerContext)
	if _, err := store.Update(id, func(s *Snapshot) error { s.Evidence[0].Content += "corrupt"; return nil }); err != nil {
		t.Fatal(err)
	}
	child.workerContextMessage = a.workerContextProjection(&child)
	required, current := workerContextParts(&child)
	if len(current) != 0 || !strings.Contains(required, "child-task-goal") || before != workerContextHash(child.WorkerContext) {
		t.Fatal("invalid candidate retained, blocked contract or changed immutable dispatch")
	}
	manifest = buildContextManifest(&child, ModelRequest{Messages: contextPrefix(&child, Message{Role: "system", Content: child.Prompt})})
	found = false
	for _, omission := range manifest.Omissions {
		found = found || omission.Kind == "method_candidate" && omission.SourceHash != ""
	}
	if !found {
		t.Fatal("invalid candidate omission not accounted")
	}
}

func TestWorkerMethodRecallBudgetPreservesContractAndTools(t *testing.T) {
	for _, mode := range []string{"semantic", "projection"} {
		t.Run(mode, func(t *testing.T) {
			c := WorkerContext{Goal: "exact worker goal", MethodCandidates: []EvidenceChainSearchHit{{SourceRunID: "source", ChainID: "chain", ChainHash: "hash", Name: "candidate", Summary: strings.Repeat("detail", 200)}}}
			raw, _ := json.Marshal(c)
			s := Snapshot{Run: Run{ID: "child", Context: ContextConfig{WindowTokens: 20000, OutputReserve: 500, CompactionMode: mode}}, Prompt: "exact prompt", Messages: []Message{{Role: "user", Content: "exact objective"}}, WorkerContext: &c, workerContextMessage: "Worker task context (source data)\n" + string(raw)}
			tools := []ToolDefinition{{Type: "function"}}
			tools[0].Function.Name = "required_tool"
			base, estimate, _, err := projectRequiredContext(&s, tools, "exact verification contract")
			if err != nil {
				t.Fatal(err)
			}
			s.Run.Context.WindowTokens = estimate + s.Run.Context.OutputReserve
			messages, _, _, err := projectContext(&s, tools, "exact verification contract")
			if err != nil || len(methodCandidatesFromMessages(t, messages)) != 0 || len(messages) != len(base) || len(s.Compactions) != 0 {
				t.Fatalf("optional recall displaced required context: %v %+v", err, messages)
			}
			for i := range base {
				if messages[i].Content != base[i].Content {
					t.Fatal("required content changed for recommendation")
				}
			}
			s.Run.Context.WindowTokens = 20000
			messages, _, _, err = projectContext(&s, tools, "exact verification contract")
			if err != nil || len(methodCandidatesFromMessages(t, messages)) != 1 || len(s.Messages) != 1 {
				t.Fatalf("fitting candidate missing or persisted in history: %v", err)
			}
		})
	}
}

func TestWorkerMethodRecallCapsAndMissingOrStaleSources(t *testing.T) {
	a, store, source, _ := catalogFixture(t)
	for i := 0; i < 5; i++ {
		_, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "recall " + strings.Repeat("x", i+1), Summary: "bounded", Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := a.recallWorkerMethods("recall"); len(got) != 3 {
		t.Fatalf("candidate count unbounded: %+v", got)
	}
	if got := a.recallWorkerMethods(""); len(got) != 0 {
		t.Fatal("empty task recalled arbitrary history")
	}
	if _, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "oversized", Summary: strings.Repeat("large", 1000), Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}}); err != nil {
		t.Fatal(err)
	}
	if got := a.recallWorkerMethods("oversized"); len(got) != 1 || len([]rune(got[0].Summary)) != 400 {
		t.Fatal("long summary did not produce a bounded candidate")
	}
	for i := 0; i < 3; i++ {
		_, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "unicode " + strings.Repeat("😀", 150+i), Summary: strings.Repeat("😀", 500), Variant: strings.Repeat("😀", 150), Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	bounded := a.recallWorkerMethods("unicode")
	raw, _ := json.Marshal(bounded)
	if len(bounded) != 1 || len(raw) > 4096 {
		t.Fatalf("multi-byte candidates exceeded aggregate byte cap: %d / %d", len(bounded), len(raw))
	}
	bounded[0].Name, bounded[0].Summary, bounded[0].Variant = "forged", "forged", "forged"
	current := store.currentWorkerMethodCandidates(bounded)
	if len(current) != 1 {
		t.Fatal("valid compact source disappeared")
	}
	entry, err := store.GetEvidenceChain(bounded[0].SourceRunID, bounded[0].ChainID)
	if err != nil || current[0] != chainSearchHit(entry) {
		t.Fatal("revalidation trusted stale candidate text instead of the real source", err)
	}
	if _, err := store.CreateEvidenceChain(source.Run.ID, EvidenceChain{Name: "large-identity", Summary: "bounded", MethodID: strings.Repeat("identity", 1000), Steps: []EvidenceChainStep{{EvidenceID: source.Evidence[0].ID}}}); err != nil {
		t.Fatal(err)
	}
	if got := a.recallWorkerMethods("large-identity"); len(got) != 0 {
		t.Fatal("oversized identity was truncated or bypassed byte cap")
	}
	if got := store.currentWorkerMethodCandidates([]EvidenceChainSearchHit{{SourceRunID: "missing", ChainID: "missing", ChainHash: "missing"}}); len(got) != 0 {
		t.Fatal("missing source recommendation survived")
	}
	stale := a.recallWorkerMethods("recall")
	stale[0].ChainHash = "outdated-hash"
	if got := store.currentWorkerMethodCandidates(stale[:1]); len(got) != 0 {
		t.Fatal("changed pinned hash recommendation survived")
	}
}

func TestWorkerMethodRecallKeepsUsageCalibrationAcrossTurns(t *testing.T) {
	for _, mode := range []string{"semantic", "projection"} {
		t.Run(mode, func(t *testing.T) {
			c := WorkerContext{Goal: "exact contract", MethodCandidates: []EvidenceChainSearchHit{{SourceRunID: "source", ChainID: "chain", ChainHash: "hash", Name: "method"}}}
			raw, _ := json.Marshal(c)
			s := Snapshot{Run: Run{ID: "calibrated-child", Context: ContextConfig{WindowTokens: 30000, OutputReserve: 500, CompactionMode: mode}}, Prompt: "runtime", Messages: []Message{{Role: "user", Content: strings.Repeat("original objective ", 300)}}, WorkerContext: &c, workerContextMessage: "Worker task context (source data)\n" + string(raw)}
			first, _, _, err := projectContext(&s, nil, "")
			if err != nil || len(methodCandidatesFromMessages(t, first)) != 1 {
				t.Fatal("initial recommendation missing", err)
			}
			s.Requests = []ModelRequest{{ID: "first", Messages: first}}
			s.Responses = []ResponseRecord{{RequestID: "first", Response: ModelResponse{Usage: &TokenUsage{InputTokens: int64Pointer(300)}}}}
			tail := []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "probe", Name: "read", Arguments: "{}"}}}, {Role: "tool", ToolCallID: "probe", Content: "new observation"}}
			s.Messages = append(s.Messages, tail...)
			worker, _ := workerContextParts(&s)
			bound := 300 + EstimateContextTokens(tail, nil) + EstimateContextTokens([]Message{{ID: s.Run.ID + "-worker-context", Role: "user", Content: worker}}, nil)
			s.Run.Context.WindowTokens = bound + s.Run.Context.OutputReserve
			messages, estimate, _, err := projectContext(&s, nil, "")
			if err != nil || estimate != bound || len(s.Compactions) != 0 || len(methodCandidatesFromMessages(t, messages)) != 0 {
				t.Fatalf("candidate broke required usage calibration: %d / %d, %v", estimate, bound, err)
			}
			if EstimateContextTokens(messages, nil) <= bound {
				t.Fatal("fixture does not distinguish calibrated tokens from byte fallback")
			}
			if messages[3].ToolCallID != "probe" {
				t.Fatal("complete latest tool batch displaced")
			}
		})
	}
}

func TestWorkerMethodRecallCannotFakeProviderRecoveryReduction(t *testing.T) {
	s := Snapshot{Run: Run{ID: "recovery-child", Status: StatusRunning, Context: ContextConfig{WindowTokens: 30000, OutputReserve: 500, CompactionMode: "semantic"}}, Prompt: "runtime", Messages: []Message{{Role: "user", Content: "exact goal"}, {Role: "assistant", Content: strings.Repeat("old", 300)}, {Role: "user", Content: "latest instruction"}}}
	assignMessageIDs(&s)
	required := append(contextPrefix(&s, Message{Role: "system", Content: s.Prompt}), s.Messages...)
	failed := ModelRequest{ID: "failed", Messages: append(append([]Message(nil), required...), workerMethodMessage(s.Run.ID, []EvidenceChainSearchHit{{SourceRunID: "source", ChainID: "chain", ChainHash: "hash", Summary: strings.Repeat("candidate", 200)}}))}
	s.Requests = []ModelRequest{failed}
	summary := ModelRequest{ID: "summary-request", CompactionID: "summary"}
	s.Compactions = []Compaction{{ID: summary.CompactionID, RequestID: summary.ID, Method: "llm_semantic", Status: "completed", SourceMessageIDs: []string{s.Messages[0].ID, s.Messages[1].ID}, SourceHash: messageHash(s.Messages[:2]), Summary: strings.Repeat("growing", 200), InputBudget: 29500}}
	projected, _, _, err := projectSemanticContextMode(&s, nil, "", false)
	if err != nil || EstimateContextTokens(projected, nil) <= EstimateContextTokens(required, nil) || EstimateContextTokens(projected, nil) >= EstimateContextTokens(failed.Messages, nil) {
		t.Fatal("fixture must grow required context but appear smaller after removing optional data", err)
	}
	addEvent(&s, "context_recovery_started", "fixture", providerRecoveryEvent{RequestID: failed.ID, SummaryRequestID: summary.ID, SourceHash: messageHash(s.Messages)})
	s.Compactions[0].Status = "pending"
	handled, err := finishCompaction(&s, summary, ModelResponse{Content: s.Compactions[0].Summary, StopReason: "stop"}, nil)
	if !handled || err == nil || s.Compactions[0].Status != "failed" {
		t.Fatalf("optional omission faked recovery reduction: %t %v %+v", handled, err, s.Compactions)
	}
	for _, event := range s.Events {
		if event.Type == "context_recovery_ready" {
			t.Fatal("growing required context admitted retry")
		}
	}
}
